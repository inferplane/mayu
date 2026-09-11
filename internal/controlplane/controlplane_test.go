package controlplane

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/policy"
)

const cpPolicyYAML = `apiVersion: inferplane.dev/v1alpha1
kind: GovernancePolicy
metadata: { name: team-a }
spec:
  subject: { team: alpha }
  rules:
  - name: cap
    failurePolicy: FailClosed
    budget:
      limitMilliUSD: 100          # $0.10 = 100,000 µUSD
      hardCap: true
      lease: { grantMilliUSD: 10, renewInterval: "5s" }   # 10,000 µUSD per grant
  - name: models
    failurePolicy: FailOpen
    modelAccess: { allow: ["claude-haiku-4-5"] }
`

// cpTierPolicyYAML adds an ADR-041 budgetTiers routing rule on top of
// cpPolicyYAML's "cap" budget rule: at 80% global utilization,
// claude-haiku-4-5 traffic is substituted to glm-4.7-gpu.
const cpTierPolicyYAML = cpPolicyYAML + `  - name: downgrade-at-80
    failurePolicy: FailOpen
    routing:
      budgetTiers:
        budgetRef: cap
        tiers:
        - thresholdPercent: 80
          substitute: { claude-haiku-4-5: glm-4.7-gpu }
`

func newTierTestServer(t *testing.T, token string) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(cpTierPolicyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(token, dir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	s.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return s, ts
}

func newTestServer(t *testing.T, token string) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(cpPolicyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(token, dir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	s.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return s, ts
}

// newTestServerWrite is newTestServer plus a dedicated policy-write token
// (authnWrite) — required for any test that exercises PUT/DELETE, since the
// heartbeat token no longer carries write authority.
func newTestServerWrite(t *testing.T, token, writeToken string) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(cpPolicyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(token, dir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.SetPolicyWriteToken(writeToken)
	mux := http.NewServeMux()
	s.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return s, ts
}

func doSync(t *testing.T, url, token string, req policy.SyncRequest) policy.SyncResponse {
	t.Helper()
	body, _ := json.Marshal(&req)
	hreq, _ := http.NewRequest(http.MethodPost, url+"/v1alpha1/sync", bytes.NewReader(body))
	hreq.Header.Set("Content-Type", "application/json")
	if token != "" {
		hreq.Header.Set("Authorization", "Bearer "+token)
	}
	hresp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	defer hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		t.Fatalf("sync: status %d", hresp.StatusCode)
	}
	var resp policy.SyncResponse
	if err := json.NewDecoder(hresp.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSyncDistributesPoliciesAndLeases(t *testing.T) {
	_, ts := newTestServer(t, "")

	resp := doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp1", APIVersions: policy.SupportedAPIVersions})
	if len(resp.Policies) != 1 || resp.Policies[0].Metadata.Name != "team-a" {
		t.Fatalf("policies not distributed: %+v", resp.Policies)
	}
	if resp.Generation == "" {
		t.Fatal("no generation")
	}
	if resp.SyncIntervalSeconds != 5 {
		t.Fatalf("interval = %d, want 5 (tightest lease renew)", resp.SyncIntervalSeconds)
	}
	if len(resp.Leases) != 1 {
		t.Fatalf("leases = %+v, want 1", resp.Leases)
	}
	l := resp.Leases[0]
	// Fresh data plane, zero spend: allowance = one grant (10,000 µUSD).
	if l.Team != "alpha" || !l.HardCap || l.AllowanceMicroUSD != 10_000 {
		t.Fatalf("lease mangled: %+v", l)
	}

	// Same generation echoed back → policy payload omitted, leases still flow.
	resp2 := doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp1", Generation: resp.Generation})
	if resp2.Policies != nil {
		t.Fatal("unchanged generation must omit the policy payload")
	}
	if len(resp2.Leases) != 1 {
		t.Fatal("leases must flow on every heartbeat")
	}
}

// A day rule and a month rule are two ledger rows keyed per ruleKey{policy,
// rule}: one heartbeat answers with TWO grants for the team, each stamped
// with its own Period and each sized against its OWN limitMilliUSD. Two
// different allowances is the point — one number would mean the ledger
// merged the rules.
func TestSyncGrantsOneLeasePerBudgetWindow(t *testing.T) {
	const twoWindows = `apiVersion: inferplane.dev/v1alpha1
kind: GovernancePolicy
metadata: { name: team-two-windows }
spec:
  subject: { team: alpha }
  rules:
  - name: daily
    failurePolicy: FailOpen
    budget:
      period: CalendarDay
      limitMilliUSD: 50000        # $50/day; default grant = 0.1% = 50 milliUSD
  - name: monthly
    failurePolicy: FailClosed
    budget:
      limitMilliUSD: 1000000      # $1000/month; default grant = 1000 milliUSD
      hardCap: true
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(twoWindows), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer("", dir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	s.Mount(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp := doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp1"})
	if len(resp.Leases) != 2 {
		t.Fatalf("leases = %+v, want one grant per budget window", resp.Leases)
	}
	byRule := map[string]policy.LeaseGrant{}
	for _, l := range resp.Leases {
		byRule[l.Rule] = l
	}
	day := byRule["daily"]
	if day.Period != v1alpha1.PeriodCalendarDay {
		t.Errorf("daily grant period = %q, want %q", day.Period, v1alpha1.PeriodCalendarDay)
	}
	// Fresh dp, zero spend: allowance = one default grant, sized against the
	// DAY rule's own $50 limit (0.1% = 50 milliUSD = 50,000 µUSD).
	if day.AllowanceMicroUSD != 50_000 {
		t.Errorf("daily allowance = %d, want 50000 (sized against the day rule's own limit)", day.AllowanceMicroUSD)
	}
	month := byRule["monthly"]
	if month.Period != v1alpha1.PeriodCalendarMonth {
		t.Errorf("monthly grant period = %q, want %q", month.Period, v1alpha1.PeriodCalendarMonth)
	}
	// Same sizing rule against the MONTH rule's own $1000 limit: 1000
	// milliUSD = 1,000,000 µUSD — a different number from the day grant,
	// which is what proves the two rules were never merged.
	if month.AllowanceMicroUSD != 1_000_000 || !month.HardCap {
		t.Errorf("monthly grant = %+v, want allowance 1000000 and HardCap true", month)
	}
}

// The wire back-compat guarantee, asserted at the source: a budget rule that
// declares NO period yields a grant stamped CalendarMonth, because
// FromV1Alpha1 normalizes the empty wire value at conversion — an old
// document keeps meaning exactly what it meant before periods existed.
func TestSyncGrantPeriodDefaultsToCalendarMonth(t *testing.T) {
	_, ts := newTestServer(t, "") // cpPolicyYAML's budget rule has no period
	resp := doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp1"})
	if len(resp.Leases) != 1 {
		t.Fatalf("leases = %+v, want 1", resp.Leases)
	}
	if got := resp.Leases[0].Period; got != v1alpha1.PeriodCalendarMonth {
		t.Fatalf("grant period = %q, want %q (a period-less rule means the month)", got, v1alpha1.PeriodCalendarMonth)
	}
}

// The global-budget invariant: across ANY number of data planes, the sum of
// outstanding allowances beyond reported spend never exceeds limit − Σspent.
func TestLeaseLedgerBoundsGlobalOvershoot(t *testing.T) {
	_, ts := newTestServer(t, "")

	// limit 100,000 µUSD; grant 10,000. dp1 reports 60,000 spent.
	r1 := doSync(t, ts.URL, "", policy.SyncRequest{
		Dataplane: "dp1",
		Reports:   []policy.ConsumptionReport{{Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: 60_000}},
	})
	if got := r1.Leases[0].AllowanceMicroUSD; got != 70_000 {
		t.Fatalf("dp1 allowance = %d, want 70000 (spent+grant)", got)
	}

	// dp2 reports 25,000: remaining = 100k − 85k − dp1 outstanding 10k = 5k
	// → grant clipped to 5k, allowance 30k.
	r2 := doSync(t, ts.URL, "", policy.SyncRequest{
		Dataplane: "dp2",
		Reports:   []policy.ConsumptionReport{{Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: 25_000}},
	})
	if got := r2.Leases[0].AllowanceMicroUSD; got != 30_000 {
		t.Fatalf("dp2 allowance = %d, want 30000 (grant clipped to global remaining)", got)
	}

	// dp3 arrives with zero spend: nothing remains → allowance 0 (its hard
	// cap fails closed at the data plane).
	r3 := doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp3"})
	if got := r3.Leases[0].AllowanceMicroUSD; got != 0 {
		t.Fatalf("dp3 allowance = %d, want 0 (global budget exhausted)", got)
	}

	// A DECREASE in dp1's cumulative spend means its budget window rolled
	// over: the ledger adopts the fresh counter and regrants from it — the
	// old 70k allowance must NOT carry into the new window.
	r4 := doSync(t, ts.URL, "", policy.SyncRequest{
		Dataplane: "dp1",
		Reports:   []policy.ConsumptionReport{{Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: 1}},
	})
	if got := r4.Leases[0].AllowanceMicroUSD; got >= 70_000 {
		t.Fatalf("window rollover carried the old allowance: %d", got)
	}
}

// An unlimited: true budget rule has nothing to lease and must not create a
// ledger entry — and, critically, must not perturb the heartbeat interval
// computed from the REAL rule's lease.renewInterval. Before the fix, a
// zero-value LeaseRenewInterval on the unlimited rule always won the
// minRenew comparison ("== 0" is the sentinel for "not yet set"), silently
// discarding the real rule's 5s renew interval.
func TestUnlimitedBudgetRuleSkipsLedgerAndIntervalCalc(t *testing.T) {
	const yaml = cpPolicyYAML + `  - name: no-cap
    failurePolicy: FailOpen
    budget: { unlimited: true }
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ledger[ruleKey{policy: "team-a", rule: "no-cap"}]; ok {
		t.Fatal("an unlimited budget rule must not create a ledger entry")
	}
	if _, ok := s.ledger[ruleKey{policy: "team-a", rule: "cap"}]; !ok {
		t.Fatal("the real budget rule's ledger entry must still exist")
	}
	if s.interval != 5 {
		t.Fatalf("interval = %d, want 5 (the real rule's renewInterval; the unlimited rule must not reset it to the default)", s.interval)
	}
}

func TestDataplanesListsVersionsAndRejections(t *testing.T) {
	_, ts := newTestServer(t, "")
	doSync(t, ts.URL, "", policy.SyncRequest{
		Dataplane:   "dp1",
		APIVersions: []string{"inferplane.dev/v1alpha1"},
		Rejections:  []policy.Rejection{{Policy: "team-a", Rule: "future", Reason: "unknown rule kind"}},
	})

	hresp, err := http.Get(ts.URL + "/v1alpha1/dataplanes")
	if err != nil {
		t.Fatal(err)
	}
	defer hresp.Body.Close()
	var body struct {
		Generation string             `json:"generation"`
		Dataplanes map[string]*dpInfo `json:"dataplanes"`
	}
	if err := json.NewDecoder(hresp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	dp, ok := body.Dataplanes["dp1"]
	if !ok || len(dp.APIVersions) != 1 || len(dp.Rejections) != 1 {
		t.Fatalf("dataplane view mangled: %+v", body.Dataplanes)
	}
}

func TestAuthRequiredWhenTokenSet(t *testing.T) {
	_, ts := newTestServer(t, "s3cret")

	body, _ := json.Marshal(&policy.SyncRequest{Dataplane: "dp1"})
	resp, err := http.Post(ts.URL+"/v1alpha1/sync", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token: status %d, want 401", resp.StatusCode)
	}
	doSync(t, ts.URL, "s3cret", policy.SyncRequest{Dataplane: "dp1"}) // correct token passes
}

// Reload keeps spend for surviving rules and re-fingerprints the set.
func TestReloadPreservesLedger(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p.yaml")
	if err := os.WriteFile(f, []byte(cpPolicyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer("", dir)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Mount(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	doSync(t, ts.URL, "", policy.SyncRequest{
		Dataplane: "dp1",
		Reports:   []policy.ConsumptionReport{{Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: 90_000}},
	})
	gen1 := s.generation

	// Widen the model list (same budget rule survives).
	edited := bytes.Replace([]byte(cpPolicyYAML), []byte(`allow: ["claude-haiku-4-5"]`), []byte(`allow: ["*"]`), 1)
	if err := os.WriteFile(f, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if s.generation == gen1 {
		t.Fatal("generation did not change after edit")
	}
	r := doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp1"})
	// Spend survived: allowance = 90k + min(grant 10k, remaining 10k) = 100k.
	if got := r.Leases[0].AllowanceMicroUSD; got != 100_000 {
		t.Fatalf("allowance after reload = %d, want 100000 (ledger must survive)", got)
	}
}

// Review finding (major): a data plane whose lease horizon passed must have
// its unspent allowance RELEASED — mayu's default instance id is per-boot,
// so restart churn would otherwise permanently strand outstanding grants and
// starve every live proxy's remaining budget. And after pruneAfter, the dead
// data plane's ledger rows disappear entirely.
func TestExpiredLeaseAllowanceReleasedAndPruned(t *testing.T) {
	dir := t.TempDir()
	// Tight limit so outstanding grants visibly clip: limit 15k µUSD,
	// grant 10k, renew 5s (lease horizon 15s).
	tight := `apiVersion: inferplane.dev/v1alpha1
kind: GovernancePolicy
metadata: { name: team-a }
spec:
  subject: { team: alpha }
  rules:
  - name: cap
    failurePolicy: FailClosed
    budget:
      limitMilliUSD: 15
      hardCap: true
      lease: { grantMilliUSD: 10, renewInterval: "5s" }
`
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(tight), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer("", dir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	s.now = func() time.Time { return clock }
	mux := http.NewServeMux()
	s.Mount(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// dp1 takes a 10k grant; dp2 is clipped to the remaining 5k.
	doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp1"})
	r := doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp2"})
	if got := r.Leases[0].AllowanceMicroUSD; got != 5_000 {
		t.Fatalf("dp2 allowance while dp1 live = %d, want 5000", got)
	}

	// dp1 goes silent past its 15s lease horizon: its unspent 10k is
	// released, so dp2's next heartbeat gets the full grant.
	clock = clock.Add(16 * time.Second)
	r = doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp2"})
	if got := r.Leases[0].AllowanceMicroUSD; got != 10_000 {
		t.Fatalf("dp2 allowance after dp1 lease expiry = %d, want 10000 (stranded grant must be released)", got)
	}

	// dp1 had actually spent 3k before dying: that spend still counts...
	s.mu.Lock()
	s.ledger[ruleKey{policy: "team-a", rule: "cap"}].spent["dp1"] = 3_000
	s.mu.Unlock()
	r = doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp2"})
	if got := r.Leases[0].AllowanceMicroUSD; got != 10_000 {
		// remaining = 15k − 3k(dp1) − 0 = 12k → grant uncapped at 10k.
		t.Fatalf("dp2 allowance with dead dp1 spend = %d, want 10000", got)
	}

	// ...until pruneAfter passes and dp1's rows are dropped wholesale.
	clock = clock.Add(pruneAfter + time.Minute)
	doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp2"})
	s.mu.Lock()
	_, spentKept := s.ledger[ruleKey{policy: "team-a", rule: "cap"}].spent["dp1"]
	_, dpKept := s.dataplanes["dp1"]
	s.mu.Unlock()
	if spentKept || dpKept {
		t.Fatal("dp1 rows must be pruned after pruneAfter")
	}
}

// PR #50 review finding (data race): the dataplane view must deep-copy under
// the lock — encoding shared *dpInfo pointers after unlock races with
// concurrent heartbeats. Run with -race.
func TestDataplanesViewConcurrentWithSync(t *testing.T) {
	_, ts := newTestServer(t, "")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 30; i++ {
			doSync(t, ts.URL, "", policy.SyncRequest{
				Dataplane:   "dp-race",
				APIVersions: []string{"inferplane.dev/v1alpha1"},
				Rejections:  []policy.Rejection{{Policy: "p", Reason: "r"}},
				Reports:     []policy.ConsumptionReport{{Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: int64(i)}},
			})
		}
	}()
	for i := 0; i < 30; i++ {
		resp, err := http.Get(ts.URL + "/v1alpha1/dataplanes")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	<-done
}

// ADR-041: the tier must activate on the GLOBAL sum, not any one data
// plane's local view — two data planes each individually well under 80%
// combine to cross it.
func TestActiveTierFiresOnGlobalUtilizationNotPerPlane(t *testing.T) {
	_, ts := newTierTestServer(t, "")

	// limit is 100,000 µUSD (cap: limitMilliUSD 100). dp1 alone at 45,000
	// (45%) must NOT see a tier yet.
	r1 := doSync(t, ts.URL, "", policy.SyncRequest{
		Dataplane: "dp1",
		Reports:   []policy.ConsumptionReport{{Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: 45_000}},
	})
	if len(r1.ActiveTiers) != 0 {
		t.Fatalf("dp1 alone at 45%%: got active tiers %+v, want none", r1.ActiveTiers)
	}

	// dp2 alone at 40,000 (40%) — but combined with dp1's 45,000 that's
	// 85,000 = 85% globally, crossing the 80% tier.
	r2 := doSync(t, ts.URL, "", policy.SyncRequest{
		Dataplane: "dp2",
		Reports:   []policy.ConsumptionReport{{Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: 40_000}},
	})
	if len(r2.ActiveTiers) != 1 {
		t.Fatalf("global 85%%: got %d active tiers, want 1: %+v", len(r2.ActiveTiers), r2.ActiveTiers)
	}
	at := r2.ActiveTiers[0]
	if at.Policy != "team-a" || at.Rule != "downgrade-at-80" || at.BudgetRef != "cap" || at.Team != "alpha" ||
		at.ThresholdPercent != 80 || at.Substitute["claude-haiku-4-5"] != "glm-4.7-gpu" {
		t.Fatalf("active tier mangled: %+v", at)
	}

	// dp1 alone in this SAME heartbeat also sees the globally-active tier —
	// substitution is judged globally, applied to every data plane.
	r1b := doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp1"})
	if len(r1b.ActiveTiers) != 1 {
		t.Fatalf("dp1's own heartbeat after global activation: got %d tiers, want 1", len(r1b.ActiveTiers))
	}
}

// The latch holds within a budget window even if a later heartbeat's
// reported spend appears lower (e.g. a window-rollover report from an
// UNRELATED data plane momentarily drops the global sum) — activation must
// not flap request-to-request.
func TestActiveTierLatchHoldsOnSpendDrop(t *testing.T) {
	_, ts := newTierTestServer(t, "")

	doSync(t, ts.URL, "", policy.SyncRequest{
		Dataplane: "dp1",
		Reports:   []policy.ConsumptionReport{{Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: 90_000}},
	})
	// Confirm it's active first.
	r := doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp1"})
	if len(r.ActiveTiers) != 1 {
		t.Fatalf("want tier active at 90%%, got %+v", r.ActiveTiers)
	}

	// dp1's own counter now drops sharply (window rollover semantics: a
	// decreasing report). Within the SAME calendar-month latch window the
	// tier must stay active.
	r2 := doSync(t, ts.URL, "", policy.SyncRequest{
		Dataplane: "dp1",
		Reports:   []policy.ConsumptionReport{{Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: 1}},
	})
	if len(r2.ActiveTiers) != 1 {
		t.Fatalf("latch did not hold on spend drop within window: %+v", r2.ActiveTiers)
	}
}

// The latch resets when the injected clock crosses a calendar-month
// boundary: a fresh window with low utilization must NOT inherit the
// previous window's active tier.
func TestActiveTierLatchResetsOnWindowChange(t *testing.T) {
	s, ts := newTierTestServer(t, "")
	fixed := time.Date(2026, time.August, 15, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }

	doSync(t, ts.URL, "", policy.SyncRequest{
		Dataplane: "dp1",
		Reports:   []policy.ConsumptionReport{{Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: 90_000}},
	})
	r := doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp1"})
	if len(r.ActiveTiers) != 1 {
		t.Fatalf("want tier active in August, got %+v", r.ActiveTiers)
	}

	// Cross into September; the ledger's own spend also rolls over (a fresh
	// window's cumulative report legitimately restarts low).
	next := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return next }
	r2 := doSync(t, ts.URL, "", policy.SyncRequest{
		Dataplane: "dp1",
		Reports:   []policy.ConsumptionReport{{Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: 1}},
	})
	if len(r2.ActiveTiers) != 0 {
		t.Fatalf("latch did not reset on window change: %+v", r2.ActiveTiers)
	}
}

// A budgetTiers rule whose budgetRef names an unlimited budget rule is
// rejected at conversion (internal/policy), so it can never even reach the
// control plane's ledger — this pins that no tier silently "activates" for
// one either.
func TestActiveTierUnlimitedBudgetRefRejectedAtLoad(t *testing.T) {
	const yaml = `apiVersion: inferplane.dev/v1alpha1
kind: GovernancePolicy
metadata: { name: bt }
spec:
  subject: { team: alpha }
  rules:
  - name: no-cap
    failurePolicy: FailOpen
    budget: { unlimited: true }
  - name: downgrade
    failurePolicy: FailOpen
    routing:
      budgetTiers:
        budgetRef: no-cap
        tiers:
        - thresholdPercent: 80
          substitute: { a: b }
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer("", dir); err == nil {
		t.Fatal("budgetTiers against an unlimited budget rule was accepted")
	}
}

// The latch survives a Reload (policy edit) for a rule whose name is
// unchanged — an operator editing an unrelated field (e.g. widening the
// model allow-list) must not silently un-latch every team mid-window.
func TestActiveTierSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p.yaml")
	if err := os.WriteFile(f, []byte(cpTierPolicyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer("", dir)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Mount(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	doSync(t, ts.URL, "", policy.SyncRequest{
		Dataplane: "dp1",
		Reports:   []policy.ConsumptionReport{{Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: 90_000}},
	})
	r := doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp1"})
	if len(r.ActiveTiers) != 1 {
		t.Fatalf("want tier active before reload, got %+v", r.ActiveTiers)
	}

	edited := bytes.Replace([]byte(cpTierPolicyYAML), []byte(`allow: ["claude-haiku-4-5"]`), []byte(`allow: ["*"]`), 1)
	if err := os.WriteFile(f, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}

	// A fresh heartbeat with a spend report BELOW 80% would, on its own,
	// evaluate to no tier — the latch is what keeps it active post-reload.
	r2 := doSync(t, ts.URL, "", policy.SyncRequest{
		Dataplane: "dp1",
		Reports:   []policy.ConsumptionReport{{Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: 90_000}},
	})
	if len(r2.ActiveTiers) != 1 {
		t.Fatalf("latch did not survive reload: %+v", r2.ActiveTiers)
	}
}
