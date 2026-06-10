package schema

import (
	"encoding/json"
	"testing"
)

func TestModelInfoRoundTrip(t *testing.T) {
	// Anthropic /v1/models 의 data 원소 형태.
	in := `{"type":"model","id":"claude-sonnet-4-6","display_name":"Claude Sonnet 4.6","created_at":"2026-02-19T00:00:00Z"}`
	var m ModelInfo
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "claude-sonnet-4-6" || m.DisplayName != "Claude Sonnet 4.6" || m.Type != "model" {
		t.Fatalf("typed fields: %+v", m)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONSemanticEqual(t, []byte(in), out)
}
