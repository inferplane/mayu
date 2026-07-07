package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
)

func TestTryAcquireLeaseExactlyOneWinner(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	epochA, okA, errA := tryAcquireLease(ctx, s.db, "A", 15*time.Second)
	epochB, okB, errB := tryAcquireLease(ctx, s.db, "B", 15*time.Second)
	if errA != nil || errB != nil {
		t.Fatalf("errA=%v errB=%v", errA, errB)
	}
	if okA == okB {
		t.Fatalf("exactly one of A/B must win, got okA=%v okB=%v", okA, okB)
	}
	if okA && epochA == 0 {
		t.Fatal("winner's epoch must be nonzero after a handover")
	}
	if okB && epochB == 0 {
		t.Fatal("winner's epoch must be nonzero after a handover")
	}
}

func TestFencedIngestRejectsStaleEpoch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	epoch1, ok, err := tryAcquireLease(ctx, s.db, "A", 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("A acquire: ok=%v err=%v", ok, err)
	}
	// Wait past a short TTL and let B take over.
	_, _, _ = tryAcquireLease(ctx, s.db, "A", -1*time.Second) // expire A's lease immediately
	epoch2, ok, err := tryAcquireLease(ctx, s.db, "B", 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("B acquire after A's expiry: ok=%v err=%v", ok, err)
	}
	if epoch2 <= epoch1 {
		t.Fatalf("handover must produce a strictly higher epoch: %d -> %d", epoch1, epoch2)
	}
	r := completedRecord("01FENCE", "2026-07-07T10:00:00Z", "alpha", "m1", 1, 1, 1)
	// A still thinks it holds epoch1 — its ingest must be fenced now that B owns epoch2.
	err = s.ingestBatch(ctx, "A", epoch1, []audit.Record{r}, map[string]int64{"seg-a": 1})
	if err == nil {
		t.Fatal("stale-epoch ingestBatch must fail (errFenced), got nil")
	}
}

func TestRenewalBySameHolderDoesNotBumpEpoch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	epoch1, ok, err := tryAcquireLease(ctx, s.db, "A", 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	epoch2, ok, err := tryAcquireLease(ctx, s.db, "A", 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("renew 1: ok=%v err=%v", ok, err)
	}
	epoch3, ok, err := tryAcquireLease(ctx, s.db, "A", 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("renew 2: ok=%v err=%v", ok, err)
	}
	if epoch1 != epoch2 || epoch2 != epoch3 {
		t.Fatalf("same-holder renewal must NOT bump epoch: %d, %d, %d", epoch1, epoch2, epoch3)
	}
}
