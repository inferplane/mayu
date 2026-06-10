package schema

import (
	"encoding/json"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	cases := map[string]string{
		// Anthropic은 content에 string과 블록 배열 양형을 허용 — 원형 보존 필수
		"string_content": `{"role":"user","content":"plain text"}`,
		"block_content":  `{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"grep","input":{"q":"x"}}]}`,
		"block_order":    `{"role":"assistant","content":[{"type":"thinking","thinking":"...","signature":"sig"},{"type":"text","text":"answer"}]}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var m Message
			if err := json.Unmarshal([]byte(in), &m); err != nil {
				t.Fatal(err)
			}
			out, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONSemanticEqual(t, []byte(in), out)
		})
	}
}

func TestMessageBlockOrderPreserved(t *testing.T) {
	in := `{"role":"assistant","content":[{"type":"thinking","thinking":"a","signature":"s"},{"type":"text","text":"b"}]}`
	var m Message
	_ = json.Unmarshal([]byte(in), &m)
	if len(m.Content) != 2 || m.Content[0].Type != "thinking" || m.Content[1].Type != "text" {
		t.Fatalf("block order broken: %+v", m.Content)
	}
}
