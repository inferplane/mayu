package pricing

// Bundled returns the default rate table (µUSD per 1M tokens). Operators
// override via config; self-hosted models supply their own chargeback rates.
func Bundled() map[Key]Rate {
	return map[Key]Rate{
		{"anthropic-direct", "claude-sonnet-4-6"}: {InputPerMTok: 3_000_000, OutputPerMTok: 15_000_000, CacheReadPerMTok: 300_000, CacheWrite5mPerMTok: 3_750_000, CacheWrite1hPerMTok: 6_000_000},
		{"anthropic-direct", "claude-opus-4-8"}:   {InputPerMTok: 5_000_000, OutputPerMTok: 25_000_000, CacheReadPerMTok: 500_000, CacheWrite5mPerMTok: 6_250_000, CacheWrite1hPerMTok: 10_000_000},
	}
}
