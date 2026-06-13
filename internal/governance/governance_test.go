package governance

import (
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func testGovernor() *Governor {
	teams := map[string]TeamPolicy{
		"t": {RatePerMin: 60, RateBurst: 2, TokensPerDay: 1000, QuotaExceeded: "block", BudgetMicrosPerMonth: 0, BudgetExceeded: "block"},
	}
	return NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), nil)
}

// testTable is the standard rate table tests pass into Settle.
func testTable() *pricing.Table {
	return pricing.New(pricing.OnMissingAllow, map[pricing.Key]pricing.Rate{{Provider: "p", Model: "m"}: {InputPerMTok: 1_000_000, OutputPerMTok: 1_000_000}})
}

func TestGovernorQuotaBlocks(t *testing.T) {
	g := testGovernor()
	// debit team t up to its daily limit, then a pre-check must block
	g.lim.DebitQuota("quota:t", 1000, 24*time.Hour)
	dec := g.PreCheck("t", 100)
	if dec.Allowed {
		t.Fatalf("quota exhausted must block: %+v", dec)
	}
	if dec.Status != 429 {
		t.Fatalf("quota block status = %d, want 429", dec.Status)
	}
}

func TestGovernorSettleComputesCost(t *testing.T) {
	g := testGovernor()
	cost, missing := g.Settle("t", "p", "m", pricing.Usage{Input: 1000, Output: 500}, testTable())
	// 1000*1 + 500*1 = 1500 µUSD
	if missing || cost != 1500 {
		t.Fatalf("settle cost=%d missing=%v", cost, missing)
	}
}

func TestGovernorSettleRecordsMetrics(t *testing.T) {
	m := metrics.New()
	teams := map[string]TeamPolicy{"t": {}}
	// Known rate for (p,m); unknown for (p,ghost) → pricing_miss.
	tbl := pricing.New(pricing.OnMissingAllow, map[pricing.Key]pricing.Rate{
		{Provider: "p", Model: "m"}: {InputPerMTok: 1_000_000, OutputPerMTok: 1_000_000},
	})
	g := NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), m)

	g.Settle("t", "p", "m", pricing.Usage{Input: 1000, Output: 500}, tbl) // 1500 µUSD → 0.0015 USD
	if got, err := testutil.GatherAndCount(m.Registry(), "inferplane_budget_spend_usd_total"); err != nil || got == 0 {
		t.Fatalf("budget_spend not recorded (count=%d err=%v)", got, err)
	}

	g.Settle("t", "p", "ghost", pricing.Usage{Input: 10}, tbl) // no rate → pricing_miss
	if got, err := testutil.GatherAndCount(m.Registry(), "inferplane_pricing_miss_total"); err != nil || got == 0 {
		t.Fatalf("pricing_miss not recorded for unknown (provider,model) (count=%d err=%v)", got, err)
	}
}

func TestGovernorTPMBlocks(t *testing.T) {
	teams := map[string]TeamPolicy{"t": {TokensPerMinute: 1000}}
	g := NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), nil)
	// first request estimate 800 → ok; second estimate 800 → 1600>1000 burst → block
	if d := g.PreCheck("t", 800); !d.Allowed {
		t.Fatalf("first: %+v", d)
	}
	if d := g.PreCheck("t", 800); d.Allowed {
		t.Fatalf("TPM should block second: %+v", d)
	}
}

// --- hot-reload: pricing passed per-call (plan 2026-06-13 task 3) ---

func TestSettleUsesPassedTable(t *testing.T) {
	g := NewGovernor(map[string]TeamPolicy{"t": {}}, limiter.NewMemory(), budget.NewMemory(), nil)
	cheap := pricing.New(pricing.OnMissingAllow, map[pricing.Key]pricing.Rate{{Provider: "p", Model: "m"}: {InputPerMTok: 1_000_000}})
	dear := pricing.New(pricing.OnMissingAllow, map[pricing.Key]pricing.Rate{{Provider: "p", Model: "m"}: {InputPerMTok: 5_000_000}})
	c1, _ := g.Settle("t", "p", "m", pricing.Usage{Input: 1000}, cheap)
	c2, _ := g.Settle("t", "p", "m", pricing.Usage{Input: 1000}, dear)
	if c1 != 1000 || c2 != 5000 {
		t.Fatalf("Settle must bill with the PASSED table: c1=%d c2=%d", c1, c2)
	}
	// nil table → cost 0, pricing missing (never panics).
	if c, miss := g.Settle("t", "p", "m", pricing.Usage{Input: 1000}, nil); c != 0 || !miss {
		t.Fatalf("nil table: cost=%d missing=%v, want 0,true", c, miss)
	}
}

func TestGovernorCountersIndependentOfTable(t *testing.T) {
	// The budget counter is the governor's own state; which table is passed
	// does not rebuild or reset it — spend accumulates across tables.
	teams := map[string]TeamPolicy{"t": {BudgetMicrosPerMonth: 1_000_000, BudgetExceeded: "block"}}
	g := NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), nil)
	tbl := pricing.New(pricing.OnMissingAllow, map[pricing.Key]pricing.Rate{{Provider: "p", Model: "m"}: {InputPerMTok: 1_000_000}})
	g.Settle("t", "p", "m", pricing.Usage{Input: 400_000}, tbl) // 400k µUSD
	g.Settle("t", "p", "m", pricing.Usage{Input: 400_000}, tbl) // +400k = 800k
	// A third would exceed 1M; PreCheck on accumulated spend now blocks.
	if d := g.PreCheck("t", 0); d.Allowed {
		// budget pre-check is on accumulated spend; 800k < 1M so still allowed —
		// assert the counter actually moved by checking a debit beyond the cap.
	}
	g.Settle("t", "p", "m", pricing.Usage{Input: 400_000}, tbl) // 1.2M > cap
	if d := g.PreCheck("t", 0); d.Allowed {
		t.Fatal("budget counter did not accumulate across Settle calls (table-independent state expected)")
	}
}
