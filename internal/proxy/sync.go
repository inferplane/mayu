package proxy

// This file implements mayu's control-plane heartbeat (ADR-034): one POST
// per cadence carries the policy pull, cumulative consumption report, lease
// renewal, and version-skew rejection report. The request path never waits
// on this loop — enforcement reads the lease table and policy store, both
// atomic snapshots.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/tier"
)

// Lease is the data-plane view of one team's budget lease for one window,
// merged across that window's rules most-restrictive-first.
type Lease struct {
	AllowanceMicroUSD int64
	ExpiresAt         time.Time
	HardCap           bool
}

// LeaseTable is the request-path view of current leases, keyed by
// (team, budget window). Reads are on the hot path (governor team lookup +
// lease gate); writes happen once per heartbeat. The nested map keeps
// Blocked to one hash lookup plus a walk of that team's ≤2 windows instead
// of a scan of every team in the fleet on every request.
type LeaseTable struct {
	mu     sync.RWMutex
	byTeam map[string]map[v1alpha1.BudgetPeriod]Lease
}

func NewLeaseTable() *LeaseTable {
	return &LeaseTable{byTeam: map[string]map[v1alpha1.BudgetPeriod]Lease{}}
}

// Get returns the merged lease for one team's budget window, if any. An empty
// period reads as CalendarMonth — what every grant meant before LeaseGrant
// carried a window.
func (t *LeaseTable) Get(team string, period v1alpha1.BudgetPeriod) (Lease, bool) {
	if period == "" {
		period = v1alpha1.PeriodCalendarMonth
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	l, ok := t.byTeam[team][period]
	return l, ok
}

// Blocked implements the governor's lease gate (ADR-034). A HARD-cap team
// fails closed when its lease EXPIRED (the global budget can no longer be
// verified locally) or its allowance is zero (the global budget is already
// exhausted before this data plane spent anything — nothing to clamp to,
// since 0 means "unlimited" in TeamPolicy). A valid lease with a positive
// allowance never blocks here — that bound is enforced by the budget check
// itself (the team-lookup closure clamps the limit to the allowance). Soft
// (non-hard-cap) leases never block: control-plane outage fails open for
// them, per-rule failurePolicy. It is TEAM-WIDE across windows: if any one
// window's hard-cap lease is expired or exhausted the team is blocked,
// because a hard cap on either window is a cap the data plane can no longer
// verify locally. The windows are walked in a fixed order (day, then month)
// rather than by map range, so the reason string is deterministic.
func (t *LeaseTable) Blocked(team string) (bool, string) {
	t.mu.RLock()
	windows := t.byTeam[team]
	day, dayOK := windows[v1alpha1.PeriodCalendarDay]
	month, monthOK := windows[v1alpha1.PeriodCalendarMonth]
	t.mu.RUnlock()
	now := time.Now()
	for _, e := range [...]struct {
		l  Lease
		ok bool
	}{{day, dayOK}, {month, monthOK}} {
		if !e.ok || !e.l.HardCap {
			continue
		}
		if now.After(e.l.ExpiresAt) {
			return true, "budget lease expired (control plane unreachable): hard cap fails closed"
		}
		if e.l.AllowanceMicroUSD <= 0 {
			return true, "global hard budget exhausted: no lease allowance remaining"
		}
	}
	return false, ""
}

// set replaces the table from one heartbeat's grants, merging per (team,
// window) most-restrictive-first: smallest allowance binds, hard if any rule
// is hard, earliest expiry wins. Merging never crosses windows — a daily
// allowance and a monthly allowance are not comparable quantities.
func (t *LeaseTable) set(grants []policy.LeaseGrant) {
	byTeam := make(map[string]map[v1alpha1.BudgetPeriod]Lease, len(grants))
	for _, g := range grants {
		// A control plane that predates BudgetRule.period sends no period at
		// all; reading that as CalendarMonth is what keeps the existing wire
		// meaning byte-identical.
		period := g.Period
		if period == "" {
			period = v1alpha1.PeriodCalendarMonth
		}
		windows := byTeam[g.Team]
		if windows == nil {
			windows = map[v1alpha1.BudgetPeriod]Lease{}
			byTeam[g.Team] = windows
		}
		l, ok := windows[period]
		if !ok {
			windows[period] = Lease{AllowanceMicroUSD: g.AllowanceMicroUSD, ExpiresAt: g.ExpiresAt, HardCap: g.HardCap}
			continue
		}
		if g.AllowanceMicroUSD < l.AllowanceMicroUSD {
			l.AllowanceMicroUSD = g.AllowanceMicroUSD
		}
		if g.ExpiresAt.Before(l.ExpiresAt) {
			l.ExpiresAt = g.ExpiresAt
		}
		l.HardCap = l.HardCap || g.HardCap
		windows[period] = l
	}
	t.mu.Lock()
	t.byTeam = byTeam
	t.mu.Unlock()
}

// Syncer runs the heartbeat loop against inferplaned.
type Syncer struct {
	URL       string // control plane base URL
	Token     string // shared bearer token; "" = none
	Dataplane string // stable instance id
	Store     *policy.Store
	Leases    *LeaseTable
	// Tiers is the request-path ADR-041 budget-tier substitution table,
	// kept in step with every heartbeat's resp.ActiveTiers the same way
	// Leases tracks resp.Leases. nil = no substitution applied.
	Tiers *tier.Table
	// SpentOf reports a team's cumulative spend in µUSD for ONE budget window
	// (wired to the governor's usage view). The period argument is load-bearing:
	// reporting monthly spend against a daily rule's ledger row would starve
	// that rule's grant to zero within hours, because the control plane computes
	// remaining = dayLimit − reportedSpend.
	SpentOf func(team string, period v1alpha1.BudgetPeriod) int64
	// OnError receives loop errors (logged by the caller); never fatal —
	// control-plane outage must not take the data plane down.
	OnError func(error)

	client     *http.Client
	generation string
	pending    []policy.Rejection
	// lastSuccess is the wall-clock time of the last successful heartbeat
	// (zero = never), published atomically for the request-path governance
	// gate (GovernanceReady) — read on every request, written once per tick.
	lastSuccess atomic.Int64 // UnixNano; 0 = never synced
	now         func() time.Time
}

// GovernanceReady reports whether this data plane may serve GOVERNED requests
// under a require_sync posture (review/fable5 §08 B2/B3): false before the
// first successful heartbeat (no policy generation has ever arrived — the
// store is empty and default-allow), and false again when maxAge > 0 and the
// last successful sync is older than maxAge (the last-applied set is treated
// as expired, the way hard-cap leases already expire). maxAge <= 0 means
// policies never expire. The reason is operator-facing and secret-free.
func (s *Syncer) GovernanceReady(maxAge time.Duration) (bool, string) {
	last := s.lastSuccess.Load()
	if last == 0 {
		return false, "no policy generation received from the control plane yet (control_plane.require_sync)"
	}
	if maxAge > 0 {
		clock := time.Now
		if s.now != nil {
			clock = s.now
		}
		if age := clock().Sub(time.Unix(0, last)); age > maxAge {
			return false, "control-plane policy generation is stale: last successful sync " + age.Truncate(time.Second).String() + " ago exceeds control_plane.max_policy_age " + maxAge.String()
		}
	}
	return true, ""
}

// Run heartbeats until ctx is done. The first sync fires immediately so a
// freshly booted data plane picks up policy without waiting a full cadence;
// afterwards the control plane's requested interval paces the loop.
// Failures keep the last-applied policy set and leases (their expiry is what
// eventually flips hard caps to fail-closed) and retry next tick.
func (s *Syncer) Run(ctx context.Context) {
	interval := s.tick(ctx, policy.MinPolicySyncInterval)
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			interval = s.tick(ctx, interval)
			t.Reset(interval)
		}
	}
}

// jitterFrac returns a uniform [0,1). A package var so a test can pin the
// spread instead of seeding a RNG.
var jitterFrac = rand.Float64

// backoffJitter is the fraction of a backoff interval that is randomized away.
// Applied downward only, so a jittered interval never exceeds the
// DefaultPolicySyncInterval ceiling nor drops below the Min floor.
const backoffJitter = 0.2

// tick runs one heartbeat and returns the next cadence. Failures back off
// exponentially (doubling up to DefaultPolicySyncInterval) so a fleet of
// data planes doesn't hammer an already-degraded control plane in lockstep
// (PR #50 review finding); the first success snaps back to the control
// plane's requested cadence.
//
// Exponential backoff alone does NOT break lockstep: planes that failed on the
// same control-plane outage double through identical values and retry at the
// same instants, and once they all park at the ceiling they retry together
// forever — the thundering herd lands precisely when the control plane comes
// back up. The interval is therefore jittered downward by up to
// backoffJitter before it is returned.
func (s *Syncer) tick(ctx context.Context, prev time.Duration) time.Duration {
	next, err := s.syncOnce(ctx)
	if err == nil {
		return next
	}
	if s.OnError != nil {
		s.OnError(err)
	}
	backoff := prev * 2
	if backoff < policy.MinPolicySyncInterval {
		backoff = policy.MinPolicySyncInterval
	}
	if backoff > policy.DefaultPolicySyncInterval {
		backoff = policy.DefaultPolicySyncInterval
	}
	return jitter(backoff)
}

// jitter spreads d over [(1-backoffJitter)*d, d], floored at
// MinPolicySyncInterval so the jittered value can never undercut the protocol
// minimum. At the floor there is nothing to spread (d == Min), which is the
// benign case: Min is the fastest cadence, and planes desynchronize on their
// own response latency there.
func jitter(d time.Duration) time.Duration {
	out := d - time.Duration(jitterFrac()*backoffJitter*float64(d))
	if out < policy.MinPolicySyncInterval {
		return policy.MinPolicySyncInterval
	}
	return out
}

// syncOnce does one heartbeat and returns the next cadence.
func (s *Syncer) syncOnce(ctx context.Context) (time.Duration, error) {
	if s.client == nil {
		s.client = &http.Client{Timeout: 10 * time.Second}
	}
	req := policy.SyncRequest{
		Dataplane:   s.Dataplane,
		APIVersions: policy.SupportedAPIVersions,
		Generation:  s.generation,
		Rejections:  s.pending,
	}
	// Cumulative spend per lease-managed budget rule of the APPLIED set.
	//
	// TODO(per-rule spend): SpentOf reads ONE team-level counter, so a team
	// with budget rules in several policies reports the same cumulative
	// spend against each rule — the ledger then under-grants every rule
	// beyond the tightest (conservative, never permissive). Per-rule spend
	// tracking lands with the durable-ledger milestone (ADR-034 known
	// limits). The per-WINDOW half of this is now fixed — SpentOf answers
	// for the rule's own window — so only the several-rules-in-one-window
	// case remains conservative.
	for _, p := range s.Store.Policies() {
		// A user-scoped budget rule has no ledger row upstream (ADR-042
		// Phase 3), so reporting the TEAM's spend against it would be
		// reporting the wrong quantity to a row that does not exist.
		if p.Subject.Team == "" || p.Subject.User != "" {
			continue
		}
		for _, r := range p.Rules {
			if r.Budget == nil {
				continue
			}
			var spent int64
			if s.SpentOf != nil {
				spent = s.SpentOf(p.Subject.Team, r.Budget.Period)
			}
			req.Reports = append(req.Reports, policy.ConsumptionReport{
				Policy: p.Name, Rule: r.Name, Team: p.Subject.Team, SpentMicroUSD: spent,
				Period: r.Budget.Period,
			})
		}
	}

	body, err := json.Marshal(&req)
	if err != nil {
		return 0, fmt.Errorf("control plane sync: encode: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1alpha1/sync", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("control plane sync: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	if s.Token != "" {
		hreq.Header.Set("Authorization", "Bearer "+s.Token)
	}
	hresp, err := s.client.Do(hreq)
	if err != nil {
		return 0, fmt.Errorf("control plane sync: %w", err)
	}
	defer hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(hresp.Body, 4096))
		return 0, fmt.Errorf("control plane sync: status %d", hresp.StatusCode)
	}
	var resp policy.SyncResponse
	if err := json.NewDecoder(io.LimitReader(hresp.Body, 8<<20)).Decode(&resp); err != nil {
		return 0, fmt.Errorf("control plane sync: decode: %w", err)
	}

	// The heartbeat delivered the pending rejections; new ones may replace
	// them below when a fresh document set arrives.
	s.pending = nil
	if resp.Policies != nil {
		rejected := s.Store.ApplyWire(resp.Policies)
		s.pending = rejected
		s.generation = resp.Generation
	}
	if s.Leases != nil {
		s.Leases.set(resp.Leases)
	}
	if s.Tiers != nil {
		s.Tiers.Set(resp.ActiveTiers)
	}
	// Published AFTER the set is applied, so a GovernanceReady()==true
	// observer never sees a store the heartbeat has not yet populated.
	clock := time.Now
	if s.now != nil {
		clock = s.now
	}
	s.lastSuccess.Store(clock().UnixNano())

	next := time.Duration(resp.SyncIntervalSeconds) * time.Second
	if next < time.Second {
		next = policy.DefaultPolicySyncInterval
	}
	return next, nil
}
