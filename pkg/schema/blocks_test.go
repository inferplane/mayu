package schema

import "testing"

func TestContentBlockRoundTrip(t *testing.T) {
	cases := map[string]string{
		"text_with_cache": `{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}`,
		"tool_use":        `{"type":"tool_use","id":"toolu_01","name":"bash","input":{"command":"ls -la"}}`,
		"tool_result":     `{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"ok"}],"is_error":false}`,
		"thinking":        `{"type":"thinking","thinking":"step 1...","signature":"EuYBCkQYAiJA"}`,
		"redacted":        `{"type":"redacted_thinking","data":"EmwKAhgB"}`,
		"unknown_type":    `{"type":"future_block","payload":{"deep":[1,2]}}`,
		"unknown_field":   `{"type":"text","text":"x","novel_attr":true}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var b ContentBlock
			if err := b.UnmarshalJSON([]byte(in)); err != nil {
				t.Fatal(err)
			}
			out, err := b.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			assertJSONSemanticEqual(t, []byte(in), out)
		})
	}
}

func TestContentBlockTypedFields(t *testing.T) {
	var b ContentBlock
	_ = b.UnmarshalJSON([]byte(`{"type":"tool_use","id":"toolu_01","name":"bash","input":{}}`))
	if b.Type != "tool_use" || b.ID != "toolu_01" || b.Name != "bash" {
		t.Fatalf("typed fields not populated: %+v", b)
	}
}
