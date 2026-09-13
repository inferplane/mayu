// Package governance is the ingress-shared governance pipeline: a Governor that
// enforces rate limit + quota + budget BEFORE the upstream call (PreCheck) and
// settles actual token usage + cost AFTER the call (Settle). It lives in its
// own package (not internal/server) so both the Anthropic and OpenAI ingress
// handlers can import it without an import cycle (internal/server already
// imports the ingress packages). Stores are swappable interfaces; M5 ships
// in-memory (per-instance), Redis is v0.2.
package governance

import (
	"fmt"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/pricing"
)

// Subject identifies WHO a governance decision is about: the team that owns
// the virtual key, the key itself, and — new in ADR-042 Phase 3 — the
// individual the key was issued to (keystore.Principal.Owner, which for a
// CLI-issued key is the OIDC "sub"). User is "" when unset, which is the
// pre-Phase-3 behaviour exactly: no user lookup, no user counter, no change.
//
// It replaced two leading positional string arguments on PreCheck/Settle/
// UsageOf. A value, not a pointer, and this package stays a leaf — the
// ingress packages build it from keystore.Principal themselves (subjectOf),
// so governance never imports keystore.
type Subject struct {
	Team  string
	KeyID string
	User  string
}

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
	// BudgetMicrosPerDay/BudgetDayExceeded are the CALENDAR-DAY counterpart of
	// the two fields above, enforced against a separate counter with its own
	// store key (budget.Key's window tag). Both windows apply at once: a team
	// can be capped at $50/day AND $1000/month, and either can deny. Zero =
	// not limited on this dimension, same as the monthly pair.
	BudgetMicrosPerDay int64
	BudgetDayExceeded  string // block|warn
	// AdminContact is surfaced verbatim in a 402 team-budget-exceeded
	// response, when set (see policy.TeamLimits.AdminContact).
	AdminContact string
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
	// BudgetMicrosPerDay is the calendar-day counterpart. Like every per-key
	// limit it always blocks — KeyOptions carries no on_exceeded knob, so
	// there is deliberately no BudgetDayExceeded here.
	BudgetMicrosPerDay int64
}

// UserPolicy is the resolved per-USER budget limits, sourced from a
// GovernancePolicy document only (there is no config key and no keystore
// column for a user's cap — a second source of truth for one human's budget
// is what ADR-042 refuses). Zero in either amount means unlimited on that
// window.
//
// There are deliberately NO RPM/TPM fields: rate stays team-only, because a
// per-user rate limit needs a rate SHARE model this phase does not build, and
// policy.checkEnforceable still refuses a user-subject rate rule. Do not add
// them "for symmetry".
//
// There is also deliberately ONE BudgetExceeded knob covering BOTH windows,
// unlike TeamPolicy's BudgetExceeded/BudgetDayExceeded pair. The two windows
// are still two independent policy rules with their own hardCap; the caller
// (cmd/mayu/gateway.go) collapses them block-wins-on-tie before building this
// value. Do not add a BudgetDayExceeded field.
type UserPolicy struct {
	BudgetMicrosPerMonth int64
	BudgetMicrosPerDay   int64
	BudgetExceeded       string // block|warn
	// AdminContact is surfaced verbatim in the 402 body, same as
	// TeamPolicy.AdminContact.
	AdminContact string
}

type BudgetUsage struct {
	LimitUSDMicros     int64     `json:"limit_usd_micros"`
	SpentUSDMicros     int64     `json:"spent_usd_micros"`
	RemainingUSDMicros int64     `json:"remaining_usd_micros"`
	Window             string    `json:"window"`
	ResetsAt           time.Time `json:"resets_at"`
}

type QuotaUsage struct {
	LimitTokens int64  `json:"limit_tokens"`
	UsedTokens  int64  `json:"used_tokens"`
	Window      string `json:"window"`
}

type UsageStatus struct {
	EnforcementMode string        `json:"enforcement_mode,omitempty"`
	SharedLimits    []SharedLimit `json:"shared_limits,omitempty"`
	Team            string        `json:"team"`
	TeamBudget      *BudgetUsage  `json:"team_budget,omitempty"`
	TeamQuota       *QuotaUsage   `json:"team_quota,omitempty"`
	KeyBudget       *BudgetUsage  `json:"key_budget,omitempty"`
	KeyQuota        *QuotaUsage   `json:"key_quota,omitempty"`
	// TeamBudgetDay/KeyBudgetDay report the calendar-DAY counters. Appended,
	// never folded into the fields above: repurposing team_budget's window
	// string would silently change what every existing /v1/usage client
	// believes it is reading.
	TeamBudgetDay *BudgetUsage `json:"team_budget_day,omitempty"`
	KeyBudgetDay  *BudgetUsage `json:"key_budget_day,omitempty"`
	// UserBudget/UserBudgetDay report the per-USER counters (ADR-042 Phase 3),
	// present only when Subject.User is set AND a UserPolicy was found.
	// Appended with omitempty for the same reason the day fields were: an
	// existing /v1/usage client must keep reading exactly what it read before.
	UserBudget    *BudgetUsage `json:"user_budget,omitempty"`
	UserBudgetDay *BudgetUsage `json:"user_budget_day,omitempty"`
}

// Governor enforces rate/quota/budget and settles cost. Its stateful stores
// (limiter rate buckets, budget µUSD counters) are owned here and PERSIST
// across config hot-reloads. The pricing table is NOT stored — it is a
// reloadable lookup, so Settle takes it as a parameter (the handler passes the
// table from the live.State it resolved against), keeping this package a leaf
// (no live/config import) and billing a request on the same generation it
// resolved on (ADR-006).
type Governor struct {
	shared          SharedAuthority
	authority       BudgetAuthority
	teams           map[string]TeamPolicy
	lookup          func(team string) (TeamPolicy, bool)                     // D3/ADR-016: optional dynamic override, checked before teams
	userLookup      func(team, user string) (UserPolicy, bool)               // ADR-042 Phase 3: optional per-user budget source, consulted when Subject.User is set
	notifyBudget    func(team string, spentMicros, limitMicros int64)        // D5b/ADR-017: optional budget-alert hook, called after each team debit
	notifyKeyBudget func(team, keyID string, spentMicros, limitMicros int64) // D5b/ADR-017 per-key follow-up: optional per-key budget-alert hook, called after each key debit
	leaseGate       func(team string) (blocked bool, reason string)          // ADR-034: optional budget-lease check, consulted first in PreCheck
	lim             limiter.LimiterStore
	bud             budget.BudgetStore
	budgetLoc       *time.Location   // ADR-034-adjacent: budget_timezone; nil = UTC
	metrics         *metrics.Metrics // nil-safe: no-op when nil
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

// SetUserLookup installs a per-USER budget source (ADR-042 Phase 3),
// consulted on every PreCheck/Settle/UsageOf when Subject.User is non-empty.
// It mirrors SetTeamLookup: a startup-only assignment with no
// synchronization, and nil (the default) reproduces pre-Phase-3 behaviour
// byte for byte — no user lookup, no user counter, no user field in
// /v1/usage. The second return value distinguishes "no per-user rule for this
// (team, user)" from a real hit; SetUserLookup itself makes no I/O call.
func (g *Governor) SetUserLookup(f func(team, user string) (UserPolicy, bool)) {
	g.userLookup = f
}

// SetBudgetNotify installs a budget-alert hook (D5b, ADR-017): called from
// Settle, after every team-budget debit, with the post-debit spend and the
// team's configured limit. Scoped to team budgets only — per-key budgets are
// not observed by THIS hook or by /metrics (a key_id must never become a
// Prometheus label, CLAUDE.md) -- see SetKeyBudgetNotify for the dedicated
// per-key alert path. Like SetTeamLookup, this is a startup-only assignment
// with no synchronization; passing nil (the default) disables alerting.
func (g *Governor) SetBudgetNotify(f func(team string, spentMicros, limitMicros int64)) {
	g.notifyBudget = f
}

// SetKeyBudgetNotify installs a per-key budget-alert hook (ADR-017 per-key
// follow-up): called from Settle, after every key-budget debit, with the
// post-debit spend and the key's configured limit. This is the per-key
// ALERT path -- key_id reaching a webhook payload body is fine (unlike a
// Prometheus label; the /metrics cardinality rule this package's Settle
// comment cites is untouched -- /metrics still never carries key_id). Like
// SetTeamLookup/SetBudgetNotify, this is a startup-only assignment with no
// synchronization; passing nil (the default) disables per-key alerting.
func (g *Governor) SetKeyBudgetNotify(f func(team, keyID string, spentMicros, limitMicros int64)) {
	g.notifyKeyBudget = f
}

// SetLeaseGate installs a budget-lease check (ADR-034), consulted FIRST in
// PreCheck: when it reports blocked, the request is denied 402 with the
// gate's reason before any counter is charged. This is how an expired
// hard-cap lease fails closed — the data plane can no longer verify the
// global budget, so it must not serve. Like SetTeamLookup, a startup-only
// assignment with no synchronization; nil (the default) disables the gate.
func (g *Governor) SetLeaseGate(f func(team string) (blocked bool, reason string)) {
	g.leaseGate = f
}

// SetBudgetTimezone sets the timezone BOTH calendar budget windows — the day
// window's midnight and the month window's first-of-month boundary — anchor to
// (config budget_timezone). One anchor per deployment, so audit and billing
// reconciliation never straddle two different boundaries. nil (the default)
// means UTC, which is exactly what every budget window meant before this knob
// existed. Like SetTeamLookup this is a startup-only assignment with no
// synchronization: the window boundary of a counter that already exists does
// not move, so changing the operator timezone takes a restart by design.
func (g *Governor) SetBudgetTimezone(loc *time.Location) {
	g.budgetLoc = loc
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

// dayWindow is the calendar-day budget window, built fresh per call from the
// configured operator timezone. A method rather than a stored Window so a nil
// Loc keeps meaning UTC and no caller can mutate a shared value.
func (g *Governor) dayWindow() budget.Window {
	return budget.CalendarDayIn(g.budgetLoc)
}

// monthWindow is the calendar-month budget window, built fresh per call from the
// configured operator timezone — same posture and same reason as dayWindow.
func (g *Governor) monthWindow() budget.Window {
	return budget.CalendarMonthIn(g.budgetLoc)
}

// budgetLocOrUTC resolves the configured operator timezone for a 402 message's
// reset date, defaulting to UTC exactly as budget.Window does for a nil Loc.
func (g *Governor) budgetLocOrUTC() *time.Location {
	if g.budgetLoc == nil {
		return time.UTC
	}
	return g.budgetLoc
}

// userBudgetID is the id half of a user budget counter's store key:
// budget.Key(budget.ScopeUser, userBudgetID(s), w) → "budget:day:user:acme/sub-1".
//
// "/" is a safe separator because adminapi.validateTeamName forbids "/" in a
// team name (internal/server/adminapi/teams.go), so the split point is
// unambiguous no matter what a team or an OIDC sub contains.
//
// ACCEPTED LIMITATION: the counter is per (team, user), so a policy whose
// subject is user-ONLY (no team) caps that user separately in each team they
// hold a key in — one human in two teams gets two counters, i.e. up to 2x the
// stated cap. Keying on the bare user id instead would fix that and break
// something worse: a (team, user) rule and a user-only rule would then share
// one counter, so a team-scoped cap would be charged for spend made under a
// different team. Documented in ADR-042; do not "fix" it here.
func userBudgetID(s Subject) string { return s.Team + "/" + s.User }

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

// mustDenyBudget decides whether a budget.Check result should deny the
// request. budget.BlockCapacity (the store's at-capacity fail-safe) is
// UNCONDITIONAL — never downgraded by onExceeded — because a warn policy
// answers "should a real budget breach still admit the request?", not
// "should we serve a request the store can never account for?" (AI review,
// PR #64: a warn-configured budget hitting store capacity used to silently
// allow the request AND drop its spend, since a Debit on a key the store
// never admitted is a no-op). budget.Block (a real breach) still honors
// warn. A key-scoped budget passes onExceeded == "" here, which is never
// "warn", so its existing always-deny behavior on a real Block is unchanged.
func mustDenyBudget(dec budget.Decision, onExceeded string) bool {
	return dec == budget.BlockCapacity || (dec == budget.Block && onExceeded != "warn")
}

// PreCheck enforces rate limit + quota + budget BEFORE the upstream call.
// estimateTokens is the request's estimated input tokens. block policy → deny;
// warn policy → allow (still settled afterward). An unknown team is ungoverned
// (but a key limit, if any, still applies — see KeyPolicy). Subject.KeyID
// scopes the key-level counters; Subject.User, when set and a UserPolicy is
// found, adds a third scope (see below).
func (g *Governor) PreCheck(s Subject, kp KeyPolicy, estimateTokens int64) GovDecision {
	if g.HasSharedAuthority() {
		// Every resource is reserved atomically at the per-attempt seam, with
		// current DB policy and key/team metadata. Local buckets are not a
		// second authority and must neither double-charge nor loosen it.
		return GovDecision{Allowed: true}
	}
	// Budget-lease gate (ADR-034) first: an expired hard-cap lease means the
	// global budget can no longer be verified locally — fail closed before
	// charging any rate/quota counter.
	if g.leaseGate != nil {
		if blocked, reason := g.leaseGate(s.Team); blocked {
			return GovDecision{Status: 402, Reason: reason, Code: audit.DenyTeamBudgetExceeded}
		}
	}
	if p, ok := g.policyOf(s.Team); ok {
		// rate limit (RPM): 1 request unit
		if p.RatePerMin > 0 && !g.lim.AllowRate("rate:"+s.Team, 1, p.RatePerMin, max64(p.RateBurst, 1)) {
			return GovDecision{Status: 429, Reason: "rate limit exceeded", Code: audit.DenyTeamRateLimited}
		}
		// token rate limit (TPM): charge the request estimate against a per-minute
		// token bucket whose burst is one minute's worth of tokens.
		if p.TokensPerMinute > 0 && !g.lim.AllowRate("tpm:"+s.Team, estimateTokens, p.TokensPerMinute, p.TokensPerMinute) {
			return GovDecision{Status: 429, Reason: "token rate limit exceeded", Code: audit.DenyTeamTokenRateLimited}
		}
		// quota (daily tokens)
		if p.TokensPerDay > 0 {
			if g.lim.CheckQuota("quota:"+s.Team, estimateTokens, p.TokensPerDay, 24*time.Hour) == limiter.Block {
				if p.QuotaExceeded != "warn" {
					return GovDecision{Status: 429, Reason: "token quota exceeded", Code: audit.DenyTeamQuotaExceeded}
				}
			}
		}
		// budget (monthly µUSD) — pre-check on accumulated spend only (estimate 0),
		// because the per-request cost is unknown before the call. Real enforcement
		// is the post-debit threshold; a single high-cost request can overshoot
		// (accepted per §5.3).
		//
		// The daily window below is a SECOND, independent counter: its own store
		// key (budget.Key's window tag), its own limit, its own on_exceeded knob.
		// Both must allow. When both would deny we return whichever window resets
		// SOONEST, so the 402's reset date is the earliest the caller can actually
		// retry. Both windows share the operator timezone (budget_timezone), so in
		// one zone the next daily midnight is always ≤ the next month boundary —
		// the comparison stays anyway, because it derives the answer instead of
		// asserting an ordering that a future window kind could silently break.
		var budgetDeny GovDecision
		var budgetDenyResets time.Time
		if p.BudgetMicrosPerMonth > 0 {
			mw := g.monthWindow()
			tbk := budget.Key(budget.ScopeTeam, s.Team, mw)
			if dec := g.bud.Check(tbk, 0, p.BudgetMicrosPerMonth, mw); mustDenyBudget(dec, p.BudgetExceeded) {
				resetsAt := g.bud.ResetsAt(tbk, mw)
				budgetDeny = GovDecision{Status: 402, Reason: budgetExceededMessage("budget", resetsAt, g.budgetLocOrUTC(), p.AdminContact), Code: audit.DenyTeamBudgetExceeded}
				budgetDenyResets = resetsAt
			}
		}
		if p.BudgetMicrosPerDay > 0 {
			dw := g.dayWindow()
			tbkDay := budget.Key(budget.ScopeTeam, s.Team, dw)
			if dec := g.bud.Check(tbkDay, 0, p.BudgetMicrosPerDay, dw); mustDenyBudget(dec, p.BudgetDayExceeded) {
				resetsAt := g.bud.ResetsAt(tbkDay, dw)
				// Block wins on tie: keep whichever window binds soonest.
				if budgetDeny.Status == 0 || resetsAt.Before(budgetDenyResets) {
					budgetDeny = GovDecision{Status: 402, Reason: budgetExceededMessage("daily budget", resetsAt, g.budgetLocOrUTC(), p.AdminContact), Code: audit.DenyTeamBudgetExceeded}
					budgetDenyResets = resetsAt
				}
			}
		}
		if budgetDeny.Status != 0 {
			return budgetDeny
		}
	}
	// Per-key limits (§8 D2) — independent of team governance, always block.
	if kp.RatePerMin > 0 && !g.lim.AllowRate("rate:key:"+s.KeyID, 1, kp.RatePerMin, kp.RatePerMin) {
		return GovDecision{Status: 429, Reason: "key rate limit exceeded", Code: audit.DenyKeyRateLimited}
	}
	if kp.TokensPerMinute > 0 && !g.lim.AllowRate("tpm:key:"+s.KeyID, estimateTokens, kp.TokensPerMinute, kp.TokensPerMinute) {
		return GovDecision{Status: 429, Reason: "key token rate limit exceeded", Code: audit.DenyKeyTokenRateLimited}
	}
	// Per-key budget: same two-window shape as the team block above, minus the
	// on_exceeded knob (a key limit always blocks).
	var keyBudgetDeny GovDecision
	var keyBudgetDenyResets time.Time
	if kp.BudgetMicrosPerMonth > 0 {
		mw := g.monthWindow()
		kbk := budget.Key(budget.ScopeKey, s.KeyID, mw)
		if dec := g.bud.Check(kbk, 0, kp.BudgetMicrosPerMonth, mw); mustDenyBudget(dec, "") {
			resetsAt := g.bud.ResetsAt(kbk, mw)
			keyBudgetDeny = GovDecision{Status: 402, Reason: budgetExceededMessage("key budget", resetsAt, g.budgetLocOrUTC(), ""), Code: audit.DenyKeyBudgetExceeded}
			keyBudgetDenyResets = resetsAt
		}
	}
	if kp.BudgetMicrosPerDay > 0 {
		dw := g.dayWindow()
		kbkDay := budget.Key(budget.ScopeKey, s.KeyID, dw)
		if dec := g.bud.Check(kbkDay, 0, kp.BudgetMicrosPerDay, dw); mustDenyBudget(dec, "") {
			resetsAt := g.bud.ResetsAt(kbkDay, dw)
			if keyBudgetDeny.Status == 0 || resetsAt.Before(keyBudgetDenyResets) {
				keyBudgetDeny = GovDecision{Status: 402, Reason: budgetExceededMessage("key daily budget", resetsAt, g.budgetLocOrUTC(), ""), Code: audit.DenyKeyBudgetExceeded}
				keyBudgetDenyResets = resetsAt
			}
		}
	}
	if keyBudgetDeny.Status != 0 {
		return keyBudgetDeny
	}
	// Per-USER budget (ADR-042 Phase 3): the third scope, after team and key.
	// Budget only — no RPM/TPM, see UserPolicy. Same two-window shape as the
	// key block above, with UserPolicy's single on_exceeded knob governing
	// both windows, and the same "whichever window resets soonest wins" so
	// the 402's date is the earliest the caller can actually retry.
	//
	// Position is deliberate: team and key are checked first, so a scope that
	// already denied returns before a user lookup is even attempted. Block
	// wins on tie across all three scopes — none of them can turn another's
	// denial into an allow.
	if s.User != "" && g.userLookup != nil {
		if up, ok := g.userLookup(s.Team, s.User); ok {
			uid := userBudgetID(s)
			var userDeny GovDecision
			var userDenyResets time.Time
			if up.BudgetMicrosPerMonth > 0 {
				mw := g.monthWindow()
				ubk := budget.Key(budget.ScopeUser, uid, mw)
				if dec := g.bud.Check(ubk, 0, up.BudgetMicrosPerMonth, mw); mustDenyBudget(dec, up.BudgetExceeded) {
					resetsAt := g.bud.ResetsAt(ubk, mw)
					userDeny = GovDecision{Status: 402, Reason: budgetExceededMessage("user budget", resetsAt, g.budgetLocOrUTC(), up.AdminContact), Code: audit.DenyUserBudgetExceeded}
					userDenyResets = resetsAt
				}
			}
			if up.BudgetMicrosPerDay > 0 {
				dw := g.dayWindow()
				ubkDay := budget.Key(budget.ScopeUser, uid, dw)
				if dec := g.bud.Check(ubkDay, 0, up.BudgetMicrosPerDay, dw); mustDenyBudget(dec, up.BudgetExceeded) {
					resetsAt := g.bud.ResetsAt(ubkDay, dw)
					if userDeny.Status == 0 || resetsAt.Before(userDenyResets) {
						userDeny = GovDecision{Status: 402, Reason: budgetExceededMessage("user daily budget", resetsAt, g.budgetLocOrUTC(), up.AdminContact), Code: audit.DenyUserBudgetExceeded}
						userDenyResets = resetsAt
					}
				}
			}
			if userDeny.Status != 0 {
				return userDeny
			}
		}
	}
	return GovDecision{Allowed: true}
}

// budgetExceededMessage builds the 402 body: when it resets — rendered in the
// budget window's OWN timezone, so a KST-anchored daily window does not print
// the previous UTC date — and, if the binding budget rule carries one, where to
// go for a raise (an admin contact hint, verbatim from the policy — never a
// default, since a wrong guess at contact info is worse than no contact info).
func budgetExceededMessage(kind string, resetsAt time.Time, loc *time.Location, adminContact string) string {
	msg := fmt.Sprintf("%s exceeded — resets %s", kind, resetsAt.In(loc).Format("2006-01-02"))
	if adminContact != "" {
		msg += ". Contact your admin: " + adminContact
	}
	return msg
}

func (g *Governor) UsageOf(s Subject, kp KeyPolicy) UsageStatus {
	if g.HasSharedAuthority() {
		return UsageStatus{Team: s.Team, EnforcementMode: "shared"}
	}
	u := UsageStatus{Team: s.Team}
	if p, ok := g.policyOf(s.Team); ok {
		if p.BudgetMicrosPerMonth > 0 {
			mw := g.monthWindow()
			tbk := budget.Key(budget.ScopeTeam, s.Team, mw)
			spent := g.bud.Spent(tbk, mw)
			u.TeamBudget = &BudgetUsage{
				LimitUSDMicros:     p.BudgetMicrosPerMonth,
				SpentUSDMicros:     spent,
				RemainingUSDMicros: max64(0, p.BudgetMicrosPerMonth-spent),
				Window:             "calendar-month",
				ResetsAt:           g.bud.ResetsAt(tbk, mw),
			}
		}
		if p.BudgetMicrosPerDay > 0 {
			dw := g.dayWindow()
			tbkDay := budget.Key(budget.ScopeTeam, s.Team, dw)
			spent := g.bud.Spent(tbkDay, dw)
			u.TeamBudgetDay = &BudgetUsage{
				LimitUSDMicros:     p.BudgetMicrosPerDay,
				SpentUSDMicros:     spent,
				RemainingUSDMicros: max64(0, p.BudgetMicrosPerDay-spent),
				Window:             "calendar-day",
				ResetsAt:           g.bud.ResetsAt(tbkDay, dw),
			}
		}
		if p.TokensPerDay > 0 {
			u.TeamQuota = &QuotaUsage{
				LimitTokens: p.TokensPerDay,
				UsedTokens:  g.lim.QuotaUsed("quota:"+s.Team, 24*time.Hour),
				Window:      "24h",
			}
		}
	}
	if kp.BudgetMicrosPerMonth > 0 {
		mw := g.monthWindow()
		kbk := budget.Key(budget.ScopeKey, s.KeyID, mw)
		spent := g.bud.Spent(kbk, mw)
		u.KeyBudget = &BudgetUsage{
			LimitUSDMicros:     kp.BudgetMicrosPerMonth,
			SpentUSDMicros:     spent,
			RemainingUSDMicros: max64(0, kp.BudgetMicrosPerMonth-spent),
			Window:             "calendar-month",
			ResetsAt:           g.bud.ResetsAt(kbk, mw),
		}
	}
	if kp.BudgetMicrosPerDay > 0 {
		dw := g.dayWindow()
		kbkDay := budget.Key(budget.ScopeKey, s.KeyID, dw)
		spent := g.bud.Spent(kbkDay, dw)
		u.KeyBudgetDay = &BudgetUsage{
			LimitUSDMicros:     kp.BudgetMicrosPerDay,
			SpentUSDMicros:     spent,
			RemainingUSDMicros: max64(0, kp.BudgetMicrosPerDay-spent),
			Window:             "calendar-day",
			ResetsAt:           g.bud.ResetsAt(kbkDay, dw),
		}
	}
	if s.User != "" && g.userLookup != nil {
		if up, ok := g.userLookup(s.Team, s.User); ok {
			uid := userBudgetID(s)
			if up.BudgetMicrosPerMonth > 0 {
				mw := g.monthWindow()
				ubk := budget.Key(budget.ScopeUser, uid, mw)
				spent := g.bud.Spent(ubk, mw)
				u.UserBudget = &BudgetUsage{
					LimitUSDMicros:     up.BudgetMicrosPerMonth,
					SpentUSDMicros:     spent,
					RemainingUSDMicros: max64(0, up.BudgetMicrosPerMonth-spent),
					Window:             "calendar-month",
					ResetsAt:           g.bud.ResetsAt(ubk, mw),
				}
			}
			if up.BudgetMicrosPerDay > 0 {
				dw := g.dayWindow()
				ubkDay := budget.Key(budget.ScopeUser, uid, dw)
				spent := g.bud.Spent(ubkDay, dw)
				u.UserBudgetDay = &BudgetUsage{
					LimitUSDMicros:     up.BudgetMicrosPerDay,
					SpentUSDMicros:     spent,
					RemainingUSDMicros: max64(0, up.BudgetMicrosPerDay-spent),
					Window:             "calendar-day",
					ResetsAt:           g.bud.ResetsAt(ubkDay, dw),
				}
			}
		}
	}
	if kp.TokensPerMinute > 0 {
		// Same bucket key/burst PreCheck debits ("tpm:key:"+s.KeyID) — RateUsed
		// only peeks at the projected refill, never writes it back.
		used := g.lim.RateUsed("tpm:key:"+s.KeyID, kp.TokensPerMinute, kp.TokensPerMinute)
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
// debits nothing. keyID/kp mirror PreCheck's per-key budget. estimatedTokens
// is the exact value PreCheck charged against the TPM bucket(s) (its
// estimateTokens argument) — Settle uses it to true up TPM against the real
// token count now that the response has completed (see AdjustRate).
// Key-level spend is deliberately NOT added to /metrics: metric labels are
// config-bounded (CLAUDE.md) and must never carry a key_id.
func (g *Governor) Settle(s Subject, kp KeyPolicy, provider, model string, u pricing.Usage, table *pricing.Table, estimatedTokens int64) (costMicros int64, pricingMissing bool) {
	if g.HasSharedAuthority() {
		if table == nil {
			return 0, true
		}
		cost, err := table.CostUSDMicrosChecked(provider, model, u)
		if err != nil {
			return 0, true
		}
		g.metrics.AddBudgetSpend(s.Team, model, "total", float64(cost)/1e6)
		return cost, false
	}
	p, _ := g.policyOf(s.Team)
	// Total tokens actually processed, including cache tiers — the same
	// figure both the daily quota debit and the TPM true-up below use.
	// Excluding cache tokens here used to make a cache-read-heavy request
	// (Claude Code's common case) debit far less quota than it actually
	// consumed, the mirror image of ADR-030's pricing bug in the quota path.
	actualTokens := u.Input + u.Output + u.CacheRead + u.CacheWrite5m + u.CacheWrite1h
	if p.TokensPerDay > 0 {
		g.lim.DebitQuota("quota:"+s.Team, actualTokens, 24*time.Hour)
		// Reflect the post-debit daily quota utilization into the gauge (0..1).
		used := g.lim.QuotaUsed("quota:"+s.Team, 24*time.Hour)
		g.metrics.SetQuotaUtilization(s.Team, "day", float64(used)/float64(p.TokensPerDay))
	}
	// TPM true-up (§5.3): PreCheck pre-blocked on a coarse pre-request byte
	// estimate (estimateTokens); now that actualTokens is known, correct the
	// bucket(s) it charged — credit back an over-charge, debit an
	// under-charge — instead of leaving the estimate as the permanent charge.
	// Claude Code's cache-heavy requests are the case this matters for: the
	// byte estimate counts the full cached prefix even though a cache HIT
	// costs a fraction of that in real load.
	tpmDelta := estimatedTokens - actualTokens
	if p.TokensPerMinute > 0 {
		g.lim.AdjustRate("tpm:"+s.Team, tpmDelta, p.TokensPerMinute)
	}
	if kp.TokensPerMinute > 0 {
		g.lim.AdjustRate("tpm:key:"+s.KeyID, tpmDelta, kp.TokensPerMinute)
	}
	if table == nil {
		costMicros, pricingMissing = 0, true
	} else {
		costMicros, pricingMissing = table.CostUSDMicros(provider, model, u)
	}
	if p.BudgetMicrosPerMonth > 0 {
		mw := g.monthWindow()
		tbk := budget.Key(budget.ScopeTeam, s.Team, mw)
		g.bud.Debit(tbk, costMicros, mw)
		// Debit and Spent are each individually mutex-protected but not one
		// atomic operation: a concurrent Settle for the same team can debit
		// between these two calls. Under concurrent load this can make the
		// read-back spend already reflect a later request's debit too,
		// skipping an intermediate alert threshold (the Notifier still fires
		// the highest crossed one — no double-fire, just a possible skip of
		// an earlier one). A tighter-scoped case of the per-instance/replica
		// approximation ADR-017 §8 documents; ponytail: add
		// BudgetStore.DebitAndRead if this needs to be exact.
		spent := g.bud.Spent(tbk, mw)
		g.metrics.SetBudgetUtilization(s.Team, float64(spent)/float64(p.BudgetMicrosPerMonth))
		if g.notifyBudget != nil {
			g.notifyBudget(s.Team, spent, p.BudgetMicrosPerMonth)
		}
	}
	if p.BudgetMicrosPerDay > 0 {
		dw := g.dayWindow()
		g.bud.Debit(budget.Key(budget.ScopeTeam, s.Team, dw), costMicros, dw)
		// Deliberately no gauge write and no alert hook for the daily window in
		// this phase, and that is CORRECTNESS, not an omission:
		// metrics.SetBudgetUtilization(team, ratio) carries no window label, so
		// a second write here would make one gauge series flap between the day
		// and month ratios; alert.Notifier dedupes already-fired thresholds by
		// team alone, so a day crossing would suppress the month crossing.
		// Both need a window dimension first (internal/metrics, internal/alert).
	}
	if kp.BudgetMicrosPerMonth > 0 {
		mw := g.monthWindow()
		kbk := budget.Key(budget.ScopeKey, s.KeyID, mw)
		g.bud.Debit(kbk, costMicros, mw)
		// Unlike the team block above, this read has no other consumer (no
		// per-key /metrics gauge) — skip it entirely when alerting is off,
		// the common case, to avoid an extra store read on every keyed
		// request (code-gate MINOR, opus).
		if g.notifyKeyBudget != nil {
			spent := g.bud.Spent(kbk, mw)
			g.notifyKeyBudget(s.Team, s.KeyID, spent, kp.BudgetMicrosPerMonth)
		}
	}
	if kp.BudgetMicrosPerDay > 0 {
		dw := g.dayWindow()
		g.bud.Debit(budget.Key(budget.ScopeKey, s.KeyID, dw), costMicros, dw)
		// Same reasoning as the team daily debit above: no per-key alert hook
		// for the daily window until alert.Notifier's fired-set is window-keyed.
	}
	if s.User != "" && g.userLookup != nil {
		if up, ok := g.userLookup(s.Team, s.User); ok {
			uid := userBudgetID(s)
			if up.BudgetMicrosPerMonth > 0 {
				mw := g.monthWindow()
				g.bud.Debit(budget.Key(budget.ScopeUser, uid, mw), costMicros, mw)
			}
			if up.BudgetMicrosPerDay > 0 {
				dw := g.dayWindow()
				g.bud.Debit(budget.Key(budget.ScopeUser, uid, dw), costMicros, dw)
			}
			// Deliberately NO gauge write and NO alert hook, and this is a
			// HARD RULE rather than an omission: metrics.SetBudgetUtilization
			// takes a team label only and a raw user id must never become a
			// Prometheus label (unbounded cardinality, CLAUDE.md — the same
			// bar key_id sits behind); alert.Notifier's fired-set is an
			// unbounded in-memory map, so keying it by user id would leak
			// memory in a long-running process.
		}
	}
	// Observability metrics (approximation; the µUSD budget store is the
	// settlement source of truth). Recorded for every settled request, even an
	// ungoverned team, so /metrics reflects all traffic.
	g.metrics.AddBudgetSpend(s.Team, model, "total", float64(costMicros)/1e6)
	if pricingMissing {
		g.metrics.IncPricingMiss(provider, model)
	}
	// budget.Memory's at-capacity fail-safe (Check returning Block because the
	// store is full, not because a real budget was exceeded) is otherwise
	// invisible to an operator: no team/user label is possible here (the same
	// cardinality bar key_id sits behind), so a plain cumulative gauge is the
	// seam. A type assertion, not a BudgetStore interface method, so a
	// BudgetStore that doesn't track rejections (e.g. a test fake) needs no
	// change.
	if rr, ok := g.bud.(interface{ Rejections() int64 }); ok {
		g.metrics.SetBudgetStoreRejections(rr.Rejections())
	}
	return costMicros, pricingMissing
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// PricingGuard denies a request whose resolved targets have no rate, when the
// operator set pricing.on_missing to "block" (ADR-030).
//
// This is the runtime half of the money guard. Boot validation
// (live.validatePricingCoverage) covers routes declared in the config file, but
// cannot see two paths that reach the same failure: a model registered through
// UI-write (ADR-008) and a target appended by a model-level fallback (ADR-029).
// Both would otherwise serve traffic the gateway cannot price, billing 0 with
// only a boolean in the audit record.
//
// It lives here rather than inside PreCheck because the resolved chain is not
// known until after the router runs, and PreCheck deliberately takes no
// topology. Callers invoke it alongside PreCheck, with the same table they will
// settle against, so the check and the billing can never disagree.
//
// A nil table means pricing is unconfigured entirely — treated as "allow",
// since refusing every request would be a worse failure than reporting zero.
// Returns the zero (allowed) decision when the table permits missing rates.
func PricingGuard(table *pricing.Table, targets []PricedTarget) GovDecision {
	if table == nil || table.OnMissing() != pricing.OnMissingBlock {
		return GovDecision{Allowed: true}
	}
	for _, t := range targets {
		if !table.HasRate(t.Provider, t.Upstream) {
			return GovDecision{
				Status: 402,
				Reason: "no pricing rate for " + t.Provider + "/" + t.Upstream,
				Code:   audit.DenyPricingMissing,
			}
		}
	}
	return GovDecision{Allowed: true}
}

// PricedTarget is the (provider, upstream-model) pair a request may be billed
// against — the same pair CostUSDMicros keys on. Declared here so the ingress
// handlers need not expose their router types to governance.
type PricedTarget struct {
	Provider string
	Upstream string
}
