package pgstore

import (
	"context"
	"math"
	"math/big"
	"testing"
	"time"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
)

// Floating point refill loses low bits here; truncating each fractional refill
// loses all progress in the per-microsecond variant.
func TestSharedRateExactRefillAndPolicyCuts(t *testing.T) {
	start := time.Date(2026, 9, 12, 1, 0, 0, 0, time.UTC)
	a := sharedCounter{rate: math.MaxInt64, updated: start}
	a.debt.Set(sharedScaled(math.MaxInt64))
	b := sharedCounter{rate: math.MaxInt64, updated: start}
	b.debt.Set(&a.debt)
	for i := 1; i <= 1000; i++ {
		if err := a.refill(start.Add(time.Duration(i)*time.Microsecond), math.MaxInt64); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.refill(start.Add(time.Millisecond), math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if a.debt.Cmp(&b.debt) != 0 || a.refilled.Cmp(&b.refilled) != 0 {
		t.Fatal("fractional refill depends on polling cadence")
	}
	c := sharedCounter{rate: 100, updated: start}
	c.debt.Set(sharedScaled(100))
	if err := c.refill(start.Add(59*time.Second), 10); err != nil {
		t.Fatal(err)
	}
	want := new(big.Int).Sub(sharedScaled(100), big.NewInt(59_000_000*10))
	if c.debt.Cmp(want) != 0 {
		t.Fatalf("rate cut refilled debt at old rate: %s != %s", c.debt.String(), want.String())
	}
	if err := c.refill(start, 10); err == nil {
		t.Fatal("backward database clock accepted")
	}
}

func TestSharedPolicyRateUserScopesAndStableIdentity(t *testing.T) {
	for _, combined := range []bool{false, true} {
		t.Run(map[bool]string{false: "user", true: "team-user"}[combined], func(t *testing.T) {
			f := newSharedFixture(t)
			f.team(t, keystore.TeamRecord{Name: "alpha"})
			f.team(t, keystore.TeamRecord{Name: "beta"})
			_, a := f.key(t, "alpha", keystore.KeyOptions{Owner: "person"})
			_, b := f.key(t, "beta", keystore.KeyOptions{Owner: "person"})
			subject := v1alpha1.Subject{User: "person"}
			if combined {
				subject.Team = "alpha"
			}
			doc := sharedDoc("person-rate", subject, v1alpha1.Rule{Name: "rate", FailurePolicy: v1alpha1.FailClosed, Rate: &v1alpha1.RateRule{RPM: 1, TPM: 10}})
			putDocument(t, f.db, doc)
			a.PolicyGeneration = f.generation(t)
			b.PolicyGeneration = a.PolicyGeneration
			sharedReserve(t, f.a, a)
			if combined {
				sharedReserve(t, f.b, b)
			} else if _, err := f.b.ReserveShared(context.Background(), b); sharedStatus(err) != 429 {
				t.Fatalf("user rate partitioned by team: %v", err)
			}
			doc.Metadata.Generation++
			doc.Spec.Rules[0].Rate.RPM = 2
			putDocument(t, f.db, doc)
			a.PolicyGeneration = f.generation(t)
			if _, err := f.b.ReserveShared(context.Background(), a); sharedStatus(err) != 429 {
				t.Fatalf("rate edit reset TPM: %v", err)
			}
		})
	}
}

func TestSharedOldRateRefundCannotCreditNewRequest(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", TPM: 10})
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	old := sharedReserve(t, f.a, r)
	if _, err := f.db.Exec(context.Background(), `UPDATE shared_counters SET updated_at=clock_timestamp()-interval '2 minutes' WHERE rate>0`); err != nil {
		t.Fatal(err)
	}
	sharedReserve(t, f.b, r)
	if err := f.a.CancelShared(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.ReserveShared(context.Background(), r); sharedStatus(err) != 429 {
		t.Fatalf("old refund credited new reservation: %v", err)
	}
}

func TestSharedRateKnownUsageReleasesBound(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", TPM: 10, RPM: 2})
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	p := sharedReserve(t, f.a, r)
	actual := int64(4)
	if err := f.b.FinishShared(context.Background(), p, governance.SharedSettlement{Complete: true, Tokens: &actual}); err != nil {
		t.Fatal(err)
	}
	r.TokenBound = 6
	sharedReserve(t, f.b, r)
	r.TokenBound = 1
	if _, err := f.a.ReserveShared(context.Background(), r); sharedStatus(err) != 429 {
		t.Fatalf("third request bypassed RPM/TPM: %v", err)
	}
}

func TestSharedOverlappingCancellationsReturnAllUnspentRateDebt(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", TPM: 20})
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	a := sharedReserve(t, f.a, r)
	b := sharedReserve(t, f.b, r)
	if _, err := f.db.Exec(context.Background(), `UPDATE shared_counters SET updated_at=clock_timestamp()-interval '15 seconds' WHERE rate>0`); err != nil {
		t.Fatal(err)
	}
	// A zero-token reservation materializes the database-time refill without
	// introducing more debt, then both still-undispatched attempts cancel.
	r.TokenBound = 0
	probe := sharedReserve(t, f.a, r)
	for _, p := range []*governance.SharedPermit{probe, a, b} {
		if err := f.b.CancelShared(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range sharedUsage(t, f.a, r.Subject) {
		if u.Used != 0 || u.Reserved != 0 {
			t.Fatalf("cancelled attempts left rate debt: used=%d reserved=%d", u.Used, u.Reserved)
		}
	}
}
