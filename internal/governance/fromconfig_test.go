package governance

import "testing"

func TestPoliciesFromConfig(t *testing.T) {
	in := map[string]ConfigTeam{
		"platform-eng": {
			RatePerMin: 300, TokensPerMinute: 2_000_000, TokensPerDay: 50_000_000, QuotaExceeded: "block",
			BudgetUSDPerMonth: 5000, BudgetExceeded: "warn",
		},
	}
	pol := PoliciesFromConfig(in)
	p := pol["platform-eng"]
	if p.RatePerMin != 300 || p.TokensPerDay != 50_000_000 || p.QuotaExceeded != "block" {
		t.Fatalf("policy: %+v", p)
	}
	// 5000 USD → 5_000_000_000 µUSD
	if p.BudgetMicrosPerMonth != 5_000_000_000 {
		t.Fatalf("budget µUSD: %d", p.BudgetMicrosPerMonth)
	}
	if p.BudgetExceeded != "warn" {
		t.Fatalf("budget exceeded policy: %q", p.BudgetExceeded)
	}
	// burst defaults to RatePerMin when unset (so a full minute's worth can burst)
	if p.RateBurst <= 0 {
		t.Fatalf("burst should default >0: %d", p.RateBurst)
	}
}
