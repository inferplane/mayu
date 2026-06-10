package schema

import (
	"encoding/json"
	"testing"
)

func TestChatResponseRoundTrip(t *testing.T) {
	in := `{
	  "id": "msg_01ABC",
	  "type": "message",
	  "role": "assistant",
	  "model": "claude-sonnet-4-6",
	  "content": [
	    {"type":"thinking","thinking":"reasoning...","signature":"EuYB"},
	    {"type":"text","text":"done"}
	  ],
	  "stop_reason": "end_turn",
	  "stop_sequence": null,
	  "usage": {
	    "input_tokens": 1200,
	    "output_tokens": 850,
	    "cache_read_input_tokens": 45000,
	    "cache_creation_input_tokens": 1024,
	    "cache_creation": {"ephemeral_5m_input_tokens":1024,"ephemeral_1h_input_tokens":0}
	  }
	}`
	var r ChatResponse
	if err := json.Unmarshal([]byte(in), &r); err != nil {
		t.Fatal(err)
	}
	if r.Usage == nil || r.Usage.CacheReadInputTokens == nil || *r.Usage.CacheReadInputTokens != 45000 {
		t.Fatalf("usage not typed: %+v", r.Usage)
	}
	if r.Usage.CacheCreation == nil || r.Usage.CacheCreation.Ephemeral5mInputTokens == nil || *r.Usage.CacheCreation.Ephemeral5mInputTokens != 1024 {
		t.Fatalf("cache_creation TTL detail not typed: %+v", r.Usage.CacheCreation)
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONSemanticEqual(t, []byte(in), out)
}

func TestUsagePinsPartialAndExplicitZero(t *testing.T) {
	// message_delta는 output_tokens만 싣기도 하고(키 추가 금지),
	// 명시적 cache_creation_input_tokens:0은 보존되어야 한다.
	for _, in := range []string{
		`{"output_tokens":850}`,
		`{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":0}`,
	} {
		var u Usage
		if err := json.Unmarshal([]byte(in), &u); err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(u)
		if err != nil {
			t.Fatal(err)
		}
		assertJSONSemanticEqual(t, []byte(in), out)
	}
}
