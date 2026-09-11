package sensitivity

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func TestNumericScalarInspection(t *testing.T) {
	for _, protocol := range []string{"anthropic", "openai", "bedrock"} {
		raw := []byte(`{"messages":[{"role":"user","content":"hi"}],"metadata":{"card":4111111111111111}}`)
		before := slices.Clone(raw)
		got, err := NewInspector().Inspect(context.Background(), protocol, raw)
		if err != nil || !slices.Contains(got.Categories, "credit_card") {
			t.Fatalf("%s: numeric card missed: %+v %v", protocol, got, err)
		}
		if !slices.Equal(raw, before) {
			t.Fatal("inspection mutated original bytes")
		}
	}
}

func TestNumericSpellingAndAccounting(t *testing.T) {
	var visited []string
	w := walker{ctx: context.Background(), visit: func(s string) error { visited = append(visited, s); return nil }}
	root, err := w.decode([]byte(`[4111111111111111,1.2300e+4,-0]`), 0)
	want := []string{"4111111111111111", "1.2300e+4", "-0"}
	if err != nil || !reflect.DeepEqual(visited, want) {
		t.Fatalf("numeric spelling: %v %v", visited, err)
	}
	// Four JSON nodes (32), and exactly 27 numeric spelling bytes.
	if w.inputTokens != 59 {
		t.Fatalf("numeric tokens double-counted or lost: %d", w.inputTokens)
	}
	if root.([]any)[1] != json.Number("1.2300e+4") {
		t.Fatal("numeric value was converted through float")
	}
}
