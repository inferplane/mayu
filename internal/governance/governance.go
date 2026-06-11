// Package governance is the ingress-shared governance pipeline: a Governor that
// enforces rate limit + quota + budget BEFORE the upstream call (PreCheck) and
// settles actual token usage + cost AFTER the call (Settle). It lives in its
// own package (not internal/server) so both the Anthropic and OpenAI ingress
// handlers can import it without an import cycle (internal/server already
// imports the ingress packages). Stores are swappable interfaces; M5 ships
// in-memory (per-instance), Redis is v0.2.
package governance

import (
	"time"

	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/pricing"
)

// TeamPolicy is the resolved per-team governance limits. Zero in any field
// means "unlimited" for that dimension. QuotaExceeded/BudgetExceeded select
// block|warn (warn admits the request and still settles afterward).
type TeamPolicy struct {
	RatePerMin           int64
	RateBurst            int64
	TokensPerMinute      int64
	TokensPerDay         int64
	QuotaExceeded        string // block|warn
	BudgetMicrosPerMonth int64
	BudgetExceeded       string
}

type Governor struct {
	teams map[string]TeamPolicy
	lim   limiter.LimiterStore
	bud   budget.BudgetStore
	price *pricing.Table
}

func NewGovernor(teams map[string]TeamPolicy, lim limiter.LimiterStore, bud budget.BudgetStore, price *pricing.Table) *Governor {
	return &Governor{teams: teams, lim: lim, bud: bud, price: price}
}

// GovDecision is the PreCheck verdict. Status is the HTTP status to return when
// !Allowed: 429 (rate/quota), 402 (budget), 0 (allowed).
type GovDecision struct {
	Allowed bool
	Status  int
	Reason  string
}

// PricingVersion exposes the rate table version for the audit CostRef.
func (g *Governor) PricingVersion() string {
	if g.price == nil {
		return ""
	}
	return g.price.Version
}

// PreCheck enforces rate limit + quota + budget BEFORE the upstream call.
// estimateTokens is the request's estimated input tokens. block policy → deny;
// warn policy → allow (still settled afterward). An unknown team is ungoverned.
func (g *Governor) PreCheck(team string, estimateTokens int64) GovDecision {
	p, ok := g.teams[team]
	if !ok {
		return GovDecision{Allowed: true}
	}
	// rate limit (RPM): 1 request unit
	if p.RatePerMin > 0 && !g.lim.AllowRate("rate:"+team, 1, p.RatePerMin, max64(p.RateBurst, 1)) {
		return GovDecision{Status: 429, Reason: "rate limit exceeded"}
	}
	// token rate limit (TPM): charge the request estimate against a per-minute
	// token bucket whose burst is one minute's worth of tokens.
	if p.TokensPerMinute > 0 && !g.lim.AllowRate("tpm:"+team, estimateTokens, p.TokensPerMinute, p.TokensPerMinute) {
		return GovDecision{Status: 429, Reason: "token rate limit exceeded"}
	}
	// quota (daily tokens)
	if p.TokensPerDay > 0 {
		if g.lim.CheckQuota("quota:"+team, estimateTokens, p.TokensPerDay, 24*time.Hour) == limiter.Block {
			if p.QuotaExceeded != "warn" {
				return GovDecision{Status: 429, Reason: "token quota exceeded"}
			}
		}
	}
	// budget (monthly µUSD) — pre-check on accumulated spend only (estimate 0),
	// because the per-request cost is unknown before the call. Real enforcement
	// is the post-debit threshold; a single high-cost request can overshoot
	// (accepted per §5.3).
	if p.BudgetMicrosPerMonth > 0 {
		if g.bud.Check("budget:"+team, 0, p.BudgetMicrosPerMonth, 30*24*time.Hour) == budget.Block {
			if p.BudgetExceeded != "warn" {
				return GovDecision{Status: 402, Reason: "budget exceeded"}
			}
		}
	}
	return GovDecision{Allowed: true}
}

// Settle records actual token usage against quota and computes+debits cost.
// Returns the cost µUSD and whether pricing was missing (for the audit record).
// An unknown team still computes cost (so the audit record carries it) but
// debits nothing.
func (g *Governor) Settle(team, provider, model string, u pricing.Usage) (costMicros int64, pricingMissing bool) {
	p := g.teams[team]
	if p.TokensPerDay > 0 {
		g.lim.DebitQuota("quota:"+team, u.Input+u.Output, 24*time.Hour)
	}
	costMicros, pricingMissing = g.price.CostUSDMicros(provider, model, u)
	if p.BudgetMicrosPerMonth > 0 {
		g.bud.Debit("budget:"+team, costMicros, 30*24*time.Hour)
	}
	return costMicros, pricingMissing
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
