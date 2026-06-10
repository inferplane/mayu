package schema

import "encoding/json"

// Usage — budget 정산의 입력 (§5.3). cache 토큰은 TTL별 단가가 다르므로
// (5m=1.25x, 1h=2x) 반드시 구분 보존한다.
// 모든 수치는 *int64: upstream이 보낸 키만 재방출한다. message_delta
// usage는 output_tokens만 싣는 경우가 있고(no-omitempty면 키 추가 발생),
// 명시적 0("cache_creation_input_tokens":0)은 보존해야 한다(omitempty
// 값 타입이면 드랍) — 48d412d/3d5e050과 동일한 결함 계열의 선제 차단.
type Usage struct {
	InputTokens              *int64                     `json:"input_tokens,omitempty"`
	OutputTokens             *int64                     `json:"output_tokens,omitempty"`
	CacheReadInputTokens     *int64                     `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int64                     `json:"cache_creation_input_tokens,omitempty"`
	CacheCreation            *CacheCreation             `json:"cache_creation,omitempty"`
	Extra                    map[string]json.RawMessage `json:"-"`
}

type CacheCreation struct {
	Ephemeral5mInputTokens *int64                     `json:"ephemeral_5m_input_tokens,omitempty"`
	Ephemeral1hInputTokens *int64                     `json:"ephemeral_1h_input_tokens,omitempty"`
	Extra                  map[string]json.RawMessage `json:"-"`
}

func (u *Usage) UnmarshalJSON(data []byte) error {
	type plain Usage
	extra, err := unmarshalWithExtra(data, (*plain)(u),
		"input_tokens", "output_tokens", "cache_read_input_tokens",
		"cache_creation_input_tokens", "cache_creation")
	u.Extra = extra
	return err
}

func (u Usage) MarshalJSON() ([]byte, error) {
	type plain Usage
	return marshalWithExtra(plain(u), u.Extra)
}

func (c *CacheCreation) UnmarshalJSON(data []byte) error {
	type plain CacheCreation
	extra, err := unmarshalWithExtra(data, (*plain)(c),
		"ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens")
	c.Extra = extra
	return err
}

func (c CacheCreation) MarshalJSON() ([]byte, error) {
	type plain CacheCreation
	return marshalWithExtra(plain(c), c.Extra)
}

// ChatResponse — canonical 비스트리밍 응답 (스트리밍 message_start의
// 골격이기도 하다). stop_reason/stop_sequence는 null 유의미 → 포인터.
type ChatResponse struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Role  string `json:"role"`
	Model string `json:"model"`
	// Content: 호출자는 nil 대신 []ContentBlock{}을 설정할 것 — nil은
	// "content":null로 방출되며 실제 API 형태는 항상 배열이다.
	Content      []ContentBlock             `json:"content"`
	StopReason   *string                    `json:"stop_reason"`
	StopSequence *string                    `json:"stop_sequence"`
	Usage        *Usage                     `json:"usage,omitempty"`
	Extra        map[string]json.RawMessage `json:"-"`
}

var chatResponseKnown = []string{
	"id", "type", "role", "model", "content",
	"stop_reason", "stop_sequence", "usage",
}

func (r *ChatResponse) UnmarshalJSON(data []byte) error {
	type plain ChatResponse
	extra, err := unmarshalWithExtra(data, (*plain)(r), chatResponseKnown...)
	r.Extra = extra
	return err
}

func (r ChatResponse) MarshalJSON() ([]byte, error) {
	type plain ChatResponse
	return marshalWithExtra(plain(r), r.Extra)
}
