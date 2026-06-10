package pricing

import "testing"

func TestCostUSDMicrosRoundHalfEven(t *testing.T) {
	tbl := New(OnMissingAllow, map[Key]Rate{
		{"anthropic-direct", "claude-sonnet-4-6"}: {InputPerMTok: 3_000_000, OutputPerMTok: 15_000_000, CacheReadPerMTok: 300_000, CacheWrite5mPerMTok: 3_750_000, CacheWrite1hPerMTok: 6_000_000},
	})
	// 1000 input, 500 output, 45000 cache_read, 1024 cache_write(5m)
	u := Usage{Input: 1000, Output: 500, CacheRead: 45000, CacheWrite5m: 1024}
	cost, missing := tbl.CostUSDMicros("anthropic-direct", "claude-sonnet-4-6", u)
	if missing {
		t.Fatal("rate present, should not be missing")
	}
	// input 1000*3_000_000/1e6=3000; output 500*15_000_000/1e6=7500;
	// cache_read 45000*300_000/1e6=13500; cache_write5m 1024*3_750_000/1e6=3840
	want := int64(3000 + 7500 + 13500 + 3840)
	if cost != want {
		t.Fatalf("cost = %d µUSD, want %d", cost, want)
	}
}

func TestOnMissingAllowReturnsZeroAndMissing(t *testing.T) {
	tbl := New(OnMissingAllow, nil)
	cost, missing := tbl.CostUSDMicros("p", "unknown-model", Usage{Input: 100})
	if cost != 0 || !missing {
		t.Fatalf("missing model: cost=%d missing=%v (want 0,true)", cost, missing)
	}
}

func TestOnMissingBlock(t *testing.T) {
	tbl := New(OnMissingBlock, nil)
	if tbl.OnMissing() != OnMissingBlock {
		t.Fatal("on_missing policy not stored")
	}
}

func TestCacheWriteTTLTiers(t *testing.T) {
	tbl := New(OnMissingAllow, map[Key]Rate{
		{"p", "m"}: {CacheWrite5mPerMTok: 1_250_000, CacheWrite1hPerMTok: 2_000_000},
	})
	c5, _ := tbl.CostUSDMicros("p", "m", Usage{CacheWrite5m: 1_000_000})
	c1h, _ := tbl.CostUSDMicros("p", "m", Usage{CacheWrite1h: 1_000_000})
	if c5 != 1_250_000 || c1h != 2_000_000 {
		t.Fatalf("ttl tiers: 5m=%d 1h=%d", c5, c1h)
	}
}
