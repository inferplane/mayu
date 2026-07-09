package pgstore

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/inferplane/inferplane/internal/analytics"
	"github.com/inferplane/inferplane/internal/audit"
)

// testDSN returns the local test Postgres DSN, skipping the test if unset —
// the zero-dependency default path never requires Postgres (ADR-013).
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("INFERPLANE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("INFERPLANE_TEST_PG_DSN not set; skipping Postgres-backed test")
	}
	return dsn
}

// newTestStore opens a Store against the test DSN and resets ALL state,
// including the lease row, so tests don't see holder/epoch/expiry left
// behind by a previous test function. Rebuild() deliberately does NOT reset
// the lease row (production Rebuild must not strip leadership) — tests need
// a stronger reset than production Rebuild provides, so this also deletes
// the lease row directly; the next tryAcquireLease call re-creates it via
// the first-ever-row path.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := testDSN(t)
	s, err := New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild (test setup truncate): %v", err)
	}
	if _, err := s.db.Exec(context.Background(), `DELETE FROM lease`); err != nil {
		t.Fatalf("reset lease (test isolation): %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// rawPool opens an independent connection to the same test DSN, for tests to
// seed/inspect tables directly without needing pgstore to export internals.
func rawPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), testDSN(t))
	if err != nil {
		t.Fatalf("rawPool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func completedRecord(id, ts, team, model string, in, out, cost int64) audit.Record {
	return audit.Record{
		Event:     "request_completed",
		ID:        id,
		TS:        ts,
		Principal: audit.PrincipalRef{Team: team},
		Request:   audit.RequestRef{ModelResolved: model},
		Usage:     &audit.UsageRef{InputTokens: in, OutputTokens: out},
		Cost:      &audit.CostRef{AmountUSDMicros: cost},
	}
}

func TestNewCreatesSchemaIdempotently(t *testing.T) {
	dsn := testDSN(t)
	s1, err := New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	defer s1.Close()
	s2, err := New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("second New (must be idempotent): %v", err)
	}
	defer s2.Close()
}

func TestStoreSatisfiesAnalyticsStoreAndRebuilder(t *testing.T) {
	var _ analytics.Store = (*Store)(nil)
	var _ analytics.Rebuilder = (*Store)(nil)
}

func TestUpsertEventCorrectsCostOnReingest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	epoch, ok, err := tryAcquireLease(ctx, s.db, "h1", 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	r := completedRecord("01UPSERT", "2026-07-07T10:00:00Z", "alpha", "m1", 100, 50, 1000)
	if err := s.ingestBatch(ctx, "h1", epoch, []audit.Record{r}, map[string]int64{"seg-a": 100}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	// Re-ingest the SAME id with a corrected cost — must UPDATE, not ignore.
	r.Cost.AmountUSDMicros = 2000
	if err := s.ingestBatch(ctx, "h1", epoch, []audit.Record{r}, map[string]int64{"seg-a": 200}); err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	sum, err := s.Summary(analytics.SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Totals.Requests != 1 {
		t.Fatalf("requests = %d, want 1 (same ULID, corrected not duplicated)", sum.Totals.Requests)
	}
	if sum.Totals.CostMicros != 2000 {
		t.Fatalf("cost = %d, want 2000 (upsert must overwrite, not insert-or-ignore)", sum.Totals.CostMicros)
	}
}

// TestUpsertEventQueryPreservesBodyRefPattern pins the exact SQL fix without
// needing a live Postgres — a re-ingest of a pre-D4 record (BodyRef="") must
// not clobber a previously-captured body_ref. Runs unconditionally (no DSN).
func TestUpsertEventQueryPreservesBodyRefPattern(t *testing.T) {
	src, err := os.ReadFile("pgstore.go")
	if err != nil {
		t.Fatalf("read pgstore.go: %v", err)
	}
	q := string(src)
	if !strings.Contains(q, "NULLIF(excluded.body_ref, '')") || !strings.Contains(q, "events.body_ref)") {
		t.Fatalf("upsertEvent's ON CONFLICT DO UPDATE must preserve body_ref via " +
			"COALESCE(NULLIF(excluded.body_ref, ''), events.body_ref) — got a query " +
			"that unconditionally overwrites it with the incoming (possibly empty) value")
	}
}

// TestUpsertEventPreservesBodyRefOnEmptyReingest is the Postgres-backed
// regression test: re-ingesting the same record ID with an empty BodyRef (the
// shape of a pre-D4 audit record) must not erase a previously-captured ref.
func TestUpsertEventPreservesBodyRefOnEmptyReingest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	epoch, ok, err := tryAcquireLease(ctx, s.db, "h1", 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	ref := "01BODYREF"
	r := completedRecord("01REBUILD", "2026-07-09T10:00:00Z", "alpha", "m1", 10, 5, 500)
	r.BodyRef = &ref
	if err := s.ingestBatch(ctx, "h1", epoch, []audit.Record{r}, map[string]int64{"seg-a": 100}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	// Re-ingest the SAME id as a pre-D4 record would parse: BodyRef unset.
	r2 := completedRecord("01REBUILD", "2026-07-09T10:00:00Z", "alpha", "m1", 10, 5, 500)
	if err := s.ingestBatch(ctx, "h1", epoch, []audit.Record{r2}, map[string]int64{"seg-a": 200}); err != nil {
		t.Fatalf("second ingest (rebuild replay): %v", err)
	}
	rows, err := s.Recent(10, "")
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	var got string
	for _, e := range rows {
		if e.ID == "01REBUILD" {
			got = e.BodyRef
		}
	}
	if got != ref {
		t.Fatalf("body_ref after empty re-ingest = %q, want %q (a rebuild replay must not erase a captured body_ref)", got, ref)
	}
}

func TestSummaryTimeSeriesReadFromRollupNotLiveEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pool := rawPool(t)
	// Seed rollup_day directly with a value events-alone wouldn't produce —
	// proves the query reads rollup_day, not a live GROUP BY over events.
	_, err := pool.Exec(ctx, `INSERT INTO rollup_day(day,team,model,input_tokens,output_tokens,cost_micros,request_count)
		VALUES('2026-07-07','planted','m',0,0,999999,7)`)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := s.Summary(analytics.SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Totals.CostMicros != 999999 || sum.Totals.Requests != 7 {
		t.Fatalf("Summary = %+v, want the planted rollup_day row reflected (proves rollup-backed, not live events GROUP BY)", sum.Totals)
	}
}

func TestHealthReflectsCheckpointsAndLease(t *testing.T) {
	s := newTestStore(t)
	s.instanceID = "holderA" // Health's IsLeader compares holder against s.instanceID
	ctx := context.Background()
	h, err := s.Health()
	if err != nil {
		t.Fatal(err)
	}
	if h.Mode != "B" || h.SegmentsTracked != 0 {
		t.Fatalf("pre-ingest health = %+v, want Mode=B SegmentsTracked=0", h)
	}
	if h.IsLeader {
		t.Fatal("IsLeader = true with no lease row at all, want false")
	}
	epoch, ok, err := tryAcquireLease(ctx, s.db, "holderA", 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("tryAcquireLease: ok=%v err=%v", ok, err)
	}
	r := completedRecord("01H", "2026-07-07T10:00:00Z", "alpha", "m1", 1, 1, 1)
	if err := s.ingestBatch(ctx, "holderA", epoch, []audit.Record{r}, map[string]int64{"seg-a": 42}); err != nil {
		t.Fatal(err)
	}
	h, err = s.Health()
	if err != nil {
		t.Fatal(err)
	}
	if h.SegmentsTracked != 1 || h.LastIngestTS == "" || h.LeaseEpoch == 0 {
		t.Fatalf("post-ingest health = %+v, want SegmentsTracked=1, non-empty LastIngestTS, nonzero LeaseEpoch", h)
	}
	// This is the actual claim the PR fixed: IsLeader must be true for the
	// live holder whose instanceID matches, computed on the DB's clock.
	if !h.IsLeader {
		t.Fatalf("IsLeader = false, want true (holder=%q instanceID=%q, lease live)", "holderA", s.instanceID)
	}

	// A different identity must never see itself as leader, even though the
	// lease is currently live for someone else.
	other := &Store{db: s.db, instanceID: "someone-else"}
	if h2, err := other.Health(); err != nil || h2.IsLeader {
		t.Fatalf("IsLeader for a non-holder instanceID = %v (err=%v), want false", h2.IsLeader, err)
	}

	// Expire the lease (same holder, negative TTL self-expires expires_at —
	// same pattern as lease_test.go) — IsLeader must flip to false once the
	// DB's own clock says the lease is no longer live, proving Health() reads
	// liveness from SQL (`expires_at > now()`), not the app's time.Now().
	if _, _, err := tryAcquireLease(ctx, s.db, "holderA", -1*time.Second); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	h, err = s.Health()
	if err != nil {
		t.Fatal(err)
	}
	if h.IsLeader {
		t.Fatal("IsLeader = true after the lease expired, want false")
	}
}

func TestRebuildTruncatesAndBumpsEpochWithoutChangingHolder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	epoch1, ok, err := tryAcquireLease(ctx, s.db, "holderA", 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	r := completedRecord("01R", "2026-07-07T10:00:00Z", "alpha", "m1", 1, 1, 1)
	if err := s.ingestBatch(ctx, "holderA", epoch1, []audit.Record{r}, map[string]int64{"seg-a": 10}); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	sum, _ := s.Summary(analytics.SummaryQuery{})
	if sum.Totals.Requests != 0 {
		t.Fatalf("events survived Rebuild: %+v", sum.Totals)
	}
	h, _ := s.Health()
	if h.SegmentsTracked != 0 {
		t.Fatalf("checkpoints survived Rebuild: %+v", h)
	}
	var holder string
	var epoch2 int64
	if err := s.db.QueryRow(ctx, `SELECT holder, epoch FROM lease WHERE id='mode_b_aggregator'`).Scan(&holder, &epoch2); err != nil {
		t.Fatal(err)
	}
	if holder != "holderA" {
		t.Fatalf("Rebuild must not change holder, got %q", holder)
	}
	if epoch2 != epoch1+1 {
		t.Fatalf("Rebuild must bump epoch by exactly 1: before=%d after=%d", epoch1, epoch2)
	}
}

func TestIngestBatchCrashRecoveryIdempotent(t *testing.T) {
	dsn := testDSN(t)
	s1, err := New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s1.db.Exec(ctx, `DELETE FROM lease`); err != nil {
		t.Fatalf("reset lease (test isolation): %v", err)
	}
	epoch, ok, err := tryAcquireLease(ctx, s1.db, "h1", 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	r := completedRecord("01CRASH", "2026-07-07T10:00:00Z", "alpha", "m1", 10, 5, 500)
	batch := []audit.Record{r}
	cps := map[string]int64{"seg-a": 300}
	if err := s1.ingestBatch(ctx, "h1", epoch, batch, cps); err != nil {
		t.Fatal(err)
	}
	s1.Close() // simulate a process restart

	s2, err := New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	// Re-ingest the SAME batch (as if the aggregator restarted and redid a
	// tick it wasn't sure completed) — must be a no-op, not a double-count.
	// Same holder, so re-acquiring is a renewal (epoch unchanged, per the
	// CASE fix) — the fencing check still matches.
	epoch2, ok, err := tryAcquireLease(ctx, s2.db, "h1", 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("re-acquire after restart: ok=%v err=%v", ok, err)
	}
	if err := s2.ingestBatch(ctx, "h1", epoch2, batch, cps); err != nil {
		t.Fatal(err)
	}
	sum, err := s2.Summary(analytics.SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Totals.Requests != 1 || sum.Totals.CostMicros != 500 {
		t.Fatalf("re-applied batch must be idempotent, got %+v", sum.Totals)
	}
}
