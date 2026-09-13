package pgstore

import (
	"context"
	"sync"
	"testing"

	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/policy"
)

// A conservative in-flight bound is capacity liability, but it is not spend
// that can latch a soft economy tier. Terminal uncertainty is retained spend.
func TestSharedSoftTierExcludesPendingUntilTerminal(t *testing.T) {
	for _, outcome := range []string{"cancel", "below-threshold", "observed", "unknown", "partial", "invalid", "over-bound"} {
		t.Run(outcome, func(t *testing.T) {
			f := newSharedFixture(t)
			f.team(t, keystore.TeamRecord{Name: "alpha"})
			putBudget(t, f.db, 10, 1, false, true)
			_, r := f.key(t, "alpha", keystore.KeyOptions{})
			r.CostBoundMicroUSD = 10000
			p := sharedReserve(t, f.a, r)
			before := syncOK(t, f.b, "node", heartbeat("boot"))
			if len(before.ActiveTiers) != 0 {
				t.Fatal("in-flight bound latched a soft tier before settlement")
			}
			for _, u := range sharedUsage(t, f.b, r.Subject) {
				if u.Policy == "budget" && (u.Used != 10000 || u.Reserved != 10000) {
					t.Fatal("excluding pending from tiers released admission liability")
				}
			}
			actual, wantConsumed, threshold := int64(1000), int64(10000), 90
			usage := governance.SharedSettlement{Complete: true, CostMicroUSD: &actual}
			wantError := false
			switch outcome {
			case "cancel":
				wantConsumed, threshold = 0, 0
			case "below-threshold":
				wantConsumed, threshold = actual, 0
			case "observed":
				actual, wantConsumed, threshold = 6000, 6000, 50
			case "unknown":
				usage = governance.SharedSettlement{}
			case "partial":
				usage.Complete = false
			case "invalid":
				usage.InvalidUsage, wantError = true, true
			case "over-bound":
				actual, wantError = 10001, true
			}
			// Retry the same terminal outcome from an independent pool.
			for _, s := range []*Store{f.a, f.b} {
				var err error
				if outcome == "cancel" {
					err = s.CancelShared(context.Background(), p)
				} else {
					err = s.FinishShared(context.Background(), p, usage)
				}
				if (err != nil) != wantError {
					t.Fatalf("terminal error=%v, want error=%t", err, wantError)
				}
			}
			var consumed, pending int64
			var frozen bool
			if err := f.db.QueryRow(context.Background(), `SELECT soft_consumed,soft_pending,frozen FROM authority_accounts`).
				Scan(&consumed, &pending, &frozen); err != nil {
				t.Fatal(err)
			}
			if consumed != wantConsumed || pending != 0 || frozen != wantError {
				t.Fatalf("terminal account: consumed=%d pending=%d frozen=%t", consumed, pending, frozen)
			}
			after := syncOK(t, f.b, "node", heartbeat("boot"))
			if threshold == 0 {
				if len(after.ActiveTiers) != 0 {
					t.Fatal("below-threshold terminal latched a soft tier")
				}
			} else if len(after.ActiveTiers) != 1 || after.ActiveTiers[0].ThresholdPercent != threshold {
				t.Fatalf("terminal spend did not activate threshold %d", threshold)
			}
		})
	}
}

func TestSharedSoftPendingReplayPreservesOtherPendingBookings(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "known", true: "invalid"}[invalid], func(t *testing.T) {
			f := newSharedFixture(t)
			f.team(t, keystore.TeamRecord{Name: "alpha"})
			putBudget(t, f.db, 10, 1, false, true)
			_, r := f.key(t, "alpha", keystore.KeyOptions{})
			r.CostBoundMicroUSD = 10000
			a := sharedReserve(t, f.a, r)
			b := sharedReserve(t, f.b, r)
			actual := int64(1000)
			usage := governance.SharedSettlement{Complete: true, CostMicroUSD: &actual, InvalidUsage: invalid}
			for i := 0; i < 2; i++ {
				err := f.a.FinishShared(context.Background(), a, usage)
				if (err != nil) != invalid {
					t.Fatalf("finish error=%v", err)
				}
			}
			var pending int64
			if err := f.db.QueryRow(context.Background(), `SELECT soft_pending FROM authority_accounts`).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if pending != 10000 {
				t.Fatalf("replay removed another permit's pending amount: %d", pending)
			}
			if err := f.b.CancelShared(context.Background(), b); err != nil {
				t.Fatal(err)
			}
			if err := f.db.QueryRow(context.Background(), `SELECT soft_pending FROM authority_accounts`).Scan(&pending); err != nil || pending != 0 {
				t.Fatalf("last cancellation left pending amount=%d err=%v", pending, err)
			}
		})
	}
}

func TestSharedSoftPendingDoesNotHideTerminalLocalMeters(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha"})
	putBudget(t, f.db, 10, 1, false, true)
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	r.CostBoundMicroUSD = 10000
	sharedReserve(t, f.a, r)
	budget := currentBudget(t, f.b)
	req := heartbeat("boot")
	req.Meters = []policy.AuthorityMeter{{Instance: "boot", Key: budget.Key, WindowID: budget.WindowID, Sequence: 1, Consumed: 6000, Observed: 6000}}
	resp := syncOK(t, f.b, "node", req)
	if len(resp.ActiveTiers) != 1 || resp.ActiveTiers[0].ThresholdPercent != 50 {
		t.Fatal("soft pending classification hid settled local spend or counted an in-flight bound")
	}
}

func TestSharedSoftPendingMigrationPreservesAccountsAndIdentity(t *testing.T) {
	dsn, db := testDatabase(t)
	ctx := context.Background()
	// The released ADR-045 schema has no shared pending classification.
	if _, err := db.Exec(ctx, `CREATE TABLE authority_accounts (
		budget_key TEXT NOT NULL, window_id TEXT NOT NULL,
		hard_encumbered BIGINT NOT NULL DEFAULT 0 CHECK(hard_encumbered>=0),
		hard_consumed BIGINT NOT NULL DEFAULT 0 CHECK(hard_consumed>=0),
		soft_consumed BIGINT NOT NULL DEFAULT 0 CHECK(soft_consumed>=0),
		accepts_meters BOOLEAN NOT NULL DEFAULT false,
		frozen BOOLEAN NOT NULL DEFAULT false,
		PRIMARY KEY(budget_key,window_id));
		INSERT INTO authority_accounts VALUES('legacy','original',100,50,300,true,false)`); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, dsn)
	id, generation, err := s.SharedBinding(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hard, consumed, soft, pending int64
	if err := db.QueryRow(ctx, `SELECT hard_encumbered,hard_consumed,soft_consumed,soft_pending
		FROM authority_accounts WHERE budget_key='legacy'`).
		Scan(&hard, &consumed, &soft, &pending); err != nil {
		t.Fatal(err)
	}
	if hard != 100 || consumed != 50 || soft != 300 || pending != 0 {
		t.Fatal("migration changed existing monetary liability")
	}
	if _, err := db.Exec(ctx, `UPDATE authority_accounts SET soft_pending=200 WHERE budget_key='legacy'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT soft_pending FROM authority_accounts WHERE budget_key='legacy'`).Scan(&pending); err != nil || pending != 200 {
		t.Fatalf("reinitialization reset pending liability: %d err=%v", pending, err)
	}
	again, nextGeneration, err := s.SharedBinding(ctx)
	if err != nil || again != id || generation != nextGeneration {
		t.Fatal("migration changed parent authority identity or generation")
	}
}

func TestSharedSoftPendingOriginalWindowAfterRestart(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha"})
	putBudget(t, f.db, 10, 1, false, true)
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	r.CostBoundMicroUSD = 10000
	old := sharedReserve(t, f.a, r)
	bookings := f.moveToOldWindows(t, old, r)
	sharedReserve(t, f.b, r)
	reopened := openStore(t, f.dsn)
	if err := reopened.InitializeShared(context.Background()); err != nil {
		t.Fatal(err)
	}
	cost := int64(1000)
	if err := reopened.FinishShared(context.Background(), old, governance.SharedSettlement{Complete: true, CostMicroUSD: &cost}); err != nil {
		t.Fatal(err)
	}
	if len(bookings) != 1 || bookings[0].moneyKey == "" {
		t.Fatal("missing historical soft money fixture")
	}
	var oldConsumed, oldPending, currentPending int64
	if err := f.db.QueryRow(context.Background(), `SELECT soft_consumed,soft_pending FROM authority_accounts
		WHERE budget_key=$1 AND window_id=$2`, bookings[0].moneyKey, bookings[0].id.window).
		Scan(&oldConsumed, &oldPending); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(context.Background(), `SELECT soft_pending FROM authority_accounts
		WHERE budget_key=$1 AND window_id<>$2`, bookings[0].moneyKey, bookings[0].id.window).
		Scan(&currentPending); err != nil {
		t.Fatal(err)
	}
	if oldConsumed != 1000 || oldPending != 0 || currentPending != 10000 {
		t.Fatalf("settlement changed wrong pending window: old=%d/%d current=%d", oldConsumed, oldPending, currentPending)
	}
	if resp := syncOK(t, reopened, "node", heartbeat("boot")); len(resp.ActiveTiers) != 0 {
		t.Fatal("current pending bound latched tier after historical settlement")
	}
}

func TestSharedSoftPendingConcurrentTerminalsAndTierRead(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha"})
	putBudget(t, f.db, 10, 1, false, true)
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	r.CostBoundMicroUSD = 9000
	a := sharedReserve(t, f.a, r)
	b := sharedReserve(t, f.b, r)
	cost := int64(1000)
	start := make(chan struct{})
	results := make(chan error, 3)
	var tiers policy.SyncResponse
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		<-start
		results <- f.a.FinishShared(context.Background(), a, governance.SharedSettlement{Complete: true, CostMicroUSD: &cost})
	}()
	go func() {
		defer wg.Done()
		<-start
		results <- f.b.CancelShared(context.Background(), b)
	}()
	go func() {
		defer wg.Done()
		<-start
		var err error
		tiers, err = f.b.Sync(context.Background(), "node", heartbeat("boot"))
		results <- err
	}()
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(tiers.ActiveTiers) != 0 {
		t.Fatal("tier evaluation observed an intermediate pending/spend mismatch")
	}
	var consumed, pending int64
	if err := f.db.QueryRow(context.Background(), `SELECT soft_consumed,soft_pending FROM authority_accounts`).
		Scan(&consumed, &pending); err != nil {
		t.Fatal(err)
	}
	if consumed != 1000 || pending != 0 {
		t.Fatalf("concurrent terminals lost accounting: consumed=%d pending=%d", consumed, pending)
	}
}
