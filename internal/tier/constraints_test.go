package tier

import (
	"encoding/json"
	"testing"

	"github.com/inferplane/inferplane/internal/policy"
)

func TestStrictConstraintsCannotBeLoosenedByLegacyTier(t *testing.T) {
	tb := NewTable()
	var active []policy.ActiveTier
	if err := json.Unmarshal([]byte(`[{"policy":"strict","team":"t","thresholdPercent":80,"enforceTargets":true,"substitute":{"premium":"cheap"}},{"policy":"legacy","team":"t","thresholdPercent":99,"substitute":{"premium":"expensive"}}]`), &active); err != nil {
		t.Fatal(err)
	}
	tb.Set(active)
	lookup, ok := any(tb).(interface {
		Constraints(string) map[string]string
	})
	if !ok {
		t.Fatal("table has no strict constraint lookup")
	}
	if tb.Get("t")["premium"] != "expensive" || lookup.Constraints("t")["premium"] != "cheap" {
		t.Fatal("legacy and strict lookup semantics were mixed")
	}
	copy := lookup.Constraints("t")
	copy["premium"] = "mutated"
	if lookup.Constraints("t")["premium"] != "cheap" {
		t.Fatal("caller mutated strict snapshot")
	}
}

func TestStrictConstraintConflictDeniesUntilReplacement(t *testing.T) {
	tb := NewTable()
	lookup, ok := any(tb).(interface {
		Constraints(string) map[string]string
	})
	if !ok {
		t.Fatal("table has no strict constraint lookup")
	}
	var active []policy.ActiveTier
	_ = json.Unmarshal([]byte(`[{"team":"t","enforceTargets":true,"substitute":{"a":"b"}},{"team":"t","enforceTargets":true,"substitute":{"a":"c"}}]`), &active)
	tb.Set(active)
	if target, exists := lookup.Constraints("t")["a"]; !exists || target != "" {
		t.Fatalf("conflicting restrictions did not produce deny: %v", lookup.Constraints("t"))
	}
	tb.Set(nil)
	if lookup.Constraints("t") != nil {
		t.Fatal("replacement did not clear old constraints")
	}
}
