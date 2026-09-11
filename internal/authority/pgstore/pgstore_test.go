package pgstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/policy"
)

// Each test owns a fresh schema, including its own policies table. Never reset
// shared application tables: other Postgres test packages run concurrently.
func testDatabase(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("INFERPLANE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("INFERPLANE_TEST_PG_DSN is unset")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot construct local test database pool")
	}
	name := "authority_test_" + nonce()
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+ident); err != nil {
		admin.Close()
		t.Fatal("cannot create isolated test schema")
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, `DROP SCHEMA `+ident+` CASCADE`); err != nil {
			t.Error("cannot drop isolated test schema")
		}
		admin.Close()
	})
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid local test DSN")
		}
		q := u.Query()
		q.Set("search_path", name)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + name
	}
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot construct isolated test database pool")
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(ctx, `CREATE TABLE policies (
		name TEXT PRIMARY KEY, doc_yaml TEXT NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	return dsn, db
}

func nonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func openStore(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Initialize(context.Background()); err != nil {
		s.Close()
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func putBudget(t *testing.T, db *pgxpool.Pool, limit, grant int64, hard, tiers bool) {
	t.Helper()
	doc := fmt.Sprintf(`{
		"apiVersion":"governance.inferplane.dev/v1alpha1",
		"kind":"GovernancePolicy","metadata":{"name":"budget"},
		"spec":{"subject":{"team":"alpha"},"rules":[
			{"name":"cap","failurePolicy":"FailClosed","budget":{
				"limitMilliUSD":%d,"hardCap":%t,"period":"CalendarDay",
				"lease":{"grantMilliUSD":%d,"renewInterval":"30s"}}}
		]}}`, limit, hard, grant)
	// Use the current schema's API version rather than a guessed test value.
	doc = strings.Replace(doc, "governance.inferplane.dev/v1alpha1", v1alpha1.APIVersion, 1)
	docs, err := policy.ParseWireDocs([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if tiers {
		docs[0].Spec.Rules = append(docs[0].Spec.Rules, v1alpha1.Rule{
			Name: "switch", FailurePolicy: v1alpha1.FailClosed,
			Routing: &v1alpha1.RoutingRule{BudgetTiers: &v1alpha1.BudgetTiersRule{
				BudgetRef: "cap", EnforceTargets: true,
				Tiers: []v1alpha1.BudgetTier{
					{ThresholdPercent: 50, Substitute: map[string]string{"expensive": "cheap"}},
					{ThresholdPercent: 90, Substitute: map[string]string{"expensive": "cheapest"}},
				},
			}},
		})
	}
	putDocument(t, db, docs[0])
}

func putDocument(t *testing.T, db *pgxpool.Pool, doc v1alpha1.GovernancePolicy) {
	t.Helper()
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ParseWireDocs(body); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), `INSERT INTO policies(name,doc_yaml)
		VALUES($1,$2) ON CONFLICT(name) DO UPDATE SET doc_yaml=excluded.doc_yaml`,
		doc.Metadata.Name, string(body)); err != nil {
		t.Fatal(err)
	}
}

func syncOK(t *testing.T, s *Store, owner string, req policy.AuthorityRequest) policy.SyncResponse {
	t.Helper()
	resp, err := s.Sync(context.Background(), owner, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Authority == nil || resp.Authority.Protocol != policy.AuthorityProtocol {
		t.Fatal("missing authority response")
	}
	return resp
}

func heartbeat(instance string) policy.AuthorityRequest {
	return policy.AuthorityRequest{Protocol: policy.AuthorityProtocol, Instance: instance}
}

func currentBudget(t *testing.T, s *Store) policy.AuthorityBudget {
	t.Helper()
	resp := syncOK(t, s, "node", heartbeat("boot"))
	if len(resp.Authority.Budgets) != 1 {
		t.Fatalf("got %d budgets, want 1", len(resp.Authority.Budgets))
	}
	return resp.Authority.Budgets[0]
}

func grantRequest(b policy.AuthorityBudget, want int64) policy.AuthorityGrantRequest {
	return policy.AuthorityGrantRequest{
		RequestID: nonce(), Key: b.Key, Revision: b.Revision, WindowID: b.WindowID, WantMicroUSD: want,
	}
}

func issue(t *testing.T, s *Store, owner, instance string, r policy.AuthorityGrantRequest) policy.AuthorityGrant {
	t.Helper()
	req := heartbeat(instance)
	req.Requests = []policy.AuthorityGrantRequest{r}
	resp := syncOK(t, s, owner, req)
	if len(resp.Authority.Grants) != 1 {
		t.Fatalf("got %d grants, %d denials", len(resp.Authority.Grants), len(resp.Authority.Denied))
	}
	return resp.Authority.Grants[0]
}

func TestConstructorWithholdsDSN(t *testing.T) {
	secret := "private-password-do-not-print"
	for _, dsn := range []string{"postgres://user:" + secret + "@host:bad/db", "password='" + secret} {
		s, err := New(dsn)
		if s != nil {
			s.Close()
		}
		if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), dsn) {
			t.Fatal("constructor failed to redact an invalid DSN")
		}
	}
}

func TestReadyChecksLiveDatabaseAndClosedPool(t *testing.T) {
	dsn, _ := testDatabase(t)
	s := openStore(t, dsn)
	if err := s.Ready(context.Background()); err != nil {
		t.Fatalf("healthy initialized store is not ready: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Ready(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("readiness did not honor caller cancellation: %v", err)
	}
	s.Close()
	if err := s.Ready(context.Background()); err == nil {
		t.Fatal("closed pool reported ready from cached initialization state")
	}
}

func TestReadyBoundsPoolAcquisitionWithoutCallerDeadline(t *testing.T) {
	dsn, _ := testDatabase(t)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("cannot parse local test database configuration")
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal("cannot construct local test database pool")
	}
	s := &Store{db: pool}
	t.Cleanup(s.Close)
	if err := s.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	done := make(chan error, 1)
	go func() { done <- s.Ready(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("readiness did not time out on unavailable pool capacity: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("readiness has no bounded deadline")
	}
}

func TestConcurrentReplicasReserveAtMostLimit(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 10, true, false)
	a, b := openStore(t, dsn), openStore(t, dsn)
	budget := currentBudget(t, a)
	start := make(chan struct{})
	results := make(chan policy.SyncResponse, 40)
	errs := make(chan error, 40)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := heartbeat("boot")
			req.Requests = []policy.AuthorityGrantRequest{grantRequest(budget, 1)}
			<-start
			resp, err := []*Store{a, b}[i%2].Sync(context.Background(), fmt.Sprintf("node-%d", i), req)
			if err != nil {
				errs <- err
			} else {
				results <- resp
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Error(err)
	}
	var total int64
	for resp := range results {
		for _, grant := range resp.Authority.Grants {
			total += grant.Amount
		}
	}
	if total != 100_000 {
		t.Fatalf("reserved %d, want exactly 100000", total)
	}
}

func TestGrantReplayAfterRestartAndConflicts(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 10, true, false)
	a := openStore(t, dsn)
	budget := currentBudget(t, a)
	r := grantRequest(budget, 60_000)
	first := issue(t, a, "node", "boot", r)
	if first.Amount != 60_000 {
		t.Fatalf("want 60000, got %d", first.Amount)
	}
	a.Close()
	b := openStore(t, dsn)
	replay := issue(t, b, "node", "boot", r)
	if replay.ID != first.ID || replay.Amount != first.Amount || !replay.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatal("restart/retry changed the fixed grant")
	}
	for _, mutate := range []func(*policy.AuthorityGrantRequest){
		func(r *policy.AuthorityGrantRequest) { r.Key += "-other" },
		func(r *policy.AuthorityGrantRequest) { r.WindowID += "-other" },
		func(r *policy.AuthorityGrantRequest) { r.Revision += "-other" },
		func(r *policy.AuthorityGrantRequest) { r.WantMicroUSD++ },
	} {
		bad := r
		mutate(&bad)
		req := heartbeat("boot")
		req.Requests = []policy.AuthorityGrantRequest{bad}
		if _, err := b.Sync(context.Background(), "node", req); err == nil {
			t.Fatal("conflicting replay was accepted")
		} else if strings.Contains(err.Error(), r.RequestID) {
			t.Fatal("request capability leaked in error")
		}
	}
	remaining := issue(t, b, "other-node", "boot", grantRequest(budget, 40_000))
	if remaining.Amount != 40_000 {
		t.Fatalf("retry encumbered extra authority: %d", remaining.Amount)
	}
}

func TestDenyUnusablePartialGrant(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 10, true, false)
	s := openStore(t, dsn)
	budget := currentBudget(t, s)
	issue(t, s, "node", "boot", grantRequest(budget, 95_000))
	req := heartbeat("boot")
	req.Requests = []policy.AuthorityGrantRequest{grantRequest(budget, 6_000)}
	resp := syncOK(t, s, "other", req)
	if len(resp.Authority.Grants) != 0 || len(resp.Authority.Denied) != 1 {
		t.Fatal("issued an unusable partial grant")
	}
	if got := issue(t, s, "other", "boot", grantRequest(budget, 5_000)).Amount; got != 5_000 {
		t.Fatalf("remaining authority = %d, want 5000", got)
	}
}

func TestReportRecoveryRefundExactlyOnceAndOwnership(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 100, true, false)
	s := openStore(t, dsn)
	budget := currentBudget(t, s)
	g := issue(t, s, "node", "original-boot", grantRequest(budget, 1))
	report := policy.AuthorityReport{
		Instance: "original-boot", GrantID: g.ID, RequestID: g.RequestID,
		Sequence: 1, Consumed: 30_000, Observed: 20_000, Closed: true,
	}
	for _, change := range []func(*policy.AuthorityReport, *string){
		func(_ *policy.AuthorityReport, owner *string) { *owner = "other-node" },
		func(r *policy.AuthorityReport, _ *string) { r.Instance = "wrong-boot" },
		func(r *policy.AuthorityReport, _ *string) { r.RequestID = nonce() },
	} {
		bad, owner := report, "node"
		change(&bad, &owner)
		req := heartbeat("recovery-boot")
		req.Reports = []policy.AuthorityReport{bad}
		if _, err := s.Sync(context.Background(), owner, req); err == nil {
			t.Fatal("unauthorized refund was accepted")
		}
	}
	req := heartbeat("recovery-boot")
	req.Reports = []policy.AuthorityReport{report}
	for i := 0; i < 3; i++ {
		resp := syncOK(t, s, "node", req)
		if len(resp.Authority.ReportAcks) != 1 || resp.Authority.ReportAcks[0].Sequence != 1 {
			t.Fatal("missing idempotent report acknowledgement")
		}
	}
	if got := issue(t, s, "other-node", "boot", grantRequest(budget, 70_000)).Amount; got != 70_000 {
		t.Fatalf("closed grant returned incorrect unused amount: %d", got)
	}
	req.Reports[0].Sequence++
	req.Reports[0].Consumed--
	if _, err := s.Sync(context.Background(), "node", req); err == nil {
		t.Fatal("decreasing closed consumption was accepted")
	}
}

func TestFreshPolicyCutAndDeleteReaddPreserveLiability(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 10, true, false)
	a, b := openStore(t, dsn), openStore(t, dsn)
	old := currentBudget(t, b)
	issue(t, a, "node", "boot", grantRequest(old, 60_000))
	putBudget(t, db, 50, 10, true, false)
	fresh := currentBudget(t, b)
	if fresh.LimitMicroUSD != 50_000 || fresh.Key != old.Key || fresh.WindowID != old.WindowID {
		t.Fatal("policy edit reset its budget identity or served a stale limit")
	}
	req := heartbeat("boot")
	req.Requests = []policy.AuthorityGrantRequest{grantRequest(fresh, 1)}
	if got := syncOK(t, b, "other", req); len(got.Authority.Grants) != 0 {
		t.Fatal("policy cut below liabilities allowed another grant")
	}
	if _, err := db.Exec(context.Background(), `DELETE FROM policies`); err != nil {
		t.Fatal(err)
	}
	if got := syncOK(t, b, "other", heartbeat("boot")); len(got.Authority.Budgets) != 0 {
		t.Fatal("deleted policy still distributed")
	}
	putBudget(t, db, 100, 10, true, false)
	readded := currentBudget(t, b)
	if got := issue(t, b, "other", "boot", grantRequest(readded, 40_000)).Amount; got != 40_000 {
		t.Fatalf("re-added policy lost liabilities: grant %d", got)
	}
	req.Requests = []policy.AuthorityGrantRequest{grantRequest(readded, 1)}
	if got := syncOK(t, a, "third", req); len(got.Authority.Grants) != 0 {
		t.Fatal("re-added policy reset historical spend")
	}
}

func TestSoftMetersAndTierLatchSurviveRestart(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 10, false, true)
	a := openStore(t, dsn)
	b := currentBudget(t, a)
	req := heartbeat("boot")
	req.Meters = []policy.AuthorityMeter{{
		Instance: "boot", Key: b.Key, WindowID: b.WindowID,
		Sequence: 1, Consumed: 60_000, Observed: 60_000,
	}}
	resp := syncOK(t, a, "node-a", req)
	if len(resp.ActiveTiers) != 1 || resp.ActiveTiers[0].ThresholdPercent != 50 {
		t.Fatal("soft cumulative consumption did not activate its tier")
	}
	a.Close()
	s := openStore(t, dsn)
	// Replay must not count the same 60% twice.
	resp = syncOK(t, s, "node-a", req)
	if len(resp.ActiveTiers) != 1 || resp.ActiveTiers[0].ThresholdPercent != 50 {
		t.Fatal("meter retry duplicated consumption")
	}
	putBudget(t, db, 200, 10, false, true)
	resp = syncOK(t, s, "node-b", heartbeat("boot"))
	if len(resp.ActiveTiers) != 1 || resp.ActiveTiers[0].ThresholdPercent != 50 {
		t.Fatal("tier latch did not survive restart or policy edit")
	}
	req.Meters[0].Consumed = 40_000
	req.Meters[0].Observed = 40_000
	req.Meters[0].Sequence = 2
	if _, err := s.Sync(context.Background(), "node-a", req); err == nil {
		t.Fatal("meter decrease was accepted")
	}
	req.Requests = []policy.AuthorityGrantRequest{grantRequest(currentBudget(t, s), 1)}
	req.Meters = nil
	if got := syncOK(t, s, "node-a", req); len(got.Authority.Grants) != 0 {
		t.Fatal("soft budget issued hard authority")
	}
}

func TestMalformedBatchRollsBackValidReport(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 100, true, false)
	s := openStore(t, dsn)
	budget := currentBudget(t, s)
	g := issue(t, s, "node", "boot", grantRequest(budget, 1))
	valid := policy.AuthorityReport{Instance: "boot", GrantID: g.ID, RequestID: g.RequestID, Sequence: 1, Closed: true}
	req := heartbeat("boot")
	req.Reports = []policy.AuthorityReport{valid, {Instance: "boot", GrantID: nonce(), RequestID: nonce(), Sequence: 1}}
	if _, err := s.Sync(context.Background(), "node", req); err == nil {
		t.Fatal("unknown grant did not reject the batch")
	}
	req = heartbeat("boot")
	req.Requests = []policy.AuthorityGrantRequest{grantRequest(budget, 1)}
	if got := syncOK(t, s, "other", req); len(got.Authority.Grants) != 0 {
		t.Fatal("malformed batch committed an earlier refund")
	}
}

func TestNegativeAndOverflowReportsFailClosed(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 10, false, false)
	s := openStore(t, dsn)
	b := currentBudget(t, s)
	req := heartbeat("boot")
	req.Meters = []policy.AuthorityMeter{{Instance: "boot", Key: b.Key, WindowID: b.WindowID, Sequence: 1, Consumed: math.MaxInt64, Observed: math.MaxInt64}}
	syncOK(t, s, "node", req)
	req.Meters[0].Consumed, req.Meters[0].Observed = 1, 1
	if _, err := s.Sync(context.Background(), "other-node", req); err == nil {
		t.Fatal("cumulative account overflow was accepted")
	}
	for _, mutate := range []func(*policy.AuthorityMeter){
		func(m *policy.AuthorityMeter) { m.Consumed = -1 },
		func(m *policy.AuthorityMeter) { m.Observed = -1 },
		func(m *policy.AuthorityMeter) { m.Sequence = -1 },
		func(m *policy.AuthorityMeter) { m.Sequence = 0 },
	} {
		m := req.Meters[0]
		mutate(&m)
		req.Meters = []policy.AuthorityMeter{m}
		if _, err := s.Sync(context.Background(), "third-node", req); err == nil {
			t.Fatal("negative or invalid meter accepted")
		}
	}
}

func TestDatabaseUTCWindowAndFixedExpiry(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 10, true, false)
	s := openStore(t, dsn)
	var before time.Time
	if err := db.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	resp := syncOK(t, s, "node", heartbeat("boot"))
	b := resp.Authority.Budgets[0]
	if resp.Authority.ServerTime.Before(before) || resp.Authority.ServerTime.Location() != time.UTC {
		t.Fatal("server time is not database UTC")
	}
	// The database may cross midnight between the first sample and Sync.
	// The response's own timestamp owns its calendar, not the earlier sample.
	now := resp.Authority.ServerTime
	wantStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if !b.WindowStart.Equal(wantStart) || !b.WindowEnd.Equal(wantStart.AddDate(0, 0, 1)) {
		t.Fatal("budget is not in the database's UTC calendar window")
	}
	g := issue(t, s, "node", "boot", grantRequest(b, 1))
	if g.ExpiresAt.After(b.WindowEnd) || !g.ExpiresAt.After(before) {
		t.Fatal("grant expiry escapes its owned budget window")
	}
}

func TestDuplicateGrantRequestsRejectWholeBatch(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 100, true, false)
	s := openStore(t, dsn)
	b := currentBudget(t, s)
	r := grantRequest(b, 1)
	req := heartbeat("boot")
	req.Requests = []policy.AuthorityGrantRequest{r, r}
	if _, err := s.Sync(context.Background(), "node", req); err == nil {
		t.Fatal("duplicate request produced an ambiguous response")
	}
	if got := issue(t, s, "other", "boot", grantRequest(b, 100_000)).Amount; got != 100_000 {
		t.Fatal("malformed duplicate batch reserved authority")
	}
}

func TestExpiryNeverRefundsOrExtendsReplay(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 100, true, false)
	s := openStore(t, dsn)
	doc := syncOK(t, s, "seed", heartbeat("boot")).Policies[0]
	doc.Spec.Rules[0].Budget.Lease.RenewInterval = "1s"
	putDocument(t, db, doc)
	b := currentBudget(t, s)
	r := grantRequest(b, 1)
	first := issue(t, s, "node", "boot", r)
	// Wait on the database's actual clock; this exercises real expiry with
	// the fixture's three-second lease, without changing the machine clock.
	if _, err := db.Exec(context.Background(), `SELECT pg_sleep(GREATEST(0, EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.02)`, first.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	s.Close()
	next := openStore(t, dsn)
	req := heartbeat("new-boot")
	req.Requests = []policy.AuthorityGrantRequest{grantRequest(b, 1)}
	if got := syncOK(t, next, "node", req); len(got.Authority.Grants) != 0 {
		t.Fatal("expiry/restart/deleted node returned uncertain escrow")
	}
	replay := issue(t, next, "node", "boot", r)
	if replay.ID != first.ID || !replay.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatal("expired retry was re-armed")
	}
}

func TestReportSequencesAndPostMutationBatchRollback(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 100, true, false)
	s := openStore(t, dsn)
	b := currentBudget(t, s)
	g := issue(t, s, "node", "boot", grantRequest(b, 1))
	report := policy.AuthorityReport{Instance: "boot", GrantID: g.ID, RequestID: g.RequestID, Sequence: 2, Consumed: 50_000, Observed: 20_000}
	req := heartbeat("boot")
	req.Reports = []policy.AuthorityReport{report}
	syncOK(t, s, "node", req)
	for _, change := range []func(*policy.AuthorityReport){
		func(r *policy.AuthorityReport) { r.Sequence = 1 },
		func(r *policy.AuthorityReport) { r.Consumed++ },
		func(r *policy.AuthorityReport) { r.Sequence++; r.Consumed-- },
		func(r *policy.AuthorityReport) { r.Sequence++; r.Observed-- },
		func(r *policy.AuthorityReport) { r.Sequence++; r.Consumed = 100_001 },
		func(r *policy.AuthorityReport) { r.Sequence++; r.Closed = true; r.Consumed = -1 },
	} {
		bad := report
		change(&bad)
		req.Reports = []policy.AuthorityReport{bad}
		if _, err := s.Sync(context.Background(), "node", req); err == nil {
			t.Fatal("nonmonotonic or invalid report accepted")
		}
	}
	report.Sequence++
	report.Closed = true
	req.Reports = []policy.AuthorityReport{report}
	// This meter passes shape validation and account lookup; it fails only
	// AFTER the report checkpoint has been written in the same transaction.
	req.Meters = []policy.AuthorityMeter{{Instance: "boot", Key: b.Key, WindowID: b.WindowID, Sequence: 1}}
	if _, err := s.Sync(context.Background(), "node", req); err == nil {
		t.Fatal("hard-budget meter was accepted")
	}
	other := heartbeat("boot")
	other.Requests = []policy.AuthorityGrantRequest{grantRequest(b, 1)}
	if got := syncOK(t, s, "other", other); len(got.Authority.Grants) != 0 {
		t.Fatal("failed batch committed its earlier closed-grant refund")
	}
	req.Meters = nil
	syncOK(t, s, "node", req)
	req.Reports[0].Sequence++
	// An unchanged terminal report may advance its durable checkpoint, but
	// must not refund again.
	syncOK(t, s, "node", req)
	issue(t, s, "other", "boot", grantRequest(b, 50_000))
	other.Requests = []policy.AuthorityGrantRequest{grantRequest(b, 1)}
	if got := syncOK(t, s, "third", other); len(got.Authority.Grants) != 0 {
		t.Fatal("terminal report checkpoint issued a second refund")
	}
}

func TestClosedOverrunFreezesRemainingWindow(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 10, true, true)
	s := openStore(t, dsn)
	b := currentBudget(t, s)
	g := issue(t, s, "node", "boot", grantRequest(b, 1))
	req := heartbeat("boot")
	req.Reports = []policy.AuthorityReport{{
		Instance: "boot", GrantID: g.ID, RequestID: g.RequestID, Sequence: 1,
		Consumed: 20_000, Observed: 20_000, Closed: true, Overrun: true,
	}}
	syncOK(t, s, "node", req)
	syncOK(t, s, "node", req)
	s.Close()
	next := openStore(t, dsn)
	// Eighty percent remains under the configured limit, but the bound
	// violation independently freezes new authority.
	req = heartbeat("boot")
	req.Requests = []policy.AuthorityGrantRequest{grantRequest(b, 1)}
	if got := syncOK(t, next, "other", req); len(got.Authority.Grants) != 0 || got.Authority.Denied[0].Reason != "frozen_window" {
		t.Fatal("overrun did not durably freeze the window")
	}
	var retained int64
	if err := db.QueryRow(context.Background(), `SELECT hard_encumbered FROM authority_accounts WHERE budget_key=$1 AND window_id=$2`, b.Key, b.WindowID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 20_000 {
		t.Fatalf("overrun accounting was refunded, duplicated or wrapped: %d", retained)
	}
}

func TestRolloverAndLateReportsUseOriginalWindow(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 100, true, false)
	s := openStore(t, dsn)
	snapshot := syncOK(t, s, "node", heartbeat("boot"))
	b := snapshot.Authority.Budgets[0]
	g := issue(t, s, "node", "old-boot", grantRequest(b, 1))
	old, err := policy.AuthorityBudgets(snapshot.Policies, snapshot.Authority.ServerTime.AddDate(0, 0, -1))
	if err != nil {
		t.Fatal(err)
	}
	// Move the committed fixture to its prior UTC day, exactly as if the DB
	// had been reopened after midnight. The public Sync clock is never faked.
	tx, err := db.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), `UPDATE authority_accounts SET window_id=$2 WHERE budget_key=$1`, b.Key, old[0].WindowID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `UPDATE authority_requests SET window_id=$2,expires_at=$3 WHERE grant_id=$1`, g.ID, old[0].WindowID, old[0].WindowEnd); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	issue(t, s, "new-node", "new-boot", grantRequest(b, 100_000))
	req := heartbeat("recovery-boot")
	req.Reports = []policy.AuthorityReport{{Instance: "old-boot", GrantID: g.ID, RequestID: g.RequestID, Sequence: 1, Consumed: 25_000, Observed: 20_000, Closed: true}}
	syncOK(t, s, "node", req)
	var oldEncumbered, currentEncumbered int64
	if err := db.QueryRow(context.Background(), `SELECT hard_encumbered FROM authority_accounts WHERE budget_key=$1 AND window_id=$2`, b.Key, old[0].WindowID).Scan(&oldEncumbered); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(context.Background(), `SELECT hard_encumbered FROM authority_accounts WHERE budget_key=$1 AND window_id=$2`, b.Key, b.WindowID).Scan(&currentEncumbered); err != nil {
		t.Fatal(err)
	}
	if oldEncumbered != 25_000 || currentEncumbered != 100_000 {
		t.Fatalf("late report crossed windows: old=%d, current=%d", oldEncumbered, currentEncumbered)
	}
	req = heartbeat("boot")
	req.Requests = []policy.AuthorityGrantRequest{grantRequest(b, 1)}
	if got := syncOK(t, s, "third", req); len(got.Authority.Grants) != 0 {
		t.Fatal("old-window refund authorized new-window spending")
	}
}

func TestPolicyWriterBlocksSyncUntilFreshCommittedRead(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 100, true, false)
	s := openStore(t, dsn)
	snapshot := syncOK(t, s, "node", heartbeat("boot"))
	old := snapshot.Authority.Budgets[0]
	tx, err := db.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var writerPID int32
	if err := tx.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&writerPID); err != nil {
		t.Fatal(err)
	}
	doc := snapshot.Policies[0]
	doc.Spec.Rules[0].Budget.LimitMilliUSD = 50
	doc.Spec.Rules[0].Budget.Lease.GrantMilliUSD = 50
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `UPDATE policies SET doc_yaml=$1`, string(body)); err != nil {
		t.Fatal(err)
	}
	type result struct {
		resp policy.SyncResponse
		err  error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		req := heartbeat("boot")
		req.Requests = []policy.AuthorityGrantRequest{grantRequest(old, 100_000)}
		r, err := s.Sync(ctx, "node", req)
		done <- result{r, err}
	}()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		var waiting bool
		if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
			WHERE relation='policies'::regclass AND mode='ShareLock' AND NOT granted
			AND $1 = ANY(pg_blocking_pids(pid)))`, writerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-done:
			t.Fatal("sync read policy without waiting for its write transaction")
		case <-ctx.Done():
			t.Fatal("sync did not reach its policy lock")
		case <-poll.C:
		}
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got result
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatal("sync did not finish after the policy writer committed")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.resp.Authority.Budgets[0].LimitMicroUSD != 50_000 || len(got.resp.Authority.Grants) != 0 {
		t.Fatal("waiting sync widened authority using pre-commit policy")
	}
}

func TestDeniedIntentCanRetryAfterProvenRefund(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 100, true, false)
	s := openStore(t, dsn)
	b := currentBudget(t, s)
	g := issue(t, s, "first", "boot", grantRequest(b, 1))
	r := grantRequest(b, 100_000)
	req := heartbeat("boot")
	req.Requests = []policy.AuthorityGrantRequest{r}
	if got := syncOK(t, s, "second", req); len(got.Authority.Grants) != 0 {
		t.Fatal("exhausted request was granted")
	}
	report := heartbeat("boot")
	report.Reports = []policy.AuthorityReport{{Instance: "boot", GrantID: g.ID, RequestID: g.RequestID, Sequence: 1, Closed: true}}
	syncOK(t, s, "first", report)
	recovered := issue(t, s, "second", "boot", r)
	if recovered.Amount != 100_000 {
		t.Fatal("retry could not acquire newly proven unused authority")
	}
	if replay := issue(t, s, "second", "boot", r); replay.ID != recovered.ID {
		t.Fatal("successful retry was not idempotent")
	}
}

func TestAuthorityEnvelopeIncludesCompletePolicySetAndEmptyClear(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 10, true, false)
	s := openStore(t, dsn)
	initial := syncOK(t, s, "node", heartbeat("boot"))
	other := initial.Policies[0]
	other.Metadata.Name = "another-policy"
	putDocument(t, db, other)
	resp := syncOK(t, s, "node", heartbeat("boot"))
	if len(resp.Authority.Policies) != 2 || resp.Authority.Policies[0].Metadata.Name != "another-policy" ||
		resp.Authority.Policies[1].Metadata.Name != "budget" || len(resp.Authority.Budgets) != 2 {
		t.Fatal("authority envelope omitted or reordered part of the authoritative policy set")
	}
	if resp.Authority.Generation != resp.Generation || resp.Authority.Generation != policy.GenerationOf(resp.Authority.Policies) {
		t.Fatal("inner and outer policy generations disagree")
	}
	if _, err := db.Exec(context.Background(), `DELETE FROM policies`); err != nil {
		t.Fatal(err)
	}
	resp = syncOK(t, s, "node", heartbeat("boot"))
	if resp.Authority.Policies == nil || len(resp.Authority.Policies) != 0 ||
		resp.Authority.Budgets == nil || len(resp.Authority.Budgets) != 0 ||
		resp.Authority.Generation != resp.Generation || resp.Authority.Generation != policy.GenerationOf(nil) {
		t.Fatal("deleted policy set did not produce an explicit, verifiable empty bundle")
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var encoded struct {
		Authority map[string]json.RawMessage `json:"authority"`
	}
	if err := json.Unmarshal(body, &encoded); err != nil {
		t.Fatal(err)
	}
	if string(encoded.Authority["policies"]) != "[]" || string(encoded.Authority["budgets"]) != "[]" {
		t.Fatal("wire bundle cannot unambiguously clear the complete policy set")
	}
}

func TestClosedGrantStaleJournalBurnAcknowledgesWithoutChangingLiability(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 100, true, false)
	s := openStore(t, dsn)
	b := currentBudget(t, s)
	intent := grantRequest(b, 1)
	g := issue(t, s, "node", "original-boot", intent)
	closed := heartbeat("original-boot")
	closed.Reports = []policy.AuthorityReport{{
		Instance: "original-boot", GrantID: g.ID, RequestID: g.RequestID,
		Sequence: 10, Consumed: 30_000, Observed: 20_000, Closed: true,
	}}
	syncOK(t, s, "node", closed)
	// The server already returned 70000 of proven unused escrow. A stale
	// journal can retain a pre-close copy with both older sequence and usage.
	recovery := heartbeat("recovery-boot")
	recovery.Reports = []policy.AuthorityReport{{
		Instance: "original-boot", GrantID: g.ID, RequestID: g.RequestID,
		Sequence: 2, Consumed: g.Amount, Observed: 10_000, Closed: true,
	}}
	unauthorized := recovery
	unauthorized.Reports = append([]policy.AuthorityReport(nil), recovery.Reports...)
	unauthorized.Reports[0].RequestID = nonce()
	if _, err := s.Sync(context.Background(), "node", unauthorized); err == nil {
		t.Fatal("closed recovery bypassed the grant capability")
	}
	if _, err := s.Sync(context.Background(), "different-node", recovery); err == nil {
		t.Fatal("closed recovery bypassed the original dataplane owner")
	}
	for i := 0; i < 2; i++ {
		resp := syncOK(t, s, "node", recovery)
		if len(resp.Authority.ReportAcks) != 1 ||
			resp.Authority.ReportAcks[0].Sequence != 2 ||
			resp.Authority.ReportAcks[0].Instance != "original-boot" ||
			len(resp.Authority.Grants) != 0 {
			t.Fatal("recovery acknowledgement cannot retire the client's original outbox")
		}
	}
	var consumed, observed, sequence int64
	if err := db.QueryRow(context.Background(), `SELECT consumed,observed,sequence FROM authority_requests WHERE grant_id=$1`, g.ID).
		Scan(&consumed, &observed, &sequence); err != nil {
		t.Fatal(err)
	}
	if consumed != 30_000 || observed != 20_000 || sequence != 10 {
		t.Fatalf("stale burn rewrote finalized accounting: consumed=%d observed=%d sequence=%d", consumed, observed, sequence)
	}
	// Further observation can refine uncertain consumed authority, but cannot
	// exceed the server's finalized consumption without an explicit overrun.
	recovery.Reports[0].Sequence = 12
	recovery.Reports[0].Observed = 25_000
	syncOK(t, s, "node", recovery)
	recovery.Reports[0].Sequence = 13
	recovery.Reports[0].Observed = 5_000
	syncOK(t, s, "node", recovery)
	if err := db.QueryRow(context.Background(), `SELECT consumed,observed,sequence FROM authority_requests WHERE grant_id=$1`, g.ID).
		Scan(&consumed, &observed, &sequence); err != nil {
		t.Fatal(err)
	}
	if consumed != 30_000 || observed != 25_000 || sequence != 13 {
		t.Fatalf("recovery did not retain monotonic server accounting: consumed=%d observed=%d sequence=%d", consumed, observed, sequence)
	}
	recovery.Reports[0].Sequence = 14
	recovery.Reports[0].Observed = 30_001
	if _, err := s.Sync(context.Background(), "node", recovery); err == nil {
		t.Fatal("recovery hid observation above the server's finalized consumption")
	}
	recovery.Reports[0].Observed = 25_000
	recovery.Instance = "original-boot"
	if _, err := s.Sync(context.Background(), "node", recovery); err == nil {
		t.Fatal("same-boot report masqueraded as a synthetic recovery burn")
	}
	// A closed grant never re-arms, and the original unused balance must be
	// available exactly once after a store restart.
	replay := heartbeat("original-boot")
	replay.Requests = []policy.AuthorityGrantRequest{intent}
	resp := syncOK(t, s, "node", replay)
	if len(resp.Authority.Grants) != 0 || len(resp.Authority.Denied) != 1 || resp.Authority.Denied[0].Reason != "closed_grant" {
		t.Fatal("recovery re-armed the original grant")
	}
	s.Close()
	next := openStore(t, dsn)
	if grant := issue(t, next, "other", "boot", grantRequest(b, 70_000)); grant.Amount != 70_000 {
		t.Fatal("recovery recharged previously refunded credit")
	}
	extra := heartbeat("boot")
	extra.Requests = []policy.AuthorityGrantRequest{grantRequest(b, 1)}
	if resp := syncOK(t, next, "third", extra); len(resp.Authority.Grants) != 0 {
		t.Fatal("recovery refunded the finalized grant twice")
	}
}

func TestClosedGrantLateOverrunAccountsOnlyNewLiabilityAndFreezes(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 200, 50, true, false)
	s := openStore(t, dsn)
	b := currentBudget(t, s)
	g := issue(t, s, "node", "original-boot", grantRequest(b, 1))
	normal := heartbeat("original-boot")
	normal.Reports = []policy.AuthorityReport{{
		Instance: "original-boot", GrantID: g.ID, RequestID: g.RequestID,
		Sequence: 10, Consumed: 10_000, Observed: 8_000, Closed: true,
	}}
	syncOK(t, s, "node", normal)
	issue(t, s, "other", "boot", grantRequest(b, 50_000))
	overrun := heartbeat("recovery-boot")
	overrun.Reports = []policy.AuthorityReport{{
		Instance: "original-boot", GrantID: g.ID, RequestID: g.RequestID,
		Sequence: 11, Consumed: 70_000, Observed: 70_000, Closed: true, Overrun: true,
	}}
	for i := 0; i < 2; i++ {
		syncOK(t, s, "node", overrun)
	}
	var encumbered, consumed int64
	var frozen bool
	if err := db.QueryRow(context.Background(), `SELECT hard_encumbered,hard_consumed,frozen
		FROM authority_accounts WHERE budget_key=$1 AND window_id=$2`, b.Key, b.WindowID).
		Scan(&encumbered, &consumed, &frozen); err != nil {
		t.Fatal(err)
	}
	if encumbered != 120_000 || consumed != 70_000 || !frozen {
		t.Fatalf("late overrun liability mismatch: encumbered=%d consumed=%d frozen=%t", encumbered, consumed, frozen)
	}
	extra := heartbeat("boot")
	extra.Requests = []policy.AuthorityGrantRequest{grantRequest(b, 1)}
	if resp := syncOK(t, s, "third", extra); len(resp.Authority.Grants) != 0 ||
		len(resp.Authority.Denied) != 1 || resp.Authority.Denied[0].Reason != "frozen_window" {
		t.Fatal("late overrun failed to freeze the remaining window balance")
	}
}
