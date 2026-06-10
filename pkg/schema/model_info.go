package schema

// ModelInfo is one entry in the Anthropic GET /v1/models response.
// Type is always "model" on the wire; CreatedAt is RFC3339.
type ModelInfo struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at,omitempty"`
}
