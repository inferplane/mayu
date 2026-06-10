package pricing

import "math"

// ConfigRate holds per-MTok rates as human USD floats. It is defined here (not
// imported from internal/config) so pricing stays independent of the config
// package — the caller (main.go) maps config.RateConfig → ConfigRate.
type ConfigRate struct {
	InputPerMTok        float64
	OutputPerMTok       float64
	CacheReadPerMTok    float64
	CacheWrite5mPerMTok float64
	CacheWrite1hPerMTok float64
}

// FromConfig builds a Table starting from Bundled() rates and applying the
// per-(provider,model) overrides, converting USD-per-MTok floats to µUSD-per-
// MTok int64 via round-half-away-from-zero. onMissing "block" selects
// OnMissingBlock; anything else selects OnMissingAllow.
func FromConfig(onMissing string, overrides map[string]map[string]ConfigRate) *Table {
	rates := Bundled()
	for provider, models := range overrides {
		for model, cr := range models {
			rates[Key{Provider: provider, Model: model}] = Rate{
				InputPerMTok:        usdToMicros(cr.InputPerMTok),
				OutputPerMTok:       usdToMicros(cr.OutputPerMTok),
				CacheReadPerMTok:    usdToMicros(cr.CacheReadPerMTok),
				CacheWrite5mPerMTok: usdToMicros(cr.CacheWrite5mPerMTok),
				CacheWrite1hPerMTok: usdToMicros(cr.CacheWrite1hPerMTok),
			}
		}
	}
	om := OnMissingAllow
	if onMissing == "block" {
		om = OnMissingBlock
	}
	return New(om, rates)
}

func usdToMicros(usd float64) int64 {
	return int64(math.Round(usd * 1_000_000))
}
