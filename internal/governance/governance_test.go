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
	dec := g.PreCheck("t", "", KeyPolicy{}, 100)
	if dec.Allowed {
		t.Fatalf("quota exhausted must block: %+v", dec)
	}
	if dec.Status != 429 {
		t.Fatalf("quota block status = %d, want 429", dec.Status)
	}
}

func TestGovernorSettleComputesCost(t *testing.T) {
	g := testGovernor()
	cost, missing := g.Settle("t", "", KeyPolicy{}, "p", "m", pricing.Usage{Input: 1000, Output: 500}, testTable())
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

	g.Settle("t", "", KeyPolicy{}, "p", "m", pricing.Usage{Input: 1000, Output: 500}, tbl) // 1500 µUSD → 0.0015 USD
	if got, err := testutil.GatherAndCount(m.Registry(), "inferplane_budget_spend_usd_total"); err != nil || got == 0 {
		t.Fatalf("budget_spend not recorded (count=%d err=%v)", got, err)
	}

	g.Settle("t", "", KeyPolicy{}, "p", "ghost", pricing.Usage{Input: 10}, tbl) // no rate → pricing_miss
	if got, err := testutil.GatherAndCount(m.Registry(), "inferplane_pricing_miss_total"); err != nil || got == 0 {
		t.Fatalf("pricing_miss not recorded for unknown (provider,model) (count=%d err=%v)", got, err)
	}
}

func TestGovernorTPMBlocks(t *testing.T) {
	teams := map[string]TeamPolicy{"t": {TokensPerMinute: 1000}}
	g := NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), nil)
	// first request estimate 800 → ok; second estimate 800 → 1600>1000 burst → block
	if d := g.PreCheck("t", "", KeyPolicy{}, 800); !d.Allowed {
		t.Fatalf("first: %+v", d)
	}
	if d := g.PreCheck("t", "", KeyPolicy{}, 800); d.Allowed {
		t.Fatalf("TPM should block second: %+v", d)
	}
}

// --- hot-reload: pricing passed per-call (plan 2026-06-13 task 3) ---

func TestSettleUsesPassedTable(t *testing.T) {
	g := NewGovernor(map[string]TeamPolicy{"t": {}}, limiter.NewMemory(), budget.NewMemory(), nil)
	cheap := pricing.New(pricing.OnMissingAllow, map[pricing.Key]pricing.Rate{{Provider: "p", Model: "m"}: {InputPerMTok: 1_000_000}})
	dear := pricing.New(pricing.OnMissingAllow, map[pricing.Key]pricing.Rate{{Provider: "p", Model: "m"}: {InputPerMTok: 5_000_000}})
	c1, _ := g.Settle("t", "", KeyPolicy{}, "p", "m", pricing.Usage{Input: 1000}, cheap)
	c2, _ := g.Settle("t", "", KeyPolicy{}, "p", "m", pricing.Usage{Input: 1000}, dear)
	if c1 != 1000 || c2 != 5000 {
		t.Fatalf("Settle must bill with the PASSED table: c1=%d c2=%d", c1, c2)
	}
	// nil table → cost 0, pricing missing (never panics).
	if c, miss := g.Settle("t", "", KeyPolicy{}, "p", "m", pricing.Usage{Input: 1000}, nil); c != 0 || !miss {
		t.Fatalf("nil table: cost=%d missing=%v, want 0,true", c, miss)
	}
}

// --- per-key budget/TPM/RPM (§8 D2) ---

func TestGovernorKeyRPMBlocksIndependentlyOfTeam(t *testing.T) {
	g := NewGovernor(nil, limiter.NewMemory(), budget.NewMemory(), nil) // ungoverned team
	kp := KeyPolicy{RatePerMin: 1}
	if d := g.PreCheck("t", "k1", kp, 0); !d.Allowed {
		t.Fatalf("first request under key RPM=1 must pass: %+v", d)
	}
	if d := g.PreCheck("t", "k1", kp, 0); d.Allowed {
		t.Fatalf("second request must block on the key's RPM limit: %+v", d)
	}
	if d := g.PreCheck("t", "k1", kp, 0); d.Status != 429 {
		t.Fatalf("key rate block status = %d, want 429", d.Status)
	}
	// A different key on the same team has its own independent bucket.
	if d := g.PreCheck("t", "k2", kp, 0); !d.Allowed {
		t.Fatalf("a different key's bucket must be independent: %+v", d)
	}
}

func TestGovernorKeyTPMBlocks(t *testing.T) {
	g := NewGovernor(nil, limiter.NewMemory(), budget.NewMemory(), nil)
	kp := KeyPolicy{TokensPerMinute: 1000}
	if d := g.PreCheck("t", "k1", kp, 800); !d.Allowed {
		t.Fatalf("first: %+v", d)
	}
	if d := g.PreCheck("t", "k1", kp, 800); d.Allowed {
		t.Fatalf("key TPM should block second: %+v", d)
	}
}

func TestGovernorKeyBudgetBlocksAndDebitsIndependentlyOfTeam(t *testing.T) {
	// The team itself carries NO budget policy — proves a key limit is
	// enforced even for an otherwise-ungoverned team.
	g := NewGovernor(nil, limiter.NewMemory(), budget.NewMemory(), nil)
	kp := KeyPolicy{BudgetMicrosPerMonth: 1_000_000}
	tbl := testTable()
	g.Settle("t", "k1", kp, "p", "m", pricing.Usage{Input: 900_000}, tbl) // 900k µUSD spent
	if d := g.PreCheck("t", "k1", kp, 0); !d.Allowed {
		t.Fatalf("900k < 1M cap should still allow: %+v", d)
	}
	g.Settle("t", "k1", kp, "p", "m", pricing.Usage{Input: 200_000}, tbl) // 1.1M > cap
	if d := g.PreCheck("t", "k1", kp, 0); d.Allowed {
		t.Fatal("key budget counter did not accumulate — should block over cap")
	}
	if d := g.PreCheck("t", "k1", kp, 0); d.Status != 402 {
		t.Fatalf("key budget block status = %d, want 402", d.Status)
	}
	// A different key is unaffected by k1's spend.
	if d := g.PreCheck("t", "k2", kp, 0); !d.Allowed {
		t.Fatalf("a different key's budget must be independent: %+v", d)
	}
}

func TestGovernorKeyAndTeamPoliciesBothApply(t *testing.T) {
	// The tighter of the two limits governs: team allows, key blocks.
	teams := map[string]TeamPolicy{"t": {BudgetMicrosPerMonth: 10_000_000, BudgetExceeded: "block"}}
	g := NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), nil)
	kp := KeyPolicy{BudgetMicrosPerMonth: 100_000}
	tbl := testTable()
	g.Settle("t", "k1", kp, "p", "m", pricing.Usage{Input: 150_000}, tbl) // key: 150k > 100k cap; team: nowhere near its 10M cap
	if d := g.PreCheck("t", "k1", kp, 0); d.Allowed {
		t.Fatal("key's tighter budget must block even though the team policy alone would allow")
	}
}

func TestGovernorCountersIndependentOfTable(t *testing.T) {
	// The budget counter is the governor's own state; which table is passed
	// does not rebuild or reset it — spend accumulates across tables.
	teams := map[string]TeamPolicy{"t": {BudgetMicrosPerMonth: 1_000_000, BudgetExceeded: "block"}}
	g := NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), nil)
	tbl := pricing.New(pricing.OnMissingAllow, map[pricing.Key]pricing.Rate{{Provider: "p", Model: "m"}: {InputPerMTok: 1_000_000}})
	g.Settle("t", "", KeyPolicy{}, "p", "m", pricing.Usage{Input: 400_000}, tbl) // 400k µUSD
	g.Settle("t", "", KeyPolicy{}, "p", "m", pricing.Usage{Input: 400_000}, tbl) // +400k = 800k
	// 800k < 1M cap → still allowed (counter accumulated, not yet over).
	if d := g.PreCheck("t", "", KeyPolicy{}, 0); !d.Allowed {
		t.Fatalf("800k < 1M cap should still allow: %+v", d)
	}
	g.Settle("t", "", KeyPolicy{}, "p", "m", pricing.Usage{Input: 400_000}, tbl) // 1.2M > cap
	if d := g.PreCheck("t", "", KeyPolicy{}, 0); d.Allowed {
		t.Fatal("budget counter did not accumulate across Settle calls (table-independent state expected)")
	}
}

func TestGovernorSettleBudgetNotify(t *testing.T) {
	teams := map[string]TeamPolicy{"t": {BudgetMicrosPerMonth: 1_000_000, BudgetExceeded: "block"}}
	g := NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), nil)
	var gotTeam string
	var gotSpent, gotLimit int64
	calls := 0
	g.SetBudgetNotify(func(team string, spent, limit int64) {
		calls++
		gotTeam, gotSpent, gotLimit = team, spent, limit
	})
	tbl := pricing.New(pricing.OnMissingAllow, map[pricing.Key]pricing.Rate{{Provider: "p", Model: "m"}: {InputPerMTok: 1_000_000}})
	g.Settle("t", "", KeyPolicy{}, "p", "m", pricing.Usage{Input: 400_000}, tbl) // 400k µUSD

	if calls != 1 {
		t.Fatalf("expected exactly one notify call, got %d", calls)
	}
	if gotTeam != "t" || gotSpent != 400_000 || gotLimit != 1_000_000 {
		t.Fatalf("notify(team=%q, spent=%d, limit=%d), want (t, 400000, 1000000)", gotTeam, gotSpent, gotLimit)
	}
}

func TestGovernorSettleBudgetNotify_UnbudgetedTeamSkipped(t *testing.T) {
	teams := map[string]TeamPolicy{"t": {}} // no budget configured
	g := NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), nil)
	calls := 0
	g.SetBudgetNotify(func(string, int64, int64) { calls++ })
	tbl := testTable()
	g.Settle("t", "", KeyPolicy{}, "p", "m", pricing.Usage{Input: 400_000}, tbl)
	if calls != 0 {
		t.Fatalf("unbudgeted team must not invoke the budget-notify hook, got %d calls", calls)
	}
}

func TestGovernorSettleBudgetNotify_KeyBudgetExcluded(t *testing.T) {
	// Per-key budgets must never reach the notify hook: key_id can't be a
	// metric/alert label (CLAUDE.md).
	teams := map[string]TeamPolicy{"t": {}} // no TEAM budget
	g := NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), nil)
	calls := 0
	g.SetBudgetNotify(func(string, int64, int64) { calls++ })
	kp := KeyPolicy{BudgetMicrosPerMonth: 1_000_000}
	tbl := testTable()
	g.Settle("t", "k1", kp, "p", "m", pricing.Usage{Input: 400_000}, tbl)
	if calls != 0 {
		t.Fatalf("key-budget debit must not invoke the team budget-notify hook, got %d calls", calls)
	}
}

func TestGovernorSettleSetsBudgetUtilizationGauge(t *testing.T) {
	m := metrics.New()
	teams := map[string]TeamPolicy{"t": {BudgetMicrosPerMonth: 1_000_000, BudgetExceeded: "block"}}
	g := NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), m)
	tbl := testTable()
	g.Settle("t", "", KeyPolicy{}, "p", "m", pricing.Usage{Input: 400_000}, tbl)
	if got, err := testutil.GatherAndCount(m.Registry(), "inferplane_budget_utilization_ratio"); err != nil || got == 0 {
		t.Fatalf("budget_utilization_ratio not recorded (count=%d err=%v)", got, err)
	}
}

// Task 3: UsageOf reports the caller's effective governance state read-only.
// F2: an unlimited dimension is nil, never remaining:0. F3: both team and key
// budget are reported.
func TestGovernorUsageOf(t *testing.T) {
	teams := map[string]TeamPolicy{
		"t": {BudgetMicrosPerMonth: 1_000_000, TokensPerDay: 10_000},
	}
	g := NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), nil)
	// spend some team budget + quota, and some per-key budget.
	kp := KeyPolicy{BudgetMicrosPerMonth: 500_000}
	// Settle debits both budget (1500 µUSD, team+key) AND the team's token
	// quota (Input+Output=1500 tokens, since TokensPerDay>0 — governance.go's
	// Settle debits "quota:"+team whenever the team has a daily token limit).
	// The extra explicit DebitQuota below simulates a second request that
	// only consumed quota (no billable cost), so team quota ends at 1500+2000.
	g.Settle("t", "ik1", kp, "p", "m", pricing.Usage{Input: 1000, Output: 500}, testTable()) // 1500 µUSD both team+key
	g.lim.DebitQuota("quota:t", 2000, 24*time.Hour)

	u := g.UsageOf("t", "ik1", kp)
	if u.TeamBudget == nil {
		t.Fatal("team budget must be reported when limited")
	}
	if u.TeamBudget.LimitUSDMicros != 1_000_000 || u.TeamBudget.SpentUSDMicros != 1500 {
		t.Fatalf("team budget wrong: %+v", u.TeamBudget)
	}
	if u.TeamBudget.RemainingUSDMicros != 1_000_000-1500 {
		t.Fatalf("remaining wrong: %+v", u.TeamBudget)
	}
	if u.KeyBudget == nil || u.KeyBudget.LimitUSDMicros != 500_000 || u.KeyBudget.SpentUSDMicros != 1500 {
		t.Fatalf("key budget wrong: %+v", u.KeyBudget)
	}
	if u.TeamQuota == nil || u.TeamQuota.UsedTokens != 3500 {
		t.Fatalf("team quota wrong: %+v", u.TeamQuota)
	}

	// F2: an unlimited team → nil budget/quota, not zero.
	g2 := NewGovernor(map[string]TeamPolicy{"t": {}}, limiter.NewMemory(), budget.NewMemory(), nil)
	u2 := g2.UsageOf("t", "", KeyPolicy{})
	if u2.TeamBudget != nil || u2.TeamQuota != nil || u2.KeyBudget != nil {
		t.Fatalf("unlimited dimensions must be nil, got %+v", u2)
	}
}
