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
		"null_content":   `{"role":"user","content":null}`,
		"empty_array":    `{"role":"user","content":[]}`,
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

func TestMessageRejectsScalarContent(t *testing.T) {
	for _, in := range []string{`{"role":"user","content":123}`, `{"role":"user","content":true}`} {
		var m Message
		if err := json.Unmarshal([]byte(in), &m); err == nil {
			t.Fatalf("expected error for %s", in)
		}
	}
}

func TestMessageRejectsCaseCollision(t *testing.T) {
	// role/content are pipeline-interpreted; a case-variant both populates
	// the typed field (Go decodes case-insensitively) and survives into Extra,
	// emitting duplicate keys — same smuggling vector f8969bb closed in
	// unmarshalWithExtra. Message hand-rolls its codec, so it must guard too.
	for _, in := range []string{
		`{"role":"user","Role":"admin","content":"x"}`,
		`{"role":"user","content":"benign","Content":[{"type":"text","text":"INJECTED"}]}`,
	} {
		var m Message
		if err := json.Unmarshal([]byte(in), &m); err == nil {
			t.Fatalf("expected case-collision rejection for %s", in)
		}
	}
}

func TestMessageExtraPreserved(t *testing.T) {
	// A genuinely-unknown top-level message field must round-trip via Extra.
	in := `{"role":"user","content":"hi","name":"alice"}`
	var m Message
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONSemanticEqual(t, []byte(in), out)
}
