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

func TestChatResponseSkeletonConstruction(t *testing.T) {
	// M2가 message_start 골격을 코드로 만들 때의 계약: nil StopReason/
	// StopSequence는 명시적 null로, 빈 Content는 []로 방출된다.
	in := int64(4805)
	out := int64(2)
	r := ChatResponse{
		ID: "msg_01S", Type: "message", Role: "assistant",
		Model:   "claude-sonnet-4-6",
		Content: []ContentBlock{},
		Usage:   &Usage{InputTokens: &in, OutputTokens: &out},
	}
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"msg_01S","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":4805,"output_tokens":2}}`
	assertJSONSemanticEqual(t, []byte(want), got)
}

func TestUsageExtraRoundTrip(t *testing.T) {
	// 정산 입력에서 미지의 과금 필드는 절대 드랍되면 안 된다.
	for _, in := range []string{
		`{"input_tokens":1,"output_tokens":2,"server_tool_use":{"web_search_requests":3}}`,
		`{"output_tokens":2,"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_30m_input_tokens":7}}`,
	} {
		var u Usage
		if err := json.Unmarshal([]byte(in), &u); err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(u)
		if err != nil {
			t.Fatal(err)
		}
		assertJSONSemanticEqual(t, []byte(in), got)
	}
}
