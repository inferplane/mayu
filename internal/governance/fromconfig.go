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

// PoliciesFromConfig converts config-shaped teams into resolved TeamPolicy.
// USD→µUSD is ×1_000_000. RateBurst defaults to RatePerMin when unset (a full
// minute's worth may burst), with a floor of 1 so a positive RatePerMin always
// has a usable bucket.
func PoliciesFromConfig(in map[string]ConfigTeam) map[string]TeamPolicy {
	out := make(map[string]TeamPolicy, len(in))
	for name, c := range in {
		burst := c.RatePerMin
		if burst <= 0 {
			burst = 1
		}
		out[name] = TeamPolicy{
			RatePerMin:           c.RatePerMin,
			RateBurst:            burst,
			TokensPerMinute:      c.TokensPerMinute,
			TokensPerDay:         c.TokensPerDay,
			QuotaExceeded:        c.QuotaExceeded,
			BudgetMicrosPerMonth: int64(c.BudgetUSDPerMonth * 1_000_000),
			BudgetExceeded:       c.BudgetExceeded,
		}
	}
	return out
}
