package pgstore

import (
	"context"
	"testing"

	"github.com/inferplane/inferplane/internal/policy"
)

func TestOldBootMeterRecoveryPreservesServerMaxima(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		seq, consumed, observed             int64
		wantSeq, wantConsumed, wantObserved int64
	}{
		{"stale restore", 1, 10_000, 5_000, 10, 100_000, 20_000},
		{"higher observed only", 1, 80_000, 60_000, 10, 100_000, 60_000},
		{"higher consumed only", 1, 150_000, 10_000, 10, 150_000, 20_000},
		{"higher sequence only", 11, 10_000, 5_000, 11, 100_000, 20_000},
		{"equal sequence changed counters", 10, 120_000, 30_000, 10, 120_000, 30_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, db := testDatabase(t)
			putBudget(t, db, 200, 100, false, false)
			s := openStore(t, dsn)
			b := currentBudget(t, s)
			original := heartbeat("original-boot")
			original.Meters = []policy.AuthorityMeter{{
				Instance: "original-boot", Key: b.Key, WindowID: b.WindowID,
				Sequence: 10, Consumed: 100_000, Observed: 20_000,
			}}
			syncOK(t, s, "node", original)
			peer := heartbeat("peer-boot")
			peer.Meters = []policy.AuthorityMeter{{
				Instance: "peer-boot", Key: b.Key, WindowID: b.WindowID,
				Sequence: 1, Consumed: 25_000, Observed: 25_000,
			}}
			syncOK(t, s, "peer", peer)

			recovery := heartbeat("recovery-boot")
			recovery.Meters = []policy.AuthorityMeter{{
				Instance: "original-boot", Key: b.Key, WindowID: b.WindowID,
				Sequence: tc.seq, Consumed: tc.consumed, Observed: tc.observed,
			}}
			for i := 0; i < 2; i++ {
				resp := syncOK(t, s, "node", recovery)
				if len(resp.Authority.MeterAcks) != 1 ||
					resp.Authority.MeterAcks[0].Sequence != tc.seq ||
					resp.Authority.MeterAcks[0].Instance != "original-boot" ||
					resp.Authority.MeterAcks[0].ID != b.WindowID {
					t.Fatal("recovery did not acknowledge the incoming outbox checkpoint")
				}
			}
			s.Close()
			reopened := openStore(t, dsn)
			syncOK(t, reopened, "node", recovery)
			var seq, consumed, observed, total int64
			if err := db.QueryRow(context.Background(), `SELECT m.sequence,m.consumed,m.observed,a.soft_consumed
				FROM authority_meters m JOIN authority_accounts a USING(budget_key,window_id)
				WHERE m.owner=$1 AND m.instance=$2 AND m.budget_key=$3 AND m.window_id=$4`,
				"node", "original-boot", b.Key, b.WindowID).Scan(&seq, &consumed, &observed, &total); err != nil {
				t.Fatal(err)
			}
			if seq != tc.wantSeq || consumed != tc.wantConsumed || observed != tc.wantObserved ||
				total != tc.wantConsumed+25_000 {
				t.Fatalf("recovery accounting: seq=%d consumed=%d observed=%d total=%d", seq, consumed, observed, total)
			}
			// A later hard cap must retain both nodes' soft liabilities.
			putBudget(t, db, 200, 100, true, false)
			hard := currentBudget(t, reopened)
			issue(t, reopened, "fresh", "boot", grantRequest(hard, 200_000-tc.wantConsumed-25_000))
			extra := heartbeat("boot")
			extra.Requests = []policy.AuthorityGrantRequest{grantRequest(hard, 1)}
			if resp := syncOK(t, reopened, "other", extra); len(resp.Authority.Grants) != 0 {
				t.Fatal("stale soft recovery invented refundable authority")
			}
		})
	}
}

func TestCurrentBootMeterStillRejectsConflictingOrDecreasingReports(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 200, 100, false, false)
	s := openStore(t, dsn)
	b := currentBudget(t, s)
	original := heartbeat("boot")
	original.Meters = []policy.AuthorityMeter{{
		Instance: "boot", Key: b.Key, WindowID: b.WindowID,
		Sequence: 10, Consumed: 100_000, Observed: 20_000,
	}}
	syncOK(t, s, "node", original)
	for _, tc := range []struct{ seq, consumed, observed int64 }{
		{9, 100_000, 20_000},
		{10, 110_000, 20_000},
		{10, 100_000, 25_000},
		{11, 90_000, 20_000},
		{11, 100_000, 10_000},
	} {
		req := heartbeat("boot")
		req.Meters = []policy.AuthorityMeter{{
			Instance: "boot", Key: b.Key, WindowID: b.WindowID,
			Sequence: tc.seq, Consumed: tc.consumed, Observed: tc.observed,
		}}
		if _, err := s.Sync(context.Background(), "node", req); err == nil {
			t.Fatalf("current-boot conflicting/decreasing meter accepted: %+v", tc)
		}
	}
	// The unchanged current checkpoint still replays without a second debit.
	syncOK(t, s, "node", original)
	var total int64
	if err := db.QueryRow(context.Background(), `SELECT soft_consumed FROM authority_accounts
		WHERE budget_key=$1 AND window_id=$2`, b.Key, b.WindowID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 100_000 {
		t.Fatalf("rejected or repeated current-boot report changed liability: %d", total)
	}
}

func TestOldBootMeterRecoveryRollsBackWithConflictingCurrentMeter(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 200, 100, false, false)
	s := openStore(t, dsn)
	b := currentBudget(t, s)
	for _, boot := range []string{"original", "current"} {
		req := heartbeat(boot)
		req.Meters = []policy.AuthorityMeter{{
			Instance: boot, Key: b.Key, WindowID: b.WindowID,
			Sequence: 10, Consumed: 50_000, Observed: 20_000,
		}}
		syncOK(t, s, "node", req)
	}
	req := heartbeat("current")
	req.Meters = []policy.AuthorityMeter{
		{Instance: "original", Key: b.Key, WindowID: b.WindowID, Sequence: 1, Consumed: 80_000, Observed: 10_000},
		{Instance: "current", Key: b.Key, WindowID: b.WindowID, Sequence: 1, Consumed: 10_000, Observed: 10_000},
	}
	if _, err := s.Sync(context.Background(), "node", req); err == nil {
		t.Fatal("conflicting current-boot meter accepted after an old-boot recovery")
	}
	var consumed, observed, seq, total int64
	if err := db.QueryRow(context.Background(), `SELECT m.consumed,m.observed,m.sequence,a.soft_consumed
		FROM authority_meters m JOIN authority_accounts a USING(budget_key,window_id)
		WHERE m.owner=$1 AND m.instance=$2 AND m.budget_key=$3 AND m.window_id=$4`,
		"node", "original", b.Key, b.WindowID).Scan(&consumed, &observed, &seq, &total); err != nil {
		t.Fatal(err)
	}
	if consumed != 50_000 || observed != 20_000 || seq != 10 || total != 100_000 {
		t.Fatal("failed batch committed its earlier old-boot meter recovery")
	}
}
