package schema

import (
	"encoding/json"
	"testing"
)

func TestChatRequestRoundTrip(t *testing.T) {
	// cache_control 멀티 breakpoint: system 블록 + 마지막 user 메시지.
	// tools/system/thinking은 M1에서 raw 보존 (M5에서 타입 승격).
	in := `{
	  "model": "claude-sonnet-4-6",
	  "max_tokens": 8192,
	  "stream": true,
	  "system": [
	    {"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}},
	    {"type":"text","text":"Project context...","cache_control":{"type":"ephemeral","ttl":"1h"}}
	  ],
	  "tools": [{"name":"bash","description":"run","input_schema":{"type":"object"}}],
	  "thinking": {"type":"enabled","budget_tokens":4096},
	  "messages": [
	    {"role":"user","content":[{"type":"text","text":"refactor this","cache_control":{"type":"ephemeral"}}]}
	  ],
	  "metadata": {"user_id":"team-platform"}
	}`
	var r ChatRequest
	if err := json.Unmarshal([]byte(in), &r); err != nil {
		t.Fatal(err)
	}
	if r.Model != "claude-sonnet-4-6" || !r.Stream || len(r.Messages) != 1 {
		t.Fatalf("typed fields: %+v", r)
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONSemanticEqual(t, []byte(in), out)
}
