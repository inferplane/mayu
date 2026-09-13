package responses

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/inferplane/inferplane/internal/openai"
	"github.com/inferplane/inferplane/pkg/schema"
)

func TestParallelToolReplayReachesChatAsOneCallBatch(t *testing.T) {
	raw := []byte(`{"model":"m","tools":[
		{"type":"function","name":"read","strict":false,"parameters":{"type":"object"}},
		{"type":"namespace","name":"local","tools":[
			{"type":"function","name":"read","strict":false,"parameters":{"type":"object"}},
			{"type":"custom","name":"apply_patch"}]}],
		"input":[
			{"role":"user","content":"Fix both files"},
			{"role":"assistant","phase":"commentary","content":"Checking files"},
			{"type":"function_call","name":"read","call_id":"c1","arguments":"{\"path\":\"one.go\"}"},
			{"role":"assistant","phase":"commentary","content":"Checking the second file"},
			{"type":"function_call","namespace":"local","name":"read","call_id":"c2","arguments":"{\"path\":\"two.go\"}"},
			{"type":"custom_tool_call","namespace":"local","name":"apply_patch","call_id":"c3","input":"patch\ntext"},
			{"type":"function_call_output","call_id":"c2","output":"two"},
			{"type":"function_call_output","call_id":"c1","output":"one"},
			{"type":"custom_tool_call_output","call_id":"c3","output":"patched"},
			{"role":"assistant","phase":"final_answer","content":"Done"},
			{"role":"user","content":"Read again"},
			{"type":"function_call","name":"read","call_id":"c4","arguments":"{}"},
			{"type":"function_call_output","call_id":"c4","output":"again"}]}`)
	before := bytes.Clone(raw)
	if err := ValidateConversion(raw); err != nil {
		t.Fatal(err)
	}
	req, err := RequestToCanonical(raw)
	if err != nil {
		t.Fatal(err)
	}
	wire := openai.CanonicalToRequest(req)
	var chat struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(wire, &chat); err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{}
	var batches [][]string
	for _, message := range chat.Messages {
		if len(pending) > 0 && message.Role != "tool" {
			t.Fatalf("assistant turn interrupts unresolved parallel calls: %s", wire)
		}
		if len(message.ToolCalls) > 0 {
			var ids []string
			for _, call := range message.ToolCalls {
				pending[call.ID] = true
				ids = append(ids, call.ID)
				want := map[string]string{"c1": `{"path":"one.go"}`, "c2": `{"path":"two.go"}`, "c3": `{"input":"patch\ntext"}`, "c4": `{}`}
				if call.Function.Arguments != want[call.ID] {
					t.Fatalf("tool arguments changed: %s", wire)
				}
			}
			batches = append(batches, ids)
		}
		if message.Role == "tool" {
			if !pending[message.ToolCallID] {
				t.Fatalf("orphaned tool result: %s", wire)
			}
			delete(pending, message.ToolCallID)
		}
	}
	if got := string(marshal(batches)); got != `[["c1","c2","c3"],["c4"]]` || len(pending) != 0 {
		t.Fatalf("call batches crossed a result/turn boundary: %s; sink=%s", got, wire)
	}
	var calls []schema.ContentBlock
	phases := map[string]string{}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_use" && block.ID != "c4" {
				calls = append(calls, block)
			}
			if block.Text != nil {
				phase := optionalText(block.Extra["phase"])
				if phase == "" {
					phase = optionalText(message.Extra["phase"])
				}
				phases[*block.Text] = phase
			}
		}
	}
	if phases["Checking files"] != "commentary" || phases["Checking the second file"] != "commentary" || phases["Done"] != "final_answer" {
		t.Fatalf("phase metadata was lost during coalescing: %v", phases)
	}
	restored, err := CanonicalToResponseForRequest(&schema.ChatResponse{
		Content: calls, Usage: &schema.Usage{InputTokens: ptr(int64(1)), OutputTokens: ptr(int64(2))},
	}, req)
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Items []struct {
			Type, Name, Namespace string
			CallID                string `json:"call_id"`
			Input                 string
		} `json:"output"`
	}
	if err := json.Unmarshal(restored, &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Items) != 3 || output.Items[0].Name != "read" || output.Items[0].Namespace != "" ||
		output.Items[1].Name != "read" || output.Items[1].Namespace != "local" ||
		output.Items[2].Name != "apply_patch" || output.Items[2].Namespace != "local" ||
		output.Items[2].Type != "custom_tool_call" || output.Items[2].Input != "patch\ntext" {
		t.Fatalf("coalescing broke tool origins: %s", restored)
	}
	if !bytes.Equal(raw, before) {
		t.Fatal("observation mutated native request bytes")
	}
}
