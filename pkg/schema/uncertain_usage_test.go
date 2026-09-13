package schema

import (
	"encoding/json"
	"testing"
)

func TestUsageUncertaintySurvivesFoldWithoutChangingWire(t *testing.T) {
	zero := int64(0)
	u := MergeUsage(&Usage{InputTokens: &zero, AccountingUncertain: true}, &Usage{OutputTokens: &zero})
	if !u.AccountingUncertain {
		t.Fatal("later output count erased unknown input accounting")
	}
	raw, err := json.Marshal(u)
	if err != nil || string(raw) != `{"input_tokens":0,"output_tokens":0}` {
		t.Fatalf("observation metadata changed wire: %s %v", raw, err)
	}
}
