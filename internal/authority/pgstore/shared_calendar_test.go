package pgstore

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/policy"
)

// Move a committed fixture's captured windows into the past. Production still
// uses the real database clock; no process-time injection can authorize traffic.
func (f sharedFixture) moveToOldWindows(t *testing.T, p *governance.SharedPermit, r governance.SharedRequest) []sharedBooking {
	t.Helper()
	ctx := context.Background()
	tx, err := f.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	principal, now, err := sharedSnapshot(ctx, tx, r.Subject)
	if err != nil {
		t.Fatal(err)
	}
	defs, _, err := sharedDefinitions(ctx, tx, principal, now.AddDate(-1, 0, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	past := map[string]sharedDefinition{}
	for _, d := range defs {
		past[d.id.key] = d
	}
	bookings, err := readSharedBookings(ctx, tx, capabilityHash(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range bookings {
		d := past[b.id.key]
		if d.end.IsZero() {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO shared_counters
			SELECT counter_key,$3,used,reserved,debt,refilled,rate,updated_at,frozen
			FROM shared_counters WHERE counter_key=$1 AND window_id=$2`, b.id.key, b.id.window, d.id.window); err != nil {
			t.Fatal(err)
		}
		if b.moneyKey != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO authority_accounts
				(budget_key,window_id,hard_encumbered,hard_consumed,soft_consumed,soft_pending,accepts_meters,frozen)
				SELECT budget_key,$3,hard_encumbered,hard_consumed,soft_consumed,soft_pending,accepts_meters,frozen
				FROM authority_accounts WHERE budget_key=$1 AND window_id=$2`, b.moneyKey, b.id.window, d.id.window); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE shared_bookings SET window_id=$4 WHERE permit_hash=$1 AND counter_key=$2 AND window_id=$3`,
			capabilityHash(p.ID), b.id.key, b.id.window, d.id.window); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM shared_counters WHERE counter_key=$1 AND window_id=$2`, b.id.key, b.id.window); err != nil {
			t.Fatal(err)
		}
		if b.moneyKey != "" {
			if _, err := tx.Exec(ctx, `DELETE FROM authority_accounts WHERE budget_key=$1 AND window_id=$2`, b.moneyKey, b.id.window); err != nil {
				t.Fatal(err)
			}
		}
		bookings[i].id.window = d.id.window
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return bookings
}

func TestSharedOriginalCalendarWindowsFinishAndCancel(t *testing.T) {
	for _, cancelOld := range []bool{false, true} {
		t.Run(map[bool]string{false: "finish", true: "cancel"}[cancelOld], func(t *testing.T) {
			f := newSharedFixture(t)
			f.team(t, keystore.TeamRecord{Name: "alpha", TokensPerDay: 100, BudgetUSDMicros: 10000, BudgetUSDMicrosPerDay: 10000})
			putBudget(t, f.db, 10, 1, true, false)
			putDocument(t, f.db, sharedDoc("monthly", v1alpha1.Subject{Team: "alpha"}, sharedQuota("cap", 100, v1alpha1.PeriodCalendarMonth)))
			_, r := f.key(t, "alpha", keystore.KeyOptions{BudgetUSDMicros: 10000, BudgetUSDMicrosPerDay: 10000})
			old := sharedReserve(t, f.a, r)
			original := f.moveToOldWindows(t, old, r)
			sharedReserve(t, f.b, r)
			n, cost := int64(2), int64(100)
			if cancelOld {
				if err := f.b.CancelShared(context.Background(), old); err != nil {
					t.Fatal(err)
				}
				n, cost = 0, 0
			} else if err := f.b.FinishShared(context.Background(), old, governance.SharedSettlement{Complete: true, Tokens: &n, CostMicroUSD: &cost}); err != nil {
				t.Fatal(err)
			}
			for _, u := range sharedUsage(t, f.a, r.Subject) {
				want := int64(10)
				if u.Kind == "microUSD" {
					want = 1000
				}
				if u.Used != want || u.Reserved != want {
					t.Fatalf("old settlement changed current window: %+v", u)
				}
			}
			for _, b := range original {
				want := n
				if b.kind == "microUSD" {
					want = cost
				}
				var used, reserved int64
				if err := f.db.QueryRow(context.Background(), `SELECT used,reserved FROM shared_counters WHERE counter_key=$1 AND window_id=$2`, b.id.key, b.id.window).Scan(&used, &reserved); err != nil {
					t.Fatal(err)
				}
				if used != want || reserved != 0 {
					t.Fatalf("old ledger used=%d reserved=%d want=%d", used, reserved, want)
				}
				if b.moneyKey != "" {
					if err := f.db.QueryRow(context.Background(), `SELECT hard_encumbered FROM authority_accounts WHERE budget_key=$1 AND window_id=$2`, b.moneyKey, b.id.window).Scan(&used); err != nil {
						t.Fatal(err)
					}
					if used != want {
						t.Fatal("old policy money not settled")
					}
				}
			}
		})
	}
}

func TestSharedFinishAfterPolicyDeleteAndKeyRevoke(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha"})
	putBudget(t, f.db, 10, 1, true, false)
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	p := sharedReserve(t, f.a, r)
	if _, err := f.db.Exec(context.Background(), `DELETE FROM policies`); err != nil {
		t.Fatal(err)
	}
	if err := f.keys.Revoke(context.Background(), r.Subject.KeyID); err != nil {
		t.Fatal(err)
	}
	cost := int64(300)
	if err := f.b.FinishShared(context.Background(), p, governance.SharedSettlement{Complete: true, CostMicroUSD: &cost}); err != nil {
		t.Fatal(err)
	}
	var used int64
	if err := f.db.QueryRow(context.Background(), `SELECT hard_encumbered FROM authority_accounts`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 300 {
		t.Fatal("removed policy forgave historical liability")
	}
}

func TestSharedConcurrentFinishCancelConflict(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", TokensPerDay: 100})
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	p := sharedReserve(t, f.a, r)
	n := int64(4)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		results <- f.a.FinishShared(context.Background(), p, governance.SharedSettlement{Complete: true, Tokens: &n})
	}()
	go func() { defer wg.Done(); <-start; results <- f.b.CancelShared(context.Background(), p) }()
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if sharedStatus(err) != 503 {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("terminal winners=%d", success)
	}
	u := sharedUsage(t, f.a, r.Subject)
	if len(u) != 1 || u[0].Reserved != 0 || (u[0].Used != 0 && u[0].Used != 4) {
		t.Fatal("terminal race double settled")
	}
}

func TestSharedPolicyEditWhileWaitingRejectsOldRoutingGeneration(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", TokensPerDay: 100})
	doc := sharedDoc("tokens", v1alpha1.Subject{Team: "alpha"}, sharedQuota("cap", 20, v1alpha1.PeriodCalendarDay))
	putDocument(t, f.db, doc)
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	sharedReserve(t, f.a, r)
	ctx := context.Background()
	tx, err := f.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	doc.Spec.Rules[0].TokenQuota.LimitTokens = 10
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE policies SET doc_yaml=$1 WHERE name='tokens'`, string(body)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.b.ReserveShared(ctx, r); done <- err }()
	// Wait for the admission's actual table-lock wait rather than relying on
	// a sleep to arrange an old read followed by an admin edit.
	deadline := time.Now().Add(2 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		if err := f.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type='Lock' AND query LIKE 'LOCK TABLE policies,keys,teams%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("admission never waited on definition lock")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; sharedStatus(err) != 503 {
		t.Fatalf("old routing generation admitted after cut: %v", err)
	}
	r.PolicyGeneration = policy.GenerationOf([]v1alpha1.GovernancePolicy{doc})
	if _, err := f.a.ReserveShared(ctx, r); sharedStatus(err) != 429 {
		t.Fatalf("current generation reset policy spend: %v", err)
	}
}
