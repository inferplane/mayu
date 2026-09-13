package policy

import (
	"encoding/json"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
)

func sharedDoc(t *testing.T, rule string) v1alpha1.GovernancePolicy {
	t.Helper()
	var doc v1alpha1.GovernancePolicy
	if err := json.Unmarshal([]byte(`{"apiVersion":"inferplane.dev/v1alpha1","kind":"GovernancePolicy","metadata":{"name":"global","generation":1},"spec":{"subject":{"user":"alice"},"rules":[`+rule+`]}}`), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestTokenQuotaWireValidation(t *testing.T) {
	for _, tc := range []struct {
		name, rule string
		valid      bool
	}{
		{"day", `{"name":"q","failurePolicy":"FailClosed","tokenQuota":{"limitTokens":5000,"period":"CalendarDay"}}`, true},
		{"month", `{"name":"q","failurePolicy":"FailClosed","tokenQuota":{"limitTokens":5000,"period":"CalendarMonth"}}`, true},
		{"missing-period", `{"name":"q","failurePolicy":"FailClosed","tokenQuota":{"limitTokens":5000}}`, false},
		{"zero", `{"name":"q","failurePolicy":"FailClosed","tokenQuota":{"limitTokens":0,"period":"CalendarDay"}}`, false},
		{"negative", `{"name":"q","failurePolicy":"FailClosed","tokenQuota":{"limitTokens":-1,"period":"CalendarDay"}}`, false},
		{"unknown-period", `{"name":"q","failurePolicy":"FailClosed","tokenQuota":{"limitTokens":5000,"period":"RollingDay"}}`, false},
		{"fail-open", `{"name":"q","failurePolicy":"FailOpen","tokenQuota":{"limitTokens":5000,"period":"CalendarDay"}}`, false},
		{"two-kinds", `{"name":"q","failurePolicy":"FailClosed","rate":{"rpm":10},"tokenQuota":{"limitTokens":5000,"period":"CalendarDay"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := sharedDoc(t, tc.rule)
			_, err := FromV1Alpha1(&doc)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}

func TestOnlySharedDataPlaneAcceptsTokenQuotaAndUserRate(t *testing.T) {
	for _, rule := range []string{
		`{"name":"q","failurePolicy":"FailClosed","tokenQuota":{"limitTokens":5000,"period":"CalendarDay"}}`,
		`{"name":"r","failurePolicy":"FailOpen","rate":{"rpm":10,"tpm":5000}}`,
	} {
		doc := sharedDoc(t, rule)
		legacy := NewEmptyStore()
		if rejected := legacy.ApplyWire([]v1alpha1.GovernancePolicy{doc}); len(rejected) != 1 {
			t.Fatal("local profile silently accepted a global-only rule")
		}
		if doc.Spec.Rules[0].TokenQuota != nil {
			requireRoutingRejection(t, legacy, "any-team", "alice")
		}
		shared := NewEmptyStore()
		enabler, ok := any(shared).(interface{ SetSharedEnforcement(bool) })
		if !ok {
			t.Fatal("no explicit shared-enforcement gate")
		}
		enabler.SetSharedEnforcement(true)
		if rejected := shared.ApplyWire([]v1alpha1.GovernancePolicy{doc}); len(rejected) != 0 {
			t.Fatalf("shared profile rejected supported rule: %+v", rejected)
		}
	}
}
