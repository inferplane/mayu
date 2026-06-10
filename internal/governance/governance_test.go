package governance

import (
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/pricing"
)

func testGovernor() *Governor {
	teams := map[string]TeamPolicy{
		"t": {RatePerMin: 60, RateBurst: 2, TokensPerDay: 1000, QuotaExceeded: "block", BudgetMicrosPerMonth: 0, BudgetExceeded: "block"},
	}
	return NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(),
		pricing.New(pricing.OnMissingAllow, map[pricing.Key]pricing.Rate{{Provider: "p", Model: "m"}: {InputPerMTok: 1_000_000, OutputPerMTok: 1_000_000}}))
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
