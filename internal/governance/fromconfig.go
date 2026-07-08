package governance

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
	BudgetExceeded    string // block|warn
}

// PolicyFromLimits builds a TeamPolicy from already-resolved limits (budget in
// µUSD, no unit conversion here). Factored out so the burst rule below can
// never diverge between the config path (PoliciesFromConfig) and the D3
// keystore-team-record path (cmd/inferplane assembly's Governor.SetTeamLookup
// callback) — both must produce byte-identical TeamPolicy shapes for the same
// numbers, or ADR-016's "DB record wins" precedence would also silently
// change enforcement behavior, not just the source of the values.
func PolicyFromLimits(ratePerMin, tokensPerMinute, tokensPerDay int64, quotaExceeded string, budgetMicrosPerMonth int64, budgetExceeded string) TeamPolicy {
	burst := ratePerMin
	if burst <= 0 {
		burst = 1
	}
	return TeamPolicy{
		RatePerMin:           ratePerMin,
		RateBurst:            burst,
		TokensPerMinute:      tokensPerMinute,
		TokensPerDay:         tokensPerDay,
		QuotaExceeded:        quotaExceeded,
		BudgetMicrosPerMonth: budgetMicrosPerMonth,
		BudgetExceeded:       budgetExceeded,
	}
}

// PoliciesFromConfig converts config-shaped teams into resolved TeamPolicy.
// USD→µUSD is ×1_000_000; see PolicyFromLimits for the burst rule.
func PoliciesFromConfig(in map[string]ConfigTeam) map[string]TeamPolicy {
	out := make(map[string]TeamPolicy, len(in))
	for name, c := range in {
		out[name] = PolicyFromLimits(c.RatePerMin, c.TokensPerMinute, c.TokensPerDay,
			c.QuotaExceeded, int64(c.BudgetUSDPerMonth*1_000_000), c.BudgetExceeded)
	}
	return out
}
