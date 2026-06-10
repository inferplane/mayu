package schema

import "encoding/json"

// ChatRequest — canonical 요청. 파이프라인이 해석하는 필드만 타입화:
// Model(라우팅·단가), Messages(블록 순서·cache 불변식), Stream, MaxTokens
// (TPM 추정). system/tools/tool_choice/thinking/metadata는 raw 보존 —
// 교차 프로토콜 변환(M5)에서 타입 승격한다.
type ChatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens *int64    `json:"max_tokens,omitempty"`
	Stream    bool      `json:"stream,omitempty"`

	System     json.RawMessage `json:"system,omitempty"`
	Tools      json.RawMessage `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	Thinking   json.RawMessage `json:"thinking,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var chatRequestKnown = []string{
	"model", "messages", "max_tokens", "stream",
	"system", "tools", "tool_choice", "thinking", "metadata",
}

func (r *ChatRequest) UnmarshalJSON(data []byte) error {
	type plain ChatRequest
	extra, err := unmarshalWithExtra(data, (*plain)(r), chatRequestKnown...)
	r.Extra = extra
	return err
}

func (r ChatRequest) MarshalJSON() ([]byte, error) {
	type plain ChatRequest
	return marshalWithExtra(plain(r), r.Extra)
}
