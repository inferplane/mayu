package responses

import (
	"encoding/json"
	"testing"

	"github.com/inferplane/inferplane/internal/openai"
	"github.com/inferplane/inferplane/pkg/schema"
)

type renderedItem struct {
	Type      string          `json:"type"`
	Phase     string          `json:"phase"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Input     string          `json:"input"`
	Arguments string          `json:"arguments"`
	Content   json.RawMessage `json:"content"`
}

func decodeItems(t *testing.T, raw []byte) []renderedItem {
	t.Helper()
	var response struct {
		Output []renderedItem `json:"output"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	return response.Output
}

func TestResponseInfersCommentaryOnlyWhenToolsArePresent(t *testing.T) {
	for _, tc := range []struct {
		name, explicit, want string
		tool                 bool
	}{
		{"tool commentary", "", "commentary", true},
		{"plain final answer", "", "final_answer", false},
		{"explicit final survives tool", "final_answer", "final_answer", true},
		{"explicit commentary survives final", "commentary", "commentary", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := schema.ContentBlock{Type: "text", Text: ptr("I will inspect the file.")}
			if tc.explicit != "" {
				block.Extra = map[string]json.RawMessage{"phase": marshal(tc.explicit)}
			}
			resp := &schema.ChatResponse{
				ID: "response_a", Model: "model",
				Content: []schema.ContentBlock{block},
				Usage:   &schema.Usage{InputTokens: ptr(int64(2)), OutputTokens: ptr(int64(4))},
			}
			if tc.tool {
				resp.Content = append(resp.Content, schema.ContentBlock{Type: "tool_use", Name: "read", ID: "read_a", Input: json.RawMessage(`{"path":"main.go"}`)})
			}
			raw, err := CanonicalToResponse(resp)
			if err != nil {
				t.Fatal(err)
			}
			items := decodeItems(t, raw)
			if items[0].Phase != tc.want {
				t.Fatalf("message phase = %q, want %q; response=%s", items[0].Phase, tc.want, raw)
			}
			if tc.explicit == "" && resp.Content[0].Extra != nil {
				t.Fatal("phase inference mutated caller's canonical content")
			}
		})
	}
}

func TestFunctionAndCustomToolTwoTurnPhaseRoundTrip(t *testing.T) {
	for _, custom := range []bool{false, true} {
		name := "read"
		tool := map[string]any{"type": "function", "name": name, "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]string{"type": "string"}}}}
		arguments := json.RawMessage(`{"path":"main.go"}`)
		outputType, resultType := "function_call", "function_call_output"
		if custom {
			name = "apply_patch"
			tool = map[string]any{"type": "custom", "name": name}
			arguments = marshal(map[string]string{"input": "*** Begin Patch\n*** End Patch"})
			outputType, resultType = "custom_tool_call", "custom_tool_call_output"
		}
		t.Run(name, func(t *testing.T) {
			user := map[string]any{"role": "user", "content": "Update main.go"}
			first := marshal(map[string]any{"model": "auto", "store": false, "input": []any{user}, "tools": []any{tool}})
			if err := ValidateConversion(first); err != nil {
				t.Fatal(err)
			}
			req, err := RequestToCanonical(first)
			if err != nil {
				t.Fatal(err)
			}
			resp := &schema.ChatResponse{
				ID: "response_tool", Model: "model",
				Content: []schema.ContentBlock{
					{Type: "text", Text: ptr("I will use the tool.")},
					{Type: "tool_use", ID: "call_stable", Name: name, Input: arguments},
				},
				Usage: &schema.Usage{InputTokens: ptr(int64(3)), OutputTokens: ptr(int64(8))},
			}
			raw, err := CanonicalToResponseForRequest(resp, req)
			if err != nil {
				t.Fatal(err)
			}
			items := decodeItems(t, raw)
			if len(items) != 2 || items[0].Phase != "commentary" || items[1].Type != outputType || items[1].CallID != "call_stable" || items[1].Name != name {
				t.Fatalf("client cannot continue tool turn: %s", raw)
			}
			if custom && items[1].Input != "*** Begin Patch\n*** End Patch" {
				t.Fatalf("custom tool input changed: %s", raw)
			}
			var firstResponse struct {
				Output []json.RawMessage `json:"output"`
			}
			if err := json.Unmarshal(raw, &firstResponse); err != nil {
				t.Fatal(err)
			}
			history := []any{user}
			for _, item := range firstResponse.Output {
				history = append(history, item)
			}
			history = append(history, map[string]any{"type": resultType, "call_id": "call_stable", "output": "Tool finished"})
			second := marshal(map[string]any{"model": "auto", "store": false, "input": history, "tools": []any{tool}})
			if err := ValidateConversion(second); err != nil {
				t.Fatalf("renderer output is not replayable: %v", err)
			}
			next, err := RequestToCanonical(second)
			if err != nil {
				t.Fatal(err)
			}
			if len(next.Messages) != 4 || string(next.Messages[1].Extra["phase"]) != `"commentary"` {
				t.Fatalf("assistant commentary lost from replay: %+v", next.Messages)
			}
			call, result := next.Messages[2].Content[0], next.Messages[3].Content[0]
			if call.ID != "call_stable" || result.ToolUseID != call.ID || string(call.Input) != string(arguments) {
				t.Fatalf("tool pairing/arguments lost: %+v %+v", call, result)
			}
			// Exercise the real downstream Chat Completions conversion. It
			// does not currently carry Message.Extra.phase; continuity must
			// therefore survive through assistant/tool ordering and call IDs.
			var bridged struct {
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
			if err := json.Unmarshal(openai.CanonicalToRequest(next), &bridged); err != nil {
				t.Fatal(err)
			}
			if len(bridged.Messages) != 4 || bridged.Messages[1].Role != "assistant" || bridged.Messages[1].Content != "I will use the tool." ||
				bridged.Messages[2].Role != "assistant" || len(bridged.Messages[2].ToolCalls) != 1 ||
				bridged.Messages[3].Role != "tool" || bridged.Messages[3].ToolCallID != "call_stable" || bridged.Messages[3].Content != "Tool finished" {
				t.Fatalf("bridge broke tool-turn ordering: %+v", bridged.Messages)
			}
			bridgedCall := bridged.Messages[2].ToolCalls[0]
			if bridgedCall.ID != "call_stable" || bridgedCall.Function.Name != name || bridgedCall.Function.Arguments != string(arguments) {
				t.Fatalf("bridge lost tool arguments: %+v", bridgedCall)
			}
			resp.Content = []schema.ContentBlock{{Type: "text", Text: ptr("The change is complete.")}}
			final, err := CanonicalToResponseForRequest(resp, next)
			if err != nil {
				t.Fatal(err)
			}
			if items := decodeItems(t, final); len(items) != 1 || items[0].Phase != "final_answer" {
				t.Fatalf("post-tool final answer mislabeled: %s", final)
			}
		})
	}
}

func TestStreamTextBeforeToolNeverAnnouncesInferredFinalAnswer(t *testing.T) {
	for _, explicit := range []string{"", "commentary", "final_answer"} {
		t.Run("phase="+explicit, func(t *testing.T) {
			s := NewStreamState("model")
			var events []Event
			emit := func(chunk *schema.ChatChunk) {
				t.Helper()
				got, err := s.Convert(chunk)
				if err != nil {
					t.Fatal(err)
				}
				events = append(events, got...)
			}
			emit(&schema.ChatChunk{Type: "message_start", Message: &schema.ChatResponse{Usage: &schema.Usage{InputTokens: ptr(int64(2))}}})
			idx := 0
			text := schema.ContentBlock{Type: "text", Text: ptr("")}
			if explicit != "" {
				text.Extra = map[string]json.RawMessage{"phase": marshal(explicit)}
			}
			emit(&schema.ChatChunk{Type: "content_block_start", Index: &idx, ContentBlock: &text})
			emit(&schema.ChatChunk{Type: "content_block_delta", Index: &idx, Delta: marshal(map[string]string{"type": "text_delta", "text": "I will inspect it."})})
			emit(&schema.ChatChunk{Type: "content_block_stop", Index: &idx})
			idx = 1
			emit(&schema.ChatChunk{Type: "content_block_start", Index: &idx, ContentBlock: &schema.ContentBlock{Type: "tool_use", ID: "call_a", Name: "read"}})
			emit(&schema.ChatChunk{Type: "content_block_delta", Index: &idx, Delta: marshal(map[string]string{"type": "input_json_delta", "partial_json": "{}"})})
			emit(&schema.ChatChunk{Type: "content_block_stop", Index: &idx})
			emit(&schema.ChatChunk{Type: "message_delta", Usage: &schema.Usage{OutputTokens: ptr(int64(3))}, Delta: json.RawMessage(`{"stop_reason":"tool_use"}`)})
			emit(&schema.ChatChunk{Type: "message_stop"})
			want := explicit
			if want == "" {
				want = "commentary"
			}
			textDone, callDone := 0, 0
			for _, event := range events {
				var payload struct {
					Item     renderedItem    `json:"item"`
					Response json.RawMessage `json:"response"`
				}
				if err := json.Unmarshal(event.Data, &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Item.Type == "message" && payload.Item.Phase != want {
					t.Fatalf("premature/wrong phase in %s: %s", event.Type, event.Data)
				}
				if event.Type == "response.output_item.done" {
					if payload.Item.Type == "message" {
						textDone++
					} else if payload.Item.CallID == "call_a" {
						callDone++
					}
				}
				if event.Type == "response.completed" && decodeItems(t, payload.Response)[0].Phase != want {
					t.Fatalf("terminal phase disagrees: %s", event.Data)
				}
			}
			if textDone != 1 || callDone != 1 {
				t.Fatalf("completed items: text=%d call=%d", textDone, callDone)
			}
		})
	}
}

func TestStreamTextOnlyFinalizesAsFinalAnswer(t *testing.T) {
	s := NewStreamState("model")
	idx := 0
	var events []Event
	for _, chunk := range []*schema.ChatChunk{
		{Type: "message_start", Message: &schema.ChatResponse{Usage: &schema.Usage{InputTokens: ptr(int64(2))}}},
		{Type: "content_block_start", Index: &idx, ContentBlock: &schema.ContentBlock{Type: "text", Text: ptr("Answer")}},
		{Type: "content_block_stop", Index: &idx},
		{Type: "message_delta", Usage: &schema.Usage{OutputTokens: ptr(int64(2))}, Delta: json.RawMessage(`{"stop_reason":"end_turn"}`)},
		{Type: "message_stop"},
	} {
		got, err := s.Convert(chunk)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, got...)
	}
	done := 0
	for _, event := range events {
		if event.Type == "response.output_item.done" {
			var payload struct {
				Item renderedItem `json:"item"`
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Item.Phase != "final_answer" {
				t.Fatalf("text final answer mislabeled: %s", event.Data)
			}
			done++
		}
	}
	if done != 1 {
		t.Fatalf("text item completed %d times", done)
	}
}
