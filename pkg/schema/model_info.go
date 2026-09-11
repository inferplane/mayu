package schema

// ModelInfo is one entry in the Anthropic GET /v1/models response.
// Type is always "model" on the wire; CreatedAt is RFC3339.
type ModelInfo struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at,omitempty"`
	// ContextWindow is the gateway-declared total context limit in tokens
	// (config models.<name>.context_window). Appended with omitempty per the
	// schema's additive-field rule: absent = undeclared, and pre-existing
	// consumers see byte-identical entries. Not an official Anthropic wire
	// field — a gateway extension so context-aware clients stop assuming a
	// default window for unrecognized model ids.
	ContextWindow int64 `json:"context_window,omitempty"`
}
