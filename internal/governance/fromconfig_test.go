package governance

import "testing"

func TestPoliciesFromConfig(t *testing.T) {
	in := map[string]ConfigTeam{
		"platform-eng": {
			RatePerMin: 300, TokensPerMinute: 2_000_000, TokensPerDay: 50_000_000, QuotaExceeded: "block",
			BudgetUSDPerMonth: 5000, BudgetUSDPerDay: 250, BudgetExceeded: "warn",
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
	// 250 USD/day → 250_000_000 µUSD, and the single on_exceeded knob feeds
	// BOTH windows (design §D2).
	if p.BudgetMicrosPerDay != 250_000_000 {
		t.Fatalf("daily budget µUSD: %d", p.BudgetMicrosPerDay)
	}
	if p.BudgetDayExceeded != "warn" {
		t.Fatalf("daily budget exceeded policy: %q", p.BudgetDayExceeded)
	}
	// burst defaults to RatePerMin when unset (so a full minute's worth can burst)
	if p.RateBurst <= 0 {
		t.Fatalf("burst should default >0: %d", p.RateBurst)
	}
}

// TestPolicyFromLimitsMapsEveryField pins the struct-argument form of
// PolicyFromLimits: six positional arguments (including two adjacent int64
// pairs) became one Limits value, and the risk that conversion carries is a
// silently transposed field. Every number below is distinct so a swap fails.
func TestPolicyFromLimitsMapsEveryField(t *testing.T) {
	got := PolicyFromLimits(Limits{
		RatePerMin:           11,
		TokensPerMinute:      22,
		TokensPerDay:         33,
		QuotaExceeded:        "warn",
		BudgetMicrosPerMonth: 44,
		BudgetExceeded:       "block",
		BudgetMicrosPerDay:   55,
		BudgetDayExceeded:    "warn",
	})
	want := TeamPolicy{
		RatePerMin:           11,
		RateBurst:            11, // burst defaults to RatePerMin
		TokensPerMinute:      22,
		TokensPerDay:         33,
		QuotaExceeded:        "warn",
		BudgetMicrosPerMonth: 44,
		BudgetExceeded:       "block",
		BudgetMicrosPerDay:   55,
		BudgetDayExceeded:    "warn",
	}
	if got != want {
		t.Fatalf("PolicyFromLimits = %+v, want %+v", got, want)
	}
}

// TestPolicyFromLimitsBurstFloor keeps the burst rule pinned through the
// signature change: a zero or negative RatePerMin must still floor to 1, never
// 0, because 0 means "unlimited" to the limiter.
func TestPolicyFromLimitsBurstFloor(t *testing.T) {
	for _, rpm := range []int64{0, -5} {
		if got := PolicyFromLimits(Limits{RatePerMin: rpm}).RateBurst; got != 1 {
			t.Fatalf("RatePerMin=%d gave RateBurst=%d, want 1", rpm, got)
		}
	}
}

// TestUSDToMicrosRoundsRatherThanTruncates pins the µUSD conversion against
// binary-float representation error: a truncating cast turns $0.29 into
// 289_999 µUSD, silently narrowing every budget the operator configured.
func TestUSDToMicrosRoundsRatherThanTruncates(t *testing.T) {
	cases := []struct {
		usd  float64
		want int64
	}{
		{0.29, 290_000},    // 0.29*1e6 == 289999.99999999994 in float64
		{8.11, 8_110_000},  // another non-representable cent value
		{1.005, 1_005_000}, //
		{0, 0},
		{250, 250_000_000},
	}
	for _, c := range cases {
		if got := usdToMicros(c.usd); got != c.want {
			t.Errorf("usdToMicros(%v) = %d, want %d", c.usd, got, c.want)
		}
	}
}

func TestPoliciesFromConfigRoundsBudgets(t *testing.T) {
	out := PoliciesFromConfig(map[string]ConfigTeam{
		"demo": {BudgetUSDPerMonth: 0.29, BudgetUSDPerDay: 8.11, BudgetExceeded: "block"},
	})
	p := out["demo"]
	if p.BudgetMicrosPerMonth != 290_000 {
		t.Errorf("monthly = %d µUSD, want 290000", p.BudgetMicrosPerMonth)
	}
	if p.BudgetMicrosPerDay != 8_110_000 {
		t.Errorf("daily = %d µUSD, want 8110000", p.BudgetMicrosPerDay)
	}
}
