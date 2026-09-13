package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/policy"
)

func TestLongLeaseClampsToOwnedWindow(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 100, true, false)
	s := openStore(t, dsn)
	doc := syncOK(t, s, "seed", heartbeat("boot")).Policies[0]
	doc.Spec.Rules[0].Budget.Lease.RenewInterval = "87600h" // ten years
	putDocument(t, db, doc)
	b := currentBudget(t, s)
	req := heartbeat("boot")
	req.Requests = []policy.AuthorityGrantRequest{grantRequest(b, 1)}
	resp := syncOK(t, s, "node", req)
	if len(resp.Authority.Grants) != 1 ||
		!resp.Authority.Grants[0].ExpiresAt.Equal(resp.Authority.Budgets[0].WindowEnd) {
		t.Fatal("long lease did not clamp to the database-owned window end")
	}
}

func TestGrantClockRefreshesAfterAccountLock(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 100, true, false)
	s := openStore(t, dsn)
	b := currentBudget(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var writerPID int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM authority_accounts
		WHERE budget_key=$1 AND window_id=$2 FOR UPDATE`, b.Key, b.WindowID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		response policy.SyncResponse
		err      error
	}
	done := make(chan result, 1)
	go func() {
		req := heartbeat("boot")
		req.Requests = []policy.AuthorityGrantRequest{grantRequest(b, 1)}
		resp, err := s.Sync(ctx, "waiting-node", req)
		done <- result{resp, err}
	}()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		var blocked bool
		if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE $1 = ANY(pg_blocking_pids(pid)))`, writerPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-done:
			t.Fatal("issuance did not wait for the account lock")
		case <-ctx.Done():
			t.Fatal("issuance did not reach the account lock")
		case <-poll.C:
		}
	}
	// This sample is strictly after Sync's initial clock read: the backend
	// is already observed waiting on our lock. No arbitrary sleep is needed.
	var releaseTime time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&releaseTime); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.response.Authority.ServerTime.Before(releaseTime) {
			t.Fatal("issuance retained a clock sample from before its account lock")
		}
		if len(got.response.Authority.Grants) != 1 {
			t.Fatal("issuance failed after the account lock was released")
		}
		grant := got.response.Authority.Grants[0]
		lifetime := grant.ExpiresAt.Sub(got.response.Authority.ServerTime)
		if lifetime <= 0 || lifetime > 90*time.Second ||
			(lifetime < 90*time.Second && !grant.ExpiresAt.Equal(got.response.Authority.Budgets[0].WindowEnd)) {
			t.Fatal("grant expiry was not derived from the refreshed database clock")
		}
	case <-ctx.Done():
		t.Fatal("issuance did not finish after the account lock was released")
	}
}
