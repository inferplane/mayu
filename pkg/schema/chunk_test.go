package schema

import (
	"encoding/json"
	"testing"
)

func TestChatChunkRoundTrip(t *testing.T) {
	cases := map[string]string{
		"message_start":  `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1200,"output_tokens":1}}}`,
		"block_start":    `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		"thinking_delta": `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step"}}`,
		"text_delta":     `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hi"}}`,
		"input_json":     `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"comm"}}`,
		"block_stop":     `{"type":"content_block_stop","index":0}`,
		"message_delta":  `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":1200,"output_tokens":850}}`,
		"message_stop":   `{"type":"message_stop"}`,
		"ping":           `{"type":"ping"}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var c ChatChunk
			if err := json.Unmarshal([]byte(in), &c); err != nil {
				t.Fatal(err)
			}
			out, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONSemanticEqual(t, []byte(in), out)
		})
	}
}
