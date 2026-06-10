package openai

import (
	"encoding/json"
	"testing"

	"github.com/inferplane/inferplane/pkg/schema"
)

func TestRequestToCanonicalBasics(t *testing.T) {
	in := []byte(`{"model":"gpt-x","max_tokens":256,"temperature":0.7,"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`)
	cr, err := RequestToCanonical(in)
	if err != nil {
		t.Fatal(err)
	}
	if cr.Model != "gpt-x" || cr.MaxTokens == nil || *cr.MaxTokens != 256 {
		t.Fatalf("req: %+v", cr)
	}
	// system extracted to top-level system; user message present
	if len(cr.System) == 0 {
		t.Fatal("system not mapped")
	}
	if len(cr.Messages) != 1 || cr.Messages[0].Role != "user" {
		t.Fatalf("messages: %+v", cr.Messages)
	}
}

func TestResponseFromCanonical(t *testing.T) {
	txt := "answer"
	stop := "end_turn"
	in, out := int64(10), int64(3)
	resp := &schema.ChatResponse{ID: "msg_1", Model: "m", Role: "assistant",
		Content: []schema.ContentBlock{{Type: "text", Text: &txt}}, StopReason: &stop,
		Usage: &schema.Usage{InputTokens: &in, OutputTokens: &out}}
	oai := ResponseFromCanonical(resp)
	var m map[string]any
	json.Unmarshal(oai, &m)
	if m["object"] != "chat.completion" {
		t.Fatalf("object: %v", m["object"])
	}
	choices := m["choices"].([]any)
	c0 := choices[0].(map[string]any)
	if c0["finish_reason"] != "stop" {
		t.Fatalf("finish_reason: %v", c0["finish_reason"])
	}
	msg := c0["message"].(map[string]any)
	if msg["content"] != "answer" {
		t.Fatalf("content: %v", msg["content"])
	}
	usage := m["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 10 || usage["completion_tokens"].(float64) != 3 {
		t.Fatalf("usage: %v", usage)
	}
}

func TestToolCallRoundTrip(t *testing.T) {
	// OpenAI assistant tool_call → canonical tool_use → back to OpenAI
	in := []byte(`{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`)
	cr, err := RequestToCanonical(in)
	if err != nil {
		t.Fatal(err)
	}
	// assistant message → tool_use block; tool message → tool_result block
	foundToolUse, foundToolResult := false, false
	for _, msg := range cr.Messages {
		for _, b := range msg.Content {
			if b.Type == "tool_use" && b.Name == "bash" && b.ID == "call_1" {
				foundToolUse = true
			}
			if b.Type == "tool_result" && b.ToolUseID == "call_1" {
				foundToolResult = true
			}
		}
	}
	if !foundToolUse || !foundToolResult {
		t.Fatalf("tool mapping: use=%v result=%v\n%+v", foundToolUse, foundToolResult, cr.Messages)
	}
}

func TestChunkFromCanonicalTextDelta(t *testing.T) {
	idx := 0
	delta := []byte(`{"type":"text_delta","text":"hi"}`)
	c := &schema.ChatChunk{Type: "content_block_delta", Index: &idx, Delta: delta}
	st := &StreamState{}
	out := ChunkFromCanonical(c, st)
	if out == nil {
		t.Fatal("text_delta should produce an OpenAI chunk")
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["object"] != "chat.completion.chunk" {
		t.Fatalf("object: %v", m["object"])
	}
	ch := m["choices"].([]any)[0].(map[string]any)
	d := ch["delta"].(map[string]any)
	if d["content"] != "hi" {
		t.Fatalf("delta content: %v", d)
	}
}

func TestChunkFromCanonicalMessageStopFinish(t *testing.T) {
	stop := "end_turn"
	delta := []byte(`{"stop_reason":"end_turn","stop_sequence":null}`)
	_ = stop
	c := &schema.ChatChunk{Type: "message_delta", Delta: delta}
	st := &StreamState{}
	out := ChunkFromCanonical(c, st)
	if out == nil {
		t.Fatal("message_delta should produce a finish chunk")
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	ch := m["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "stop" {
		t.Fatalf("finish_reason: %v", ch["finish_reason"])
	}
}
