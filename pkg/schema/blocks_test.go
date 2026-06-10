package schema

import (
	"encoding/json"
	"testing"
)

func TestContentBlockRoundTrip(t *testing.T) {
	cases := map[string]string{
		"text_with_cache": `{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}`,
		"tool_use":        `{"type":"tool_use","id":"toolu_01","name":"bash","input":{"command":"ls -la"}}`,
		"tool_result":     `{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"ok"}],"is_error":false}`,
		"thinking":        `{"type":"thinking","thinking":"step 1...","signature":"EuYBCkQYAiJA"}`,
		"redacted":        `{"type":"redacted_thinking","data":"EmwKAhgB"}`,
		"unknown_type":    `{"type":"future_block","payload":{"deep":[1,2]}}`,
		"unknown_field":   `{"type":"text","text":"x","novel_attr":true}`,

		"empty_text_start":     `{"type":"text","text":""}`,
		"empty_thinking_start": `{"type":"thinking","thinking":"","signature":""}`,
		"empty_redacted":       `{"type":"redacted_thinking","data":""}`,
		"cache_no_ttl":         `{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}`,

		"nested_tool_result_cache": `{"type":"tool_result","tool_use_id":"t9","content":[{"type":"text","text":"r","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var b ContentBlock
			if err := json.Unmarshal([]byte(in), &b); err != nil {
				t.Fatal(err)
			}
			out, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONSemanticEqual(t, []byte(in), out)
		})
	}
}

func TestContentBlockTypedFields(t *testing.T) {
	var b ContentBlock
	_ = json.Unmarshal([]byte(`{"type":"tool_use","id":"toolu_01","name":"bash","input":{}}`), &b)
	if b.Type != "tool_use" || b.ID != "toolu_01" || b.Name != "bash" {
		t.Fatalf("typed fields not populated: %+v", b)
	}
}
