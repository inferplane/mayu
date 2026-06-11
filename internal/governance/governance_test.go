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
	return NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(),
		pricing.New(pricing.OnMissingAllow, map[pricing.Key]pricing.Rate{{Provider: "p", Model: "m"}: {InputPerMTok: 1_000_000, OutputPerMTok: 1_000_000}}), nil)
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
	cost, missing := g.Settle("t", "p", "m", pricing.Usage{Input: 1000, Output: 500})
	// 1000*1 + 500*1 = 1500 µUSD
	if missing || cost != 1500 {
		t.Fatalf("settle cost=%d missing=%v", cost, missing)
	}
}

func TestGovernorSettleRecordsMetrics(t *testing.T) {
	m := metrics.New()
	teams := map[string]TeamPolicy{"t": {}}
	// Known rate for (p,m); unknown for (p,ghost) → pricing_miss.
	g := NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(),
		pricing.New(pricing.OnMissingAllow, map[pricing.Key]pricing.Rate{
			{Provider: "p", Model: "m"}: {InputPerMTok: 1_000_000, OutputPerMTok: 1_000_000},
		}), m)

	g.Settle("t", "p", "m", pricing.Usage{Input: 1000, Output: 500}) // 1500 µUSD → 0.0015 USD
	if got, err := testutil.GatherAndCount(m.Registry(), "inferplane_budget_spend_usd_total"); err != nil || got == 0 {
		t.Fatalf("budget_spend not recorded (count=%d err=%v)", got, err)
	}

	g.Settle("t", "p", "ghost", pricing.Usage{Input: 10}) // no rate → pricing_miss
	if got, err := testutil.GatherAndCount(m.Registry(), "inferplane_pricing_miss_total"); err != nil || got == 0 {
		t.Fatalf("pricing_miss not recorded for unknown (provider,model) (count=%d err=%v)", got, err)
	}
}

func TestGovernorTPMBlocks(t *testing.T) {
	teams := map[string]TeamPolicy{"t": {TokensPerMinute: 1000}}
	g := NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), pricing.New(pricing.OnMissingAllow, nil), nil)
	// first request estimate 800 → ok; second estimate 800 → 1600>1000 burst → block
	if d := g.PreCheck("t", 800); !d.Allowed {
		t.Fatalf("first: %+v", d)
	}
	if d := g.PreCheck("t", 800); d.Allowed {
		t.Fatalf("TPM should block second: %+v", d)
	}
}
