package audit

import "testing"

// TestDenyReasonValues pins the exact wire string for each taxonomy constant
// (ADR-020 deferred item). A rename here is a breaking change to any external
// audit-log consumer coding against these values, so this must fail loudly.
// DenyRegionBlocked stays "region_blocked" — the literal already in the audit
// log today (this is not a new value, just the first one made part of a
// closed set).
func TestDenyReasonValues(t *testing.T) {
	cases := map[DenyReason]string{
		DenyModelNotAllowed:      "model_not_allowed",
		DenyTeamRateLimited:      "team_rate_limited",
		DenyTeamTokenRateLimited: "team_token_rate_limited",
		DenyTeamQuotaExceeded:    "team_quota_exceeded",
		DenyKeyRateLimited:       "key_rate_limited",
		DenyKeyTokenRateLimited:  "key_token_rate_limited",
		DenyTeamBudgetExceeded:   "team_budget_exceeded",
		DenyKeyBudgetExceeded:    "key_budget_exceeded",
		DenyRegionBlocked:        "region_blocked",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Fatalf("DenyReason %v = %q, want %q", got, string(got), want)
		}
	}
	if len(cases) != 9 {
		t.Fatalf("expected 9 taxonomy constants, test table has %d", len(cases))
	}
}

// TestDenyReasonPtr pins Ptr()'s contract: a non-nil pointer to the reason's
// own string value, safe to assign directly into OutcomeRef.Error.
func TestDenyReasonPtr(t *testing.T) {
	p := DenyRegionBlocked.Ptr()
	if p == nil {
		t.Fatal("Ptr() returned nil")
	}
	if *p != "region_blocked" {
		t.Fatalf("*Ptr() = %q, want %q", *p, "region_blocked")
	}
}
