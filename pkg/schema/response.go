package schema

import "encoding/json"

// Usage — input to budget settlement. Cache tokens are priced per TTL tier
// (5m=1.25x, 1h=2x), so the tiers must be kept separate rather than collapsed.
// Every field is *int64: only re-emit keys the upstream actually sent. A
// message_delta's usage sometimes carries only output_tokens (a non-omitempty
// field would add a key that wasn't there), and an explicit 0
// (`"cache_creation_input_tokens":0`) must survive (an omitempty value type
// would drop it) — this preempts the same bug class fixed in 48d412d/3d5e050.
type Usage struct {
	InputTokens              *int64                     `json:"input_tokens,omitempty"`
	OutputTokens             *int64                     `json:"output_tokens,omitempty"`
	CacheReadInputTokens     *int64                     `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int64                     `json:"cache_creation_input_tokens,omitempty"`
	CacheCreation            *CacheCreation             `json:"cache_creation,omitempty"`
	Extra                    map[string]json.RawMessage `json:"-"`
	// AccountingUncertain preserves a provider adapter's loss of billing
	// information. It is observation-only and never changes wire bytes.
	AccountingUncertain bool `json:"-"`
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

// ChatResponse — canonical non-streaming response (also the skeleton of a
// streaming message_start). stop_reason/stop_sequence are pointers because
// null is meaningful, not absent.
type ChatResponse struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Role  string `json:"role"`
	Model string `json:"model"`
	// Content: callers should set []ContentBlock{} rather than nil — nil
	// marshals as "content":null, but the real API shape is always an array.
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
