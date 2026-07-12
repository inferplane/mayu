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

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/metrics"
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

// KeyPolicy is the resolved per-key governance limits (a virtual key's
// optional budget/TPM/RPM fields, keystore.KeyOptions — this package stays a
// leaf and does not import keystore). Zero in any field means unlimited for
// that dimension. Key limits are layered ON TOP OF the team policy: both must
// allow, and they apply even when the key's team carries no TeamPolicy entry
// (an ungoverned team must not bypass an explicit key limit). There is no
// on_exceeded knob here (KeyOptions has none) — a key limit always blocks.
// There is also no per-key RateBurst knob (unlike TeamPolicy): RatePerMin's
// own value is used as its bucket's burst too, same self-burst shape as
// TokensPerMinute below — a key with rpm:60 can burst all 60 in the first
// second, not one-at-a-time like a team's default burst=1. Intentional and
// simplest given KeyOptions exposes no separate burst field.
type KeyPolicy struct {
	RatePerMin           int64
	TokensPerMinute      int64
	BudgetMicrosPerMonth int64
}

type BudgetUsage struct {
	LimitUSDMicros     int64  `json:"limit_usd_micros"`
	SpentUSDMicros     int64  `json:"spent_usd_micros"`
	RemainingUSDMicros int64  `json:"remaining_usd_micros"`
	Window             string `json:"window"`
}

type QuotaUsage struct {
	LimitTokens int64  `json:"limit_tokens"`
	UsedTokens  int64  `json:"used_tokens"`
	Window      string `json:"window"`
}

type UsageStatus struct {
	Team       string       `json:"team"`
	TeamBudget *BudgetUsage `json:"team_budget,omitempty"`
	TeamQuota  *QuotaUsage  `json:"team_quota,omitempty"`
	KeyBudget  *BudgetUsage `json:"key_budget,omitempty"`
	KeyQuota   *QuotaUsage  `json:"key_quota,omitempty"`
}

// Governor enforces rate/quota/budget and settles cost. Its stateful stores
// (limiter rate buckets, budget µUSD counters) are owned here and PERSIST
// across config hot-reloads. The pricing table is NOT stored — it is a
// reloadable lookup, so Settle takes it as a parameter (the handler passes the
// table from the live.State it resolved against), keeping this package a leaf
// (no live/config import) and billing a request on the same generation it
// resolved on (ADR-006).
type Governor struct {
	teams        map[string]TeamPolicy
	lookup       func(team string) (TeamPolicy, bool)              // D3/ADR-016: optional dynamic override, checked before teams
	notifyBudget func(team string, spentMicros, limitMicros int64) // D5b/ADR-017: optional budget-alert hook, called after each team debit
	lim          limiter.LimiterStore
	bud          budget.BudgetStore
	metrics      *metrics.Metrics // nil-safe: no-op when nil
}

// NewGovernor builds the Governor. m is the Prometheus metrics sink for
// budget_spend / pricing_miss; pass nil to disable metrics (unit tests).
func NewGovernor(teams map[string]TeamPolicy, lim limiter.LimiterStore, bud budget.BudgetStore, m *metrics.Metrics) *Governor {
	return &Governor{teams: teams, lim: lim, bud: bud, metrics: m}
}

// SetTeamLookup installs a dynamic team-policy source (D3, ADR-016) — e.g. a
// keystore team-record lookup — consulted on every PreCheck/Settle BEFORE the
// static config map, so editing a team's budget/limits in the admin console
// takes effect on the very next request: no restart, no hot-reload. Passing
// nil (the default) reproduces today's config-only behavior exactly. The
// lookup's second return value distinguishes "no record for this team" (fall
// through to config) from a real hit; SetTeamLookup itself makes no I/O call.
func (g *Governor) SetTeamLookup(f func(team string) (TeamPolicy, bool)) {
	g.lookup = f
}

// SetBudgetNotify installs a budget-alert hook (D5b, ADR-017): called from
// Settle, after every team-budget debit, with the post-debit spend and the
// team's configured limit. Scoped to team budgets only — per-key budgets are
// not observed here (a key_id must never become a metric/alert label,
// CLAUDE.md). Like SetTeamLookup, this is a startup-only assignment with no
// synchronization; passing nil (the default) disables alerting.
func (g *Governor) SetBudgetNotify(f func(team string, spentMicros, limitMicros int64)) {
	g.notifyBudget = f
}

// policyOf resolves a team's policy: a dynamic-lookup hit wins over a config
// entry of the same name (ADR-016 precedence — an admin console edit must not
// be silently shadowed by the config file); a team present in neither is
// ungoverned (ok=false).
func (g *Governor) policyOf(team string) (TeamPolicy, bool) {
	if g.lookup != nil {
		if p, ok := g.lookup(team); ok {
			return p, true
		}
	}
	p, ok := g.teams[team]
	return p, ok
}

// PricingVersionOf returns the rate table version for the audit CostRef,
// nil-safe.
func PricingVersionOf(table *pricing.Table) string {
	if table == nil {
		return ""
	}
	return table.Version
}

// GovDecision is the PreCheck verdict. Status is the HTTP status to return when
// !Allowed: 429 (rate/quota), 402 (budget), 0 (allowed).
type GovDecision struct {
	Allowed bool
	Status  int
	Reason  string
	Code    audit.DenyReason
}

// PreCheck enforces rate limit + quota + budget BEFORE the upstream call.
// estimateTokens is the request's estimated input tokens. block policy → deny;
// warn policy → allow (still settled afterward). An unknown team is ungoverned
// (but a key limit, if any, still applies — see KeyPolicy). keyID scopes the
// key-level counters; pass "" with a zero KeyPolicy when there is none.
func (g *Governor) PreCheck(team, keyID string, kp KeyPolicy, estimateTokens int64) GovDecision {
	if p, ok := g.policyOf(team); ok {
		// rate limit (RPM): 1 request unit
		if p.RatePerMin > 0 && !g.lim.AllowRate("rate:"+team, 1, p.RatePerMin, max64(p.RateBurst, 1)) {
			return GovDecision{Status: 429, Reason: "rate limit exceeded", Code: audit.DenyTeamRateLimited}
		}
		// token rate limit (TPM): charge the request estimate against a per-minute
		// token bucket whose burst is one minute's worth of tokens.
		if p.TokensPerMinute > 0 && !g.lim.AllowRate("tpm:"+team, estimateTokens, p.TokensPerMinute, p.TokensPerMinute) {
			return GovDecision{Status: 429, Reason: "token rate limit exceeded", Code: audit.DenyTeamTokenRateLimited}
		}
		// quota (daily tokens)
		if p.TokensPerDay > 0 {
			if g.lim.CheckQuota("quota:"+team, estimateTokens, p.TokensPerDay, 24*time.Hour) == limiter.Block {
				if p.QuotaExceeded != "warn" {
					return GovDecision{Status: 429, Reason: "token quota exceeded", Code: audit.DenyTeamQuotaExceeded}
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
					return GovDecision{Status: 402, Reason: "budget exceeded", Code: audit.DenyTeamBudgetExceeded}
				}
			}
		}
	}
	// Per-key limits (§8 D2) — independent of team governance, always block.
	if kp.RatePerMin > 0 && !g.lim.AllowRate("rate:key:"+keyID, 1, kp.RatePerMin, kp.RatePerMin) {
		return GovDecision{Status: 429, Reason: "key rate limit exceeded", Code: audit.DenyKeyRateLimited}
	}
	if kp.TokensPerMinute > 0 && !g.lim.AllowRate("tpm:key:"+keyID, estimateTokens, kp.TokensPerMinute, kp.TokensPerMinute) {
		return GovDecision{Status: 429, Reason: "key token rate limit exceeded", Code: audit.DenyKeyTokenRateLimited}
	}
	if kp.BudgetMicrosPerMonth > 0 && g.bud.Check("budget:key:"+keyID, 0, kp.BudgetMicrosPerMonth, 30*24*time.Hour) == budget.Block {
		return GovDecision{Status: 402, Reason: "key budget exceeded", Code: audit.DenyKeyBudgetExceeded}
	}
	return GovDecision{Allowed: true}
}

func (g *Governor) UsageOf(team, keyID string, kp KeyPolicy) UsageStatus {
	u := UsageStatus{Team: team}
	if p, ok := g.policyOf(team); ok {
		if p.BudgetMicrosPerMonth > 0 {
			spent := g.bud.Spent("budget:"+team, 30*24*time.Hour)
			u.TeamBudget = &BudgetUsage{
				LimitUSDMicros:     p.BudgetMicrosPerMonth,
				SpentUSDMicros:     spent,
				RemainingUSDMicros: max64(0, p.BudgetMicrosPerMonth-spent),
				Window:             "720h",
			}
		}
		if p.TokensPerDay > 0 {
			u.TeamQuota = &QuotaUsage{
				LimitTokens: p.TokensPerDay,
				UsedTokens:  g.lim.QuotaUsed("quota:"+team, 24*time.Hour),
				Window:      "24h",
			}
		}
	}
	if kp.BudgetMicrosPerMonth > 0 {
		spent := g.bud.Spent("budget:key:"+keyID, 30*24*time.Hour)
		u.KeyBudget = &BudgetUsage{
			LimitUSDMicros:     kp.BudgetMicrosPerMonth,
			SpentUSDMicros:     spent,
			RemainingUSDMicros: max64(0, kp.BudgetMicrosPerMonth-spent),
			Window:             "720h",
		}
	}
	if kp.TokensPerMinute > 0 {
		// Same bucket key/burst PreCheck debits ("tpm:key:"+keyID) — RateUsed
		// only peeks at the projected refill, never writes it back.
		used := g.lim.RateUsed("tpm:key:"+keyID, kp.TokensPerMinute, kp.TokensPerMinute)
		u.KeyQuota = &QuotaUsage{
			LimitTokens: kp.TokensPerMinute,
			UsedTokens:  used,
			Window:      "1m",
		}
	}
	return u
}

// Settle records actual token usage against quota and computes+debits cost.
// Returns the cost µUSD and whether pricing was missing (for the audit record).
// An unknown team still computes cost (so the audit record carries it) but
// debits nothing. keyID/kp mirror PreCheck's per-key budget (RPM/TPM are
// charge-on-check only, like the team dimension — nothing to debit here).
// Key-level spend is deliberately NOT added to /metrics: metric labels are
// config-bounded (CLAUDE.md) and must never carry a key_id.
func (g *Governor) Settle(team, keyID string, kp KeyPolicy, provider, model string, u pricing.Usage, table *pricing.Table) (costMicros int64, pricingMissing bool) {
	p, _ := g.policyOf(team)
	if p.TokensPerDay > 0 {
		g.lim.DebitQuota("quota:"+team, u.Input+u.Output, 24*time.Hour)
		// Reflect the post-debit daily quota utilization into the gauge (0..1).
		used := g.lim.QuotaUsed("quota:"+team, 24*time.Hour)
		g.metrics.SetQuotaUtilization(team, "day", float64(used)/float64(p.TokensPerDay))
	}
	if table == nil {
		costMicros, pricingMissing = 0, true
	} else {
		costMicros, pricingMissing = table.CostUSDMicros(provider, model, u)
	}
	if p.BudgetMicrosPerMonth > 0 {
		g.bud.Debit("budget:"+team, costMicros, 30*24*time.Hour)
		// Debit and Spent are each individually mutex-protected but not one
		// atomic operation: a concurrent Settle for the same team can debit
		// between these two calls. Under concurrent load this can make the
		// read-back spend already reflect a later request's debit too,
		// skipping an intermediate alert threshold (the Notifier still fires
		// the highest crossed one — no double-fire, just a possible skip of
		// an earlier one). A tighter-scoped case of the per-instance/replica
		// approximation ADR-017 §8 documents; ponytail: add
		// BudgetStore.DebitAndRead if this needs to be exact.
		spent := g.bud.Spent("budget:"+team, 30*24*time.Hour)
		g.metrics.SetBudgetUtilization(team, float64(spent)/float64(p.BudgetMicrosPerMonth))
		if g.notifyBudget != nil {
			g.notifyBudget(team, spent, p.BudgetMicrosPerMonth)
		}
	}
	if kp.BudgetMicrosPerMonth > 0 {
		g.bud.Debit("budget:key:"+keyID, costMicros, 30*24*time.Hour)
	}
	// Observability metrics (approximation; the µUSD budget store is the
	// settlement source of truth). Recorded for every settled request, even an
	// ungoverned team, so /metrics reflects all traffic.
	g.metrics.AddBudgetSpend(team, model, "total", float64(costMicros)/1e6)
	if pricingMissing {
		g.metrics.IncPricingMiss(provider, model)
	}
	return costMicros, pricingMissing
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
