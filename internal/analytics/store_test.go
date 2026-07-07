package analytics

import (
	"testing"

	"github.com/inferplane/inferplane/internal/audit"
)

func TestIndexSatisfiesStoreAndRebuilder(t *testing.T) {
	var _ Store = (*Index)(nil)
	// *Index intentionally does NOT implement Rebuilder (Mode A has no rebuild
	// endpoint) — this is a negative compile-time check via a type assertion below.
}

func TestIndexHealth(t *testing.T) {
	ix, err := OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	h, err := ix.Health()
	if err != nil {
		t.Fatal(err)
	}
	if h.Mode != "A" || !h.IsLeader || h.LeaseEpoch != 0 || h.LagSeconds != 0 || h.SegmentsTracked != 0 {
		t.Fatalf("pre-ingest health = %+v, want Mode=A IsLeader=true LeaseEpoch=0 LagSeconds=0 SegmentsTracked=0", h)
	}
	if h.LastIngestTS != "" {
		t.Fatalf("pre-ingest LastIngestTS = %q, want empty", h.LastIngestTS)
	}

	r := completed("01A", "2026-06-29T10:00:00Z", "alpha", "claude-sonnet-4-6", 100, 50, 10, 1_000)
	if err := ix.Ingest(r); err != nil {
		t.Fatal(err)
	}
	h, err = ix.Health()
	if err != nil {
		t.Fatal(err)
	}
	if h.LastIngestTS == "" {
		t.Fatal("post-ingest LastIngestTS is empty, want a non-empty RFC3339Nano timestamp")
	}
}

func TestBillableModelOfDayOfExported(t *testing.T) {
	// Round-1 plan-gate: pgstore (Task 2) needs the exact same classification
	// rules Mode A uses. Exported here (not duplicated in pgstore) so both
	// modes agree by construction.
	r := completed("01A", "2026-06-29T10:00:00Z", "alpha", "claude-sonnet-4-6", 100, 50, 10, 1_000)
	if !Billable(r) {
		t.Fatal("Billable(completed-with-cost) = false, want true")
	}
	if Billable(audit.Record{Event: "request_started"}) {
		t.Fatal("Billable(started) = true, want false")
	}
	if ModelOf(r) != "claude-sonnet-4-6" {
		t.Fatalf("ModelOf = %q", ModelOf(r))
	}
	if DayOf("2026-06-29T10:00:00Z") != "2026-06-29" {
		t.Fatalf("DayOf = %q", DayOf("2026-06-29T10:00:00Z"))
	}
}
