package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/controlplane"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/tier"
)

// The distributed set carries one enforceable policy and one this data-plane
// build must reject (a routing rule) — the rejection must be reported on the
// NEXT heartbeat, never dropped.
const syncPolicyYAML = `apiVersion: inferplane.dev/v1alpha1
kind: GovernancePolicy
metadata: { name: team-a }
spec:
  subject: { team: alpha }
  rules:
  - name: cap
    failurePolicy: FailClosed
    budget:
      limitMilliUSD: 100
      hardCap: true
      lease: { grantMilliUSD: 10, renewInterval: "5s" }
---
apiVersion: inferplane.dev/v1alpha1
kind: GovernancePolicy
metadata: { name: team-b-pin }
spec:
  subject: { team: beta }
  rules:
  - name: pin
    failurePolicy: FailOpen
    routing: { onAffinityConflict: PreferAffinity }
`

// syncTierPolicyYAML adds an ADR-041 budgetTiers routing rule on top of the
// "cap" budget rule.
const syncTierPolicyYAML = `apiVersion: inferplane.dev/v1alpha1
kind: GovernancePolicy
metadata: { name: team-a }
spec:
  subject: { team: alpha }
  rules:
  - name: cap
    failurePolicy: FailClosed
    budget:
      limitMilliUSD: 100
      hardCap: true
      lease: { grantMilliUSD: 10, renewInterval: "5s" }
  - name: downgrade-at-80
    failurePolicy: FailOpen
    routing:
      budgetTiers:
        budgetRef: cap
        tiers:
        - thresholdPercent: 80
          substitute: { claude-haiku-4-5: glm-4.7-gpu }
`

func newControlPlaneWithTiers(t *testing.T, token string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(syncTierPolicyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cp, err := controlplane.NewServer(token, dir)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	cp.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func newControlPlane(t *testing.T, token string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(syncPolicyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cp, err := controlplane.NewServer(token, dir)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	cp.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestSyncerAppliesPoliciesLeasesAndReportsRejections(t *testing.T) {
	ts := newControlPlane(t, "tok")
	store := policy.NewEmptyStore()
	leases := NewLeaseTable()
	spent := int64(0)
	s := &Syncer{
		URL: ts.URL, Token: "tok", Dataplane: "dp1",
		Store: store, Leases: leases,
		SpentOf: func(team string, period v1alpha1.BudgetPeriod) int64 { return spent },
	}

	next, err := s.syncOnce(context.Background())
	if err != nil {
		t.Fatalf("syncOnce: %v", err)
	}
	if next != 5*time.Second {
		t.Fatalf("next interval = %s, want 5s (control-plane cadence)", next)
	}
	// The enforceable policy applied; the routing one was rejected.
	if tl, ok := store.TeamLimits("alpha"); !ok || tl.BudgetMicrosPerMonth != 100_000 || !tl.BudgetHard {
		t.Fatalf("distributed budget not applied: %+v", tl)
	}
	if len(s.pending) != 1 || s.pending[0].Policy != "team-b-pin" {
		t.Fatalf("rejection not recorded: %+v", s.pending)
	}
	// Lease landed: fresh dp, zero spend → allowance = one grant.
	l, ok := leases.Get("alpha", v1alpha1.PeriodCalendarMonth)
	if !ok || l.AllowanceMicroUSD != 10_000 || !l.HardCap {
		t.Fatalf("lease not applied: %+v", l)
	}
	if blocked, _ := leases.Blocked("alpha"); blocked {
		t.Fatal("valid lease with allowance must not block")
	}

	// Second heartbeat: reports spend, delivers the pending rejection, and
	// the control plane raises the allowance to spent+grant.
	spent = 60_000
	if _, err := s.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.pending) != 0 {
		t.Fatalf("rejections not cleared after delivery: %+v", s.pending)
	}
	l, _ = leases.Get("alpha", v1alpha1.PeriodCalendarMonth)
	if l.AllowanceMicroUSD != 70_000 {
		t.Fatalf("allowance after report = %d, want 70000", l.AllowanceMicroUSD)
	}
}

func TestLeaseTableGate(t *testing.T) {
	lt := NewLeaseTable()

	// Hard cap, expired → blocked.
	lt.set([]policy.LeaseGrant{{Team: "a", AllowanceMicroUSD: 10, ExpiresAt: time.Now().Add(-time.Second), HardCap: true}})
	if blocked, _ := lt.Blocked("a"); !blocked {
		t.Fatal("expired hard-cap lease must block")
	}
	// Hard cap, zero allowance → blocked (global budget exhausted).
	lt.set([]policy.LeaseGrant{{Team: "a", AllowanceMicroUSD: 0, ExpiresAt: time.Now().Add(time.Minute), HardCap: true}})
	if blocked, _ := lt.Blocked("a"); !blocked {
		t.Fatal("zero-allowance hard-cap lease must block")
	}
	// Soft lease: never blocks, even expired (fails open per rule).
	lt.set([]policy.LeaseGrant{{Team: "a", AllowanceMicroUSD: 0, ExpiresAt: time.Now().Add(-time.Second), HardCap: false}})
	if blocked, _ := lt.Blocked("a"); blocked {
		t.Fatal("soft lease must fail open")
	}
	// No lease at all: not blocked.
	if blocked, _ := lt.Blocked("unknown"); blocked {
		t.Fatal("lease-less team must not block")
	}

	// Per-team merge is most-restrictive.
	lt.set([]policy.LeaseGrant{
		{Team: "m", AllowanceMicroUSD: 500, ExpiresAt: time.Now().Add(time.Hour), HardCap: false},
		{Team: "m", AllowanceMicroUSD: 200, ExpiresAt: time.Now().Add(time.Minute), HardCap: true},
	})
	l, _ := lt.Get("m", v1alpha1.PeriodCalendarMonth)
	if l.AllowanceMicroUSD != 200 || !l.HardCap {
		t.Fatalf("merge not most-restrictive: %+v", l)
	}
}

// A dead control plane must not clear the last-applied policy set or leases —
// expiry is what degrades them, per rule.
func TestSyncerOutageKeepsLastState(t *testing.T) {
	ts := newControlPlane(t, "")
	store := policy.NewEmptyStore()
	leases := NewLeaseTable()
	s := &Syncer{URL: ts.URL, Dataplane: "dp1", Store: store, Leases: leases, SpentOf: func(string, v1alpha1.BudgetPeriod) int64 { return 0 }}
	if _, err := s.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts.Close()
	if _, err := s.syncOnce(context.Background()); err == nil {
		t.Fatal("sync against a dead control plane must error")
	}
	if _, ok := store.TeamLimits("alpha"); !ok {
		t.Fatal("policy set lost on outage")
	}
	if _, ok := leases.Get("alpha", v1alpha1.PeriodCalendarMonth); !ok {
		t.Fatal("leases lost on outage")
	}
}

// ---------------------------------------------------------------------------
// Phase 2 (BudgetRule.period): the lease channel must be WINDOW-AWARE.
// These tests are written against the NEW API and do not compile against the
// old one — that is deliberate.
// ---------------------------------------------------------------------------

// twoWindowPolicyYAML declares the shape this whole phase exists for: one team,
// a soft DAILY cap and a hard MONTHLY cap, as two budget rules.
const twoWindowPolicyYAML = `apiVersion: inferplane.dev/v1alpha1
kind: GovernancePolicy
metadata: { name: team-two-windows }
spec:
  subject: { team: alpha }
  rules:
  - name: daily
    failurePolicy: FailOpen
    budget:
      period: CalendarDay
      limitMilliUSD: 50000
      lease: { grantMilliUSD: 50, renewInterval: "5s" }
  - name: monthly
    failurePolicy: FailClosed
    budget:
      limitMilliUSD: 1000000
      hardCap: true
      lease: { grantMilliUSD: 1000, renewInterval: "5s" }
`

// recordingCP is a fake control plane that captures the decoded sync request and
// answers with a canned response. It exists because the assertion here is about
// what the DATA PLANE puts on the wire, which the real control plane's own
// ledger would obscure.
type recordingCP struct {
	srv  *httptest.Server
	last policy.SyncRequest
	resp policy.SyncResponse
}

func newRecordingCP(t *testing.T) *recordingCP {
	t.Helper()
	cp := &recordingCP{resp: policy.SyncResponse{Generation: "g1", SyncIntervalSeconds: 5}}
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&cp.last); err != nil {
			t.Errorf("decode sync request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&cp.resp)
	}))
	t.Cleanup(cp.srv.Close)
	return cp
}

// TestSyncOnceReportsDailyRuleAgainstDailySpend is the sharpest correctness trap
// of this phase. SpentOf used to take only a team and always answered with the
// MONTHLY counter, so adding a daily rule would report month-to-date spend
// against the daily rule's ledger row — the control plane computes
// remaining = dayLimit - reportedSpend, so a $50/day rule would be starved to a
// zero grant within hours of the month's spend passing $50.
//
// This test fails against the old single-argument SpentOf (it does not compile)
// AND against a window-blind implementation (both reports would carry the same
// number).
func TestSyncOnceReportsDailyRuleAgainstDailySpend(t *testing.T) {
	cp := newRecordingCP(t)
	store := policy.NewEmptyStore()
	// Load the two-rule document through the same path the data plane uses, so
	// the periods under test are the ones FromV1Alpha1 actually produces.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(twoWindowPolicyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	wire, _, err := policy.LoadWirePaths(dir)
	if err != nil {
		t.Fatalf("LoadWirePaths: %v", err)
	}
	if rej := store.ApplyWire(wire); len(rej) != 0 {
		t.Fatalf("two-window policy rejected: %+v", rej)
	}

	const daySpent, monthSpent = 7_000_000, 123_000_000
	var asked []v1alpha1.BudgetPeriod
	s := &Syncer{
		URL: cp.srv.URL, Dataplane: "dp1", Store: store, Leases: NewLeaseTable(),
		SpentOf: func(team string, period v1alpha1.BudgetPeriod) int64 {
			if team != "alpha" {
				t.Errorf("SpentOf team = %q, want alpha", team)
			}
			asked = append(asked, period)
			if period == v1alpha1.PeriodCalendarDay {
				return daySpent
			}
			return monthSpent
		},
	}
	if _, err := s.syncOnce(context.Background()); err != nil {
		t.Fatalf("syncOnce: %v", err)
	}

	byRule := map[string]policy.ConsumptionReport{}
	for _, rep := range cp.last.Reports {
		byRule[rep.Rule] = rep
	}
	if len(byRule) != 2 {
		t.Fatalf("want one report per budget rule, got %+v", cp.last.Reports)
	}
	if got := byRule["daily"].SpentMicroUSD; got != daySpent {
		t.Errorf("daily rule reported %d µUSD, want the DAY counter %d (reporting month spend against a day rule starves its grant)", got, daySpent)
	}
	if got := byRule["monthly"].SpentMicroUSD; got != monthSpent {
		t.Errorf("monthly rule reported %d µUSD, want the MONTH counter %d", got, monthSpent)
	}
	// The report must also name the window it measured.
	if got := byRule["daily"].Period; got != v1alpha1.PeriodCalendarDay {
		t.Errorf("daily report period = %q, want %q", got, v1alpha1.PeriodCalendarDay)
	}
	if got := byRule["monthly"].Period; got != v1alpha1.PeriodCalendarMonth {
		t.Errorf("monthly report period = %q, want %q (an omitted wire period is normalized to CalendarMonth at conversion)", got, v1alpha1.PeriodCalendarMonth)
	}
	// And SpentOf must have been consulted once per window, not twice for one.
	seen := map[v1alpha1.BudgetPeriod]int{}
	for _, p := range asked {
		seen[p]++
	}
	if seen[v1alpha1.PeriodCalendarDay] != 1 || seen[v1alpha1.PeriodCalendarMonth] != 1 {
		t.Errorf("SpentOf asked for %v, want exactly one day and one month query", asked)
	}
}

// TestLeaseTableKeepsDayAndMonthAllowancesSeparate pins the re-keying. The old
// table merged every grant for a team by min(AllowanceMicroUSD), so a $50/day
// allowance and a $1000/month allowance collapsed into one meaningless number
// that was then clamped onto BOTH windows.
func TestLeaseTableKeepsDayAndMonthAllowancesSeparate(t *testing.T) {
	lt := NewLeaseTable()
	exp := time.Now().Add(time.Hour)
	lt.set([]policy.LeaseGrant{
		{Team: "t", Period: v1alpha1.PeriodCalendarDay, AllowanceMicroUSD: 50_000, ExpiresAt: exp},
		{Team: "t", Period: v1alpha1.PeriodCalendarMonth, AllowanceMicroUSD: 900_000, ExpiresAt: exp, HardCap: true},
	})

	day, ok := lt.Get("t", v1alpha1.PeriodCalendarDay)
	if !ok {
		t.Fatal("no daily lease")
	}
	if day.AllowanceMicroUSD != 50_000 || day.HardCap {
		t.Errorf("daily lease = %+v, want allowance 50000 and HardCap false", day)
	}
	month, ok := lt.Get("t", v1alpha1.PeriodCalendarMonth)
	if !ok {
		t.Fatal("no monthly lease")
	}
	if month.AllowanceMicroUSD != 900_000 || !month.HardCap {
		t.Errorf("monthly lease = %+v, want allowance 900000 and HardCap true (the smaller DAILY allowance must not bind the month)", month)
	}

	// Merging still happens WITHIN a window, most-restrictive-first.
	lt.set([]policy.LeaseGrant{
		{Team: "m", Period: v1alpha1.PeriodCalendarDay, AllowanceMicroUSD: 500, ExpiresAt: exp},
		{Team: "m", Period: v1alpha1.PeriodCalendarDay, AllowanceMicroUSD: 200, ExpiresAt: time.Now().Add(time.Minute), HardCap: true},
		{Team: "m", Period: v1alpha1.PeriodCalendarMonth, AllowanceMicroUSD: 9_000, ExpiresAt: exp},
	})
	d, _ := lt.Get("m", v1alpha1.PeriodCalendarDay)
	if d.AllowanceMicroUSD != 200 || !d.HardCap {
		t.Errorf("within-window merge not most-restrictive: %+v", d)
	}
	if mo, _ := lt.Get("m", v1alpha1.PeriodCalendarMonth); mo.AllowanceMicroUSD != 9_000 {
		t.Errorf("month allowance corrupted by the day merge: %+v", mo)
	}
}

// TestLeaseTableEmptyPeriodIsCalendarMonth is the wire back-compat guarantee: a
// control plane that predates BudgetRule.period sends no period at all, and that
// must keep meaning exactly what it meant before — the monthly window.
func TestLeaseTableEmptyPeriodIsCalendarMonth(t *testing.T) {
	lt := NewLeaseTable()
	lt.set([]policy.LeaseGrant{
		{Team: "old", AllowanceMicroUSD: 4_242, ExpiresAt: time.Now().Add(time.Hour), HardCap: true},
	})
	l, ok := lt.Get("old", v1alpha1.PeriodCalendarMonth)
	if !ok || l.AllowanceMicroUSD != 4_242 {
		t.Fatalf("a period-less grant must land in the CalendarMonth window: %+v ok=%v", l, ok)
	}
	// Reading with an empty period must resolve to the same entry.
	if l2, ok2 := lt.Get("old", ""); !ok2 || l2.AllowanceMicroUSD != 4_242 {
		t.Fatalf(`Get(team, "") must read as CalendarMonth: %+v ok=%v`, l2, ok2)
	}
	if _, ok := lt.Get("old", v1alpha1.PeriodCalendarDay); ok {
		t.Fatal("a period-less grant must NOT create a daily lease")
	}
}

// TestLeaseTableBlockedIsTeamWideAcrossWindows: Blocked stays team-wide. A hard
// cap on EITHER window that can no longer be verified locally blocks the team —
// block wins on tie.
func TestLeaseTableBlockedIsTeamWideAcrossWindows(t *testing.T) {
	future := time.Now().Add(time.Hour)

	// Healthy month lease, EXHAUSTED hard day lease → blocked.
	lt := NewLeaseTable()
	lt.set([]policy.LeaseGrant{
		{Team: "t", Period: v1alpha1.PeriodCalendarDay, AllowanceMicroUSD: 0, ExpiresAt: future, HardCap: true},
		{Team: "t", Period: v1alpha1.PeriodCalendarMonth, AllowanceMicroUSD: 900_000, ExpiresAt: future, HardCap: true},
	})
	if blocked, reason := lt.Blocked("t"); !blocked {
		t.Errorf("an exhausted hard DAY lease must block the team even with a healthy month lease (reason=%q)", reason)
	}

	// Healthy day lease, EXPIRED hard month lease → blocked.
	lt = NewLeaseTable()
	lt.set([]policy.LeaseGrant{
		{Team: "t", Period: v1alpha1.PeriodCalendarDay, AllowanceMicroUSD: 50_000, ExpiresAt: future, HardCap: true},
		{Team: "t", Period: v1alpha1.PeriodCalendarMonth, AllowanceMicroUSD: 900_000, ExpiresAt: time.Now().Add(-time.Second), HardCap: true},
	})
	if blocked, _ := lt.Blocked("t"); !blocked {
		t.Error("an expired hard MONTH lease must block the team")
	}

	// Both windows healthy → not blocked.
	lt = NewLeaseTable()
	lt.set([]policy.LeaseGrant{
		{Team: "t", Period: v1alpha1.PeriodCalendarDay, AllowanceMicroUSD: 50_000, ExpiresAt: future, HardCap: true},
		{Team: "t", Period: v1alpha1.PeriodCalendarMonth, AllowanceMicroUSD: 900_000, ExpiresAt: future, HardCap: true},
	})
	if blocked, reason := lt.Blocked("t"); blocked {
		t.Errorf("two healthy hard leases must not block: %q", reason)
	}

	// A SOFT day lease, exhausted and expired, still fails open.
	lt = NewLeaseTable()
	lt.set([]policy.LeaseGrant{
		{Team: "t", Period: v1alpha1.PeriodCalendarDay, AllowanceMicroUSD: 0, ExpiresAt: time.Now().Add(-time.Second)},
	})
	if blocked, _ := lt.Blocked("t"); blocked {
		t.Error("a soft lease must fail open on either window")
	}
}

// The syncer applies the control plane's ActiveTiers into the Tiers table
// (ADR-041), the same way it applies Leases.
func TestSyncerAppliesActiveTiers(t *testing.T) {
	ts := newControlPlaneWithTiers(t, "")
	store := policy.NewEmptyStore()
	leases := NewLeaseTable()
	tiers := tier.NewTable()
	s := &Syncer{
		URL: ts.URL, Dataplane: "dp1",
		Store: store, Leases: leases, Tiers: tiers,
		SpentOf: func(string, v1alpha1.BudgetPeriod) int64 { return 90_000 }, // 90% of the 100,000 µUSD limit
	}
	// First heartbeat applies the policy set (the report loop below reads
	// s.Store.Policies(), which is still empty before this); the second
	// heartbeat's report then carries the 90% spend to the ledger.
	if _, err := s.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := tiers.Get("alpha")
	if got["claude-haiku-4-5"] != "glm-4.7-gpu" {
		t.Fatalf("tier not applied: %v", got)
	}
}

// A dead control plane must not clear the last-applied tier state either —
// same keep-last-state posture as leases and policies.
func TestSyncerOutageKeepsLastTierState(t *testing.T) {
	ts := newControlPlaneWithTiers(t, "")
	store := policy.NewEmptyStore()
	leases := NewLeaseTable()
	tiers := tier.NewTable()
	s := &Syncer{
		URL: ts.URL, Dataplane: "dp1",
		Store: store, Leases: leases, Tiers: tiers,
		SpentOf: func(string, v1alpha1.BudgetPeriod) int64 { return 90_000 },
	}
	if _, err := s.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tiers.Get("alpha")["claude-haiku-4-5"] != "glm-4.7-gpu" {
		t.Fatal("tier not applied before outage")
	}
	ts.Close()
	if _, err := s.syncOnce(context.Background()); err == nil {
		t.Fatal("sync against a dead control plane must error")
	}
	if tiers.Get("alpha")["claude-haiku-4-5"] != "glm-4.7-gpu" {
		t.Fatal("tier state lost on outage")
	}
}

// TestBackoffIsJittered pins the anti-thundering-herd property: two planes
// that fail on the same outage must NOT return the same retry interval, and
// the jittered value must stay inside the protocol's [Min, Default] band.
func TestBackoffIsJittered(t *testing.T) {
	orig := jitterFrac
	defer func() { jitterFrac = orig }()

	// Ceiling case — the one that matters: every plane parks at Default and,
	// unjittered, would retry in lockstep when the control plane returns.
	for _, frac := range []float64{0, 0.5, 0.999999} {
		jitterFrac = func() float64 { return frac }
		got := jitter(policy.DefaultPolicySyncInterval)
		lo := policy.DefaultPolicySyncInterval - time.Duration(backoffJitter*float64(policy.DefaultPolicySyncInterval))
		if got > policy.DefaultPolicySyncInterval {
			t.Errorf("jitter(%v) = %v, must never exceed the ceiling", policy.DefaultPolicySyncInterval, got)
		}
		if got < lo {
			t.Errorf("jitter(%v) = %v, want >= %v", policy.DefaultPolicySyncInterval, got, lo)
		}
	}

	// Distinct fractions must produce distinct intervals — otherwise the fleet
	// is still in lockstep.
	jitterFrac = func() float64 { return 0 }
	a := jitter(policy.DefaultPolicySyncInterval)
	jitterFrac = func() float64 { return 0.9 }
	b := jitter(policy.DefaultPolicySyncInterval)
	if a == b {
		t.Errorf("jitter is not spreading: both planes returned %v", a)
	}

	// Floor case: never undercut the protocol minimum.
	jitterFrac = func() float64 { return 0.999999 }
	if got := jitter(policy.MinPolicySyncInterval); got != policy.MinPolicySyncInterval {
		t.Errorf("jitter at the floor = %v, want exactly %v", got, policy.MinPolicySyncInterval)
	}
}

// TestTickBacksOffWithinBandOnFailure covers tick's whole failure path: the
// doubling, the two clamps, and the jitter, against an unreachable endpoint.
func TestTickBacksOffWithinBandOnFailure(t *testing.T) {
	orig := jitterFrac
	defer func() { jitterFrac = orig }()
	jitterFrac = func() float64 { return 0.5 }

	// A closed port: syncOnce fails without waiting on a network timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	s := &Syncer{URL: url, Token: "t", Dataplane: "dp", Store: policy.NewEmptyStore(), Leases: NewLeaseTable()}
	got := s.tick(context.Background(), policy.MinPolicySyncInterval)
	if got < policy.MinPolicySyncInterval || got > policy.DefaultPolicySyncInterval {
		t.Errorf("backoff = %v, want within [%v, %v]", got, policy.MinPolicySyncInterval, policy.DefaultPolicySyncInterval)
	}
}

// TestGovernanceReady pins the require_sync gate (review/fable5 §08 B2/B3):
// not ready before the first successful heartbeat; ready after; stale again
// once the last success is older than maxAge; maxAge<=0 never expires.
func TestGovernanceReady(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := &Syncer{now: func() time.Time { return now }}
	if ok, reason := s.GovernanceReady(0); ok || !strings.Contains(reason, "no policy generation") {
		t.Fatalf("never-synced must be not-ready: ok=%v reason=%q", ok, reason)
	}
	s.lastSuccess.Store(now.UnixNano())
	if ok, _ := s.GovernanceReady(0); !ok {
		t.Fatal("synced with maxAge 0 must be ready")
	}
	if ok, _ := s.GovernanceReady(10 * time.Minute); !ok {
		t.Fatal("fresh sync within maxAge must be ready")
	}
	now = now.Add(11 * time.Minute)
	if ok, reason := s.GovernanceReady(10 * time.Minute); ok || !strings.Contains(reason, "stale") {
		t.Fatalf("sync older than maxAge must be not-ready: ok=%v reason=%q", ok, reason)
	}
	if ok, _ := s.GovernanceReady(0); !ok {
		t.Fatal("maxAge 0 never expires")
	}
}

// A successful heartbeat publishes lastSuccess AFTER applying the set.
func TestSyncOnceMarksGovernanceReady(t *testing.T) {
	ts := newControlPlane(t, "tok")
	s := &Syncer{URL: ts.URL, Token: "tok", Dataplane: "dp1", Store: policy.NewEmptyStore(), Leases: NewLeaseTable(),
		SpentOf: func(string, v1alpha1.BudgetPeriod) int64 { return 0 }}
	if ok, _ := s.GovernanceReady(0); ok {
		t.Fatal("must not be ready before any sync")
	}
	if _, err := s.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.GovernanceReady(0); !ok {
		t.Fatal("must be ready after a successful sync")
	}
}
