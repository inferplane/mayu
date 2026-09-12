package pgstore

import (
	"context"
	"sync"
	"testing"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/policy"
)

func TestSharedConcurrentMoneyReservationAndLocalGrant(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha"})
	putBudget(t, f.db, 1, 1, true, false)
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	budget := currentBudget(t, f.b)
	start := make(chan struct{})
	var permit *governance.SharedPermit
	var sharedErr, localErr error
	var result policy.SyncResponse
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; permit, sharedErr = f.a.ReserveShared(context.Background(), r) }()
	go func() {
		defer wg.Done()
		<-start
		req := heartbeat("mixed")
		req.Requests = []policy.AuthorityGrantRequest{grantRequest(budget, 1000)}
		result, localErr = f.b.Sync(context.Background(), "node", req)
	}()
	close(start)
	wg.Wait()
	if localErr != nil {
		t.Fatal(localErr)
	}
	winners := len(result.Authority.Grants)
	if sharedErr == nil && permit != nil {
		winners++
	} else if sharedStatus(sharedErr) != 402 {
		t.Fatal(sharedErr)
	}
	if winners != 1 {
		t.Fatalf("mixed profiles admitted %d winners", winners)
	}
	var used int64
	if err := f.db.QueryRow(context.Background(), `SELECT hard_encumbered+soft_consumed FROM authority_accounts`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 1000 {
		t.Fatalf("mixed accounting used=%d", used)
	}
}

func TestSharedSoftPendingHardCutUsesOriginalBooking(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha"})
	putBudget(t, f.db, 1, 1, false, true)
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	r.CostBoundMicroUSD = 2000
	p := sharedReserve(t, f.a, r)
	response := syncOK(t, f.b, "node", heartbeat("boot"))
	if len(response.ActiveTiers) != 0 {
		t.Fatal("pending shared bound activated a soft tier")
	}
	putBudget(t, f.db, 1, 1, true, true)
	r.PolicyGeneration = f.generation(t)
	r.CostBoundMicroUSD = 1000
	if _, err := f.b.ReserveShared(context.Background(), r); sharedStatus(err) != 402 {
		t.Fatalf("hard cut forgot shared soft spend: %v", err)
	}
	response = syncOK(t, f.b, "node", heartbeat("boot"))
	if len(response.ActiveTiers) != 1 || response.ActiveTiers[0].ThresholdPercent != 90 {
		t.Fatal("hard tier ignored pending liability from the original soft booking")
	}
	if err := f.b.CancelShared(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	sharedReserve(t, f.a, r)
	var hard, soft int64
	if err := f.db.QueryRow(context.Background(), `SELECT hard_encumbered,soft_consumed FROM authority_accounts`).Scan(&hard, &soft); err != nil {
		t.Fatal(err)
	}
	if hard != 1000 || soft != 0 {
		t.Fatalf("original soft booking refunded wrong ledger: hard=%d soft=%d", hard, soft)
	}
}

func TestSharedUnknownMoneyRetainsPolicyReservationAndInvalidFreezesLocal(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", TokensPerDay: 100})
	putBudget(t, f.db, 10, 1, true, false)
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	unknown := sharedReserve(t, f.a, r)
	if err := f.b.FinishShared(context.Background(), unknown, governance.SharedSettlement{}); err != nil {
		t.Fatal(err)
	}
	for _, u := range sharedUsage(t, f.a, r.Subject) {
		if u.Policy == "budget" && (u.Used != 1000 || u.Reserved != 1000) {
			t.Fatalf("unknown money lost liability: used=%d reserved=%d", u.Used, u.Reserved)
		}
	}
	p := sharedReserve(t, f.a, r)
	invalid := int64(1001)
	if err := f.b.FinishShared(context.Background(), p, governance.SharedSettlement{Complete: true, CostMicroUSD: &invalid}); err == nil {
		t.Fatal("over-bound money accepted")
	}
	budget := currentBudget(t, f.b)
	req := heartbeat("boot")
	req.Requests = []policy.AuthorityGrantRequest{grantRequest(budget, 1000)}
	resp := syncOK(t, f.b, "node", req)
	if len(resp.Authority.Grants) != 0 || len(resp.Authority.Denied) != 1 || resp.Authority.Denied[0].Reason != "frozen_window" {
		t.Fatal("shared overrun did not freeze ADR-045 account")
	}
}

func TestSharedPolicyMoneyUserOnlySpansTeams(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha"})
	f.team(t, keystore.TeamRecord{Name: "beta"})
	putDocument(t, f.db, sharedDoc("user-money", v1alpha1.Subject{User: "person"}, v1alpha1.Rule{
		Name: "cap", FailurePolicy: v1alpha1.FailClosed,
		Budget: &v1alpha1.BudgetRule{LimitMilliUSD: 1, HardCap: true, Period: v1alpha1.PeriodCalendarMonth}}))
	_, a := f.key(t, "alpha", keystore.KeyOptions{Owner: "person"})
	_, b := f.key(t, "beta", keystore.KeyOptions{Owner: "person"})
	sharedReserve(t, f.a, a)
	if _, err := f.b.ReserveShared(context.Background(), b); sharedStatus(err) != 402 {
		t.Fatalf("user money split by team: %v", err)
	}
}
