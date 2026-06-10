package schema

import "encoding/json"

// CacheControl marks a prompt-cache breakpoint. The gateway never adds,
// moves, or strips these (§4.4 — cache pass-through is a hard constraint).
type CacheControl struct {
	Type  string                     `json:"type"`          // "ephemeral"
	TTL   string                     `json:"ttl,omitempty"` // "5m" | "1h" | "" (기본 5m)
	Extra map[string]json.RawMessage `json:"-"`
}

func (c *CacheControl) UnmarshalJSON(data []byte) error {
	type plain CacheControl
	extra, err := unmarshalWithExtra(data, (*plain)(c), "type", "ttl")
	c.Extra = extra
	return err
}

func (c CacheControl) MarshalJSON() ([]byte, error) {
	type plain CacheControl
	return marshalWithExtra(plain(c), c.Extra)
}

// ContentBlock is the canonical content unit — a single-struct tagged union
// over the Anthropic block vocabulary. Unknown block types round-trip via
// Extra; tool_result.content stays raw until a milestone needs to interpret it.
type ContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result — content is string OR block array; raw preserves both forms
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   *bool           `json:"is_error,omitempty"`

	// thinking / redacted_thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`

	CacheControl *CacheControl `json:"cache_control,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var contentBlockKnown = []string{
	"type", "text", "id", "name", "input", "tool_use_id", "content",
	"is_error", "thinking", "signature", "data", "cache_control",
}

func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	type plain ContentBlock
	extra, err := unmarshalWithExtra(data, (*plain)(b), contentBlockKnown...)
	b.Extra = extra
	return err
}

func (b ContentBlock) MarshalJSON() ([]byte, error) {
	type plain ContentBlock
	return marshalWithExtra(plain(b), b.Extra)
}
