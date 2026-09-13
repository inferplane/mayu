package openai

import (
	"encoding/json"
	"testing"

	"github.com/inferplane/inferplane/pkg/schema"
)

func TestCanonicalFunctionStrictnessSurvivesChatRendering(t *testing.T) {
	for _, setting := range []string{"true", "false", ""} {
		t.Run(setting, func(t *testing.T) {
			tool := `{"name":"read","input_schema":{"type":"object"}}`
			if setting != "" {
				tool = `{"name":"read","strict":` + setting + `,"input_schema":{"type":"object"}}`
			}
			raw := CanonicalToRequest(&schema.ChatRequest{Model: "m", Tools: json.RawMessage(`[` + tool + `]`)})
			var sink struct {
				Tools []struct {
					Function struct {
						Strict *bool `json:"strict"`
					} `json:"function"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(raw, &sink); err != nil || len(sink.Tools) != 1 {
				t.Fatalf("invalid Chat request: %s; %v", raw, err)
			}
			got := sink.Tools[0].Function.Strict
			if setting == "" {
				if got != nil {
					t.Fatalf("legacy canonical tools acquired strict semantics: %s", raw)
				}
			} else if got == nil || *got != (setting == "true") {
				t.Fatalf("canonical strict=%s lost on Chat wire: %s", setting, raw)
			}
		})
	}
}
