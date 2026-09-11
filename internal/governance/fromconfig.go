package governance

import "math"

// ConfigTeam is the flat, config-shaped per-team input for PoliciesFromConfig.
// It is defined here (not imported from internal/config) so governance stays
// independent of the config package — the caller (main.go) maps
// config.TeamConfig → ConfigTeam. BudgetUSDPerMonth is a human USD float;
// PoliciesFromConfig converts it to µUSD.
type ConfigTeam struct {
	RatePerMin        int64
	TokensPerMinute   int64
	TokensPerDay      int64
	QuotaExceeded     string // block|warn
	BudgetUSDPerMonth float64
	// BudgetUSDPerDay is the calendar-day counterpart; there is deliberately
	// no BudgetDayExceeded here because config.BudgetConfig carries a single
	// on_exceeded knob that governs both windows (BudgetExceeded feeds both).
	BudgetUSDPerDay float64
	BudgetExceeded  string // block|warn
}

// Limits is PolicyFromLimits's already-resolved input, one field per governance
// dimension. It replaced six positional arguments — two adjacent int64 pairs
// among them, which a caller could silently transpose — so that adding a
// dimension is an additive struct field rather than an edit at every call site.
// Field names match the TeamPolicy fields they feed.
type Limits struct {
	RatePerMin           int64
	TokensPerMinute      int64
	TokensPerDay         int64
	QuotaExceeded        string // block|warn
	BudgetMicrosPerMonth int64
	BudgetExceeded       string // block|warn
	BudgetMicrosPerDay   int64
	BudgetDayExceeded    string // block|warn
}

// PolicyFromLimits builds a TeamPolicy from already-resolved limits (budget in
// µUSD, no unit conversion here). Factored out so the burst rule below can
// never diverge between the config path (PoliciesFromConfig) and the D3
// keystore-team-record path (cmd/mayu assembly's Governor.SetTeamLookup
// callback) — both must produce byte-identical TeamPolicy shapes for the same
// numbers, or ADR-016's "DB record wins" precedence would also silently
// change enforcement behavior, not just the source of the values.
func PolicyFromLimits(l Limits) TeamPolicy {
	burst := l.RatePerMin
	if burst <= 0 {
		burst = 1
	}
	return TeamPolicy{
		RatePerMin:           l.RatePerMin,
		RateBurst:            burst,
		TokensPerMinute:      l.TokensPerMinute,
		TokensPerDay:         l.TokensPerDay,
		QuotaExceeded:        l.QuotaExceeded,
		BudgetMicrosPerMonth: l.BudgetMicrosPerMonth,
		BudgetExceeded:       l.BudgetExceeded,
		BudgetMicrosPerDay:   l.BudgetMicrosPerDay,
		BudgetDayExceeded:    l.BudgetDayExceeded,
	}
}

// usdToMicros converts a human USD float to integer µUSD, rounding rather than
// truncating. A bare int64(usd * 1_000_000) cast loses a µUSD on any value the
// binary float cannot represent exactly — $0.29 evaluates to 289999.99999999994
// and truncates to 289999 — which silently narrows the operator's configured
// budget. Mirrors internal/pricing's usdToMicros; kept local so governance
// stays a leaf package (see ConfigTeam's comment and internal/CLAUDE.md).
func usdToMicros(usd float64) int64 {
	return int64(math.Round(usd * 1_000_000))
}

// PoliciesFromConfig converts config-shaped teams into resolved TeamPolicy.
// USD→µUSD is ×1_000_000 via usdToMicros; see PolicyFromLimits for the burst rule.
func PoliciesFromConfig(in map[string]ConfigTeam) map[string]TeamPolicy {
	out := make(map[string]TeamPolicy, len(in))
	for name, c := range in {
		out[name] = PolicyFromLimits(Limits{
			RatePerMin:           c.RatePerMin,
			TokensPerMinute:      c.TokensPerMinute,
			TokensPerDay:         c.TokensPerDay,
			QuotaExceeded:        c.QuotaExceeded,
			BudgetMicrosPerMonth: usdToMicros(c.BudgetUSDPerMonth),
			BudgetExceeded:       c.BudgetExceeded,
			BudgetMicrosPerDay:   usdToMicros(c.BudgetUSDPerDay),
			BudgetDayExceeded:    c.BudgetExceeded,
		})
	}
	return out
}
