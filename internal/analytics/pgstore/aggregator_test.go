package pgstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/analytics"
	"github.com/inferplane/inferplane/internal/audit"
)

func line(id, ts, team, model string, in, out, cost int64) string {
	r := completedRecord(id, ts, team, model, in, out, cost)
	b, _ := r.Canonical()
	return string(b) + "\n"
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestAggregator(t *testing.T, s *Store, dir string) *Aggregator {
	t.Helper()
	return NewAggregator(s, AggregatorConfig{
		AggregatedAuditDir: dir,
		PollInterval:       time.Hour, // tests call tick() directly, never rely on the sleep loop
		LeaseTTL:           15 * time.Second,
		MaxLinesPerTick:    5000,
	})
}

func TestTickIngestsOnlyFreshSegmentAndIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	writeFile(t, dir, "seg-a.jsonl", line("01A", "2026-07-07T10:00:00Z", "alpha", "m1", 1, 1, 100))
	writeFile(t, dir, "seg-b.jsonl", line("01B", "2026-07-07T10:00:00Z", "alpha", "m1", 2, 2, 200))
	ctx := context.Background()

	// Pre-seed seg-a as fully consumed (its one line already ingested).
	segAContent, _ := os.ReadFile(filepath.Join(dir, "seg-a.jsonl"))
	if _, err := s.db.Exec(ctx, `INSERT INTO checkpoints(segment, byte_offset, updated_at) VALUES ('seg-a.jsonl', $1, now())`, len(segAContent)); err != nil {
		t.Fatal(err)
	}

	ag := newTestAggregator(t, s, dir)
	if err := ag.tick(ctx); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	sum, err := s.Summary(analytics.SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Totals.Requests != 1 {
		t.Fatalf("requests = %d, want 1 (only seg-b's fresh line)", sum.Totals.Requests)
	}

	// A second identical tick must be a no-op (both segments now fully consumed).
	if err := ag.tick(ctx); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	sum, err = s.Summary(analytics.SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Totals.Requests != 1 {
		t.Fatalf("requests after no-op tick = %d, want still 1", sum.Totals.Requests)
	}
}

func TestTickPinsCheckpointBeforePartialLineAndResumesLater(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	complete := line("01C", "2026-07-07T10:00:00Z", "alpha", "m1", 1, 1, 100)
	partial := `{"event":"request_completed","id":"01D","ts":"2026-07-07T10:0` // no trailing \n, deliberately incomplete
	writeFile(t, dir, "seg-a.jsonl", complete+partial)
	ctx := context.Background()
	ag := newTestAggregator(t, s, dir)

	if err := ag.tick(ctx); err != nil {
		t.Fatal(err)
	}
	var offset int64
	if err := s.db.QueryRow(ctx, `SELECT byte_offset FROM checkpoints WHERE segment='seg-a.jsonl'`).Scan(&offset); err != nil {
		t.Fatal(err)
	}
	if int(offset) != len(complete) {
		t.Fatalf("checkpoint = %d, want exactly %d (byte position immediately before the incomplete line, never past it)", offset, len(complete))
	}
	sum, _ := s.Summary(analytics.SummaryQuery{})
	if sum.Totals.Requests != 1 {
		t.Fatalf("requests = %d, want 1 (only the complete line)", sum.Totals.Requests)
	}

	// Complete the partial line and append a trailing newline — a later tick
	// must ingest it, proving nothing was permanently lost.
	rest := `0:01Z","principal":{"team":"alpha"},"request":{"model_resolved":"m1"},"usage":{"input_tokens":1,"output_tokens":1},"cost":{"amount_usd_micros":50}}` + "\n"
	f, err := os.OpenFile(filepath.Join(dir, "seg-a.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(rest); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := ag.tick(ctx); err != nil {
		t.Fatal(err)
	}
	sum, _ = s.Summary(analytics.SummaryQuery{})
	if sum.Totals.Requests != 2 {
		t.Fatalf("requests after completing the line = %d, want 2", sum.Totals.Requests)
	}
}

func TestTickAdvancesBothSegmentsCheckpointsInOneTransaction(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	writeFile(t, dir, "seg-a.jsonl", line("01A", "2026-07-07T10:00:00Z", "alpha", "m1", 1, 1, 100))
	writeFile(t, dir, "seg-b.jsonl", line("01B", "2026-07-07T10:00:00Z", "alpha", "m1", 1, 1, 100))
	ctx := context.Background()
	ag := newTestAggregator(t, s, dir)
	if err := ag.tick(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM checkpoints WHERE byte_offset > 0`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("checkpoints advanced = %d, want 2 (both segments in the one tick's transaction)", n)
	}
}

func TestTickMalformedOnlyLinesStillAdvanceCheckpoint(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	malformed := "not json\n{\"also\":\"not a record\"}garbage\n"
	valid := line("01V", "2026-07-07T10:00:00Z", "alpha", "m1", 1, 1, 100)
	writeFile(t, dir, "seg-a.jsonl", malformed+valid)
	ctx := context.Background()
	ag := newTestAggregator(t, s, dir)

	if err := ag.tick(ctx); err != nil {
		t.Fatal(err)
	}
	var offset int64
	if err := s.db.QueryRow(ctx, `SELECT byte_offset FROM checkpoints WHERE segment='seg-a.jsonl'`).Scan(&offset); err != nil {
		t.Fatal(err)
	}
	if int(offset) != len(malformed)+len(valid) {
		t.Fatalf("checkpoint = %d, want %d — malformed-only lines must still advance the checkpoint (else permanent starvation)", offset, len(malformed)+len(valid))
	}
	sum, _ := s.Summary(analytics.SummaryQuery{})
	if sum.Totals.Requests != 1 {
		t.Fatalf("requests = %d, want 1 (the one valid line)", sum.Totals.Requests)
	}
}

func TestTickRacingRebuildIsFenced(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	writeFile(t, dir, "seg-a.jsonl", line("01A", "2026-07-07T10:00:00Z", "alpha", "m1", 1, 1, 100))
	ctx := context.Background()
	ag := newTestAggregator(t, s, dir)

	// Simulate a tick that already captured its epoch (via tryAcquireLease)
	// before a Rebuild runs concurrently and bumps the epoch.
	epoch, ok, err := tryAcquireLease(ctx, s.db, ag.instanceID, 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	// The tick's ingest, still using the pre-rebuild epoch, must be fenced —
	// not silently write a stale checkpoint into the freshly truncated table.
	content, _ := os.ReadFile(filepath.Join(dir, "seg-a.jsonl"))
	err = s.ingestBatch(ctx, ag.instanceID, epoch, []audit.Record{completedRecord("01A", "2026-07-07T10:00:00Z", "alpha", "m1", 1, 1, 100)}, map[string]int64{"seg-a.jsonl": int64(len(content))})
	if err == nil {
		t.Fatal("post-rebuild ingestBatch with the pre-rebuild epoch must fail (errFenced)")
	}
}

func TestTickWhenNotLeaderIsGracefulNoop(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	writeFile(t, dir, "seg-a.jsonl", line("01A", "2026-07-07T10:00:00Z", "alpha", "m1", 1, 1, 100))
	ctx := context.Background()

	if _, ok, err := tryAcquireLease(ctx, s.db, "someone-else", 15*time.Second); err != nil || !ok {
		t.Fatalf("seed lease: ok=%v err=%v", ok, err)
	}
	ag := newTestAggregator(t, s, dir)
	ag.instanceID = "not-the-leader"
	// someone-else's lease is fresh (15s TTL), so ag can't win it.
	if err := ag.tick(ctx); err != nil {
		t.Fatalf("non-leader tick must return nil, not an error: %v", err)
	}
	sum, _ := s.Summary(analytics.SummaryQuery{})
	if sum.Totals.Requests != 0 {
		t.Fatalf("non-leader tick must not ingest anything, got %d requests", sum.Totals.Requests)
	}
}
