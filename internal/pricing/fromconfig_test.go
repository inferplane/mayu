package pricing

import "testing"

func TestTableFromConfig(t *testing.T) {
	overrides := map[string]map[string]ConfigRate{
		"anthropic-direct": {"claude-sonnet-4-6": {InputPerMTok: 3.0, OutputPerMTok: 15.0, CacheReadPerMTok: 0.3, CacheWrite5mPerMTok: 3.75, CacheWrite1hPerMTok: 6.0}},
	}
	tbl := FromConfig("allow", overrides)
	if tbl.OnMissing() != OnMissingAllow {
		t.Fatal("on_missing allow")
	}
	cost, missing := tbl.CostUSDMicros("anthropic-direct", "claude-sonnet-4-6", Usage{Input: 1_000_000})
	if missing || cost != 3_000_000 { // 1M tokens * 3.0 USD/MTok = 3 USD = 3_000_000 µUSD
		t.Fatalf("cost=%d missing=%v", cost, missing)
	}
}

func TestFromConfigStartsFromBundled(t *testing.T) {
	// with no overrides, bundled rates apply
	tbl := FromConfig("allow", nil)
	cost, missing := tbl.CostUSDMicros("anthropic-direct", "claude-sonnet-4-6", Usage{Input: 1_000_000})
	if missing || cost == 0 {
		t.Fatalf("bundled rate should apply: cost=%d missing=%v", cost, missing)
	}
}
