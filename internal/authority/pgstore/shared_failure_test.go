package pgstore

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
)

func TestSharedUsageAuthorizesEverySubjectSelector(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", TokensPerDay: 20})
	f.team(t, keystore.TeamRecord{Name: "beta", TokensPerDay: 20})
	_, r := f.key(t, "alpha", keystore.KeyOptions{Owner: "person"})
	sharedReserve(t, f.a, r)
	for _, s := range []governance.Subject{
		{Team: "beta", KeyID: r.Subject.KeyID, User: "person"},
		{Team: "alpha", KeyID: r.Subject.KeyID, User: "other"},
		{Team: "alpha", User: "person"},
	} {
		if got, err := f.b.SharedUsage(context.Background(), s); err == nil || len(got) != 0 {
			t.Fatal("usage disclosed another subject")
		}
	}
	if err := f.keys.Revoke(context.Background(), r.Subject.KeyID); err != nil {
		t.Fatal(err)
	}
	if got, err := f.b.SharedUsage(context.Background(), r.Subject); err == nil || len(got) != 0 {
		t.Fatal("revoked subject read usage")
	}
}

func TestSharedMissingTeamAndBookingMetadataFailClosed(t *testing.T) {
	f := newSharedFixture(t)
	_, r := f.key(t, "absent-team", keystore.KeyOptions{})
	if _, err := f.a.ReserveShared(context.Background(), r); sharedStatus(err) != 503 {
		t.Fatalf("missing team: %v", err)
	}
	f.team(t, keystore.TeamRecord{Name: "alpha", TokensPerDay: 20})
	_, r = f.key(t, "alpha", keystore.KeyOptions{})
	p := sharedReserve(t, f.a, r)
	if _, err := f.db.Exec(context.Background(), `DELETE FROM shared_bookings WHERE permit_hash=$1`, capabilityHash(p.ID)); err != nil {
		t.Fatal(err)
	}
	if err := f.b.CancelShared(context.Background(), p); err == nil {
		t.Fatal("missing booking metadata accepted")
	}
}

func TestSharedFailedBookingRollsBackAllScopesAndRedacts(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", TokensPerDay: 100, BudgetUSDMicros: 10000})
	_, r := f.key(t, "alpha", keystore.KeyOptions{RPM: 2})
	putBudget(t, f.db, 10, 1, true, false)
	r.PolicyGeneration = f.generation(t)
	// An abort AFTER money checks and the permit insert must roll everything
	// back. Its database error intentionally includes private-looking text.
	_, err := f.db.Exec(context.Background(), `CREATE FUNCTION shared_test_abort() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'secret-dsn-and-identity-should-never-escape'; END $$;
		CREATE TRIGGER shared_test_abort BEFORE INSERT ON shared_bookings FOR EACH ROW EXECUTE FUNCTION shared_test_abort()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.a.ReserveShared(context.Background(), r); !errors.Is(err, governance.ErrSharedUnavailable) ||
		strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe admission failure: %v", err)
	}
	var permits int
	if err = f.db.QueryRow(context.Background(), `SELECT count(*) FROM shared_permits`).Scan(&permits); err != nil {
		t.Fatal(err)
	}
	if permits != 0 {
		t.Fatal("aborted transaction left permit")
	}
	for _, u := range sharedUsage(t, f.b, r.Subject) {
		if u.Used != 0 || u.Reserved != 0 {
			t.Fatalf("aborted transaction billed %s", u.Kind)
		}
	}
}

func TestSharedBoundedLockWaitAndUnavailable(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", TokensPerDay: 20})
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	ctx := context.Background()
	tx, err := f.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `LOCK TABLE policies IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	begin := time.Now()
	if _, err := f.a.ReserveShared(short, r); sharedStatus(err) != 503 {
		t.Fatalf("lock wait did not fail closed: %v", err)
	}
	if time.Since(begin) > time.Second {
		t.Fatal("caller deadline was ignored")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := New(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := s.ReserveShared(ctx, r); sharedStatus(err) != 503 {
		t.Fatalf("closed pool: %v", err)
	}
}

func TestSharedOverflowRollsBackSoftBudgets(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", BudgetUSDMicros: 1, BudgetOnExceeded: "warn"})
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	r.CostBoundMicroUSD = math.MaxInt64
	sharedReserve(t, f.a, r)
	r.CostBoundMicroUSD = 1
	if _, err := f.b.ReserveShared(context.Background(), r); sharedStatus(err) != 503 {
		t.Fatalf("overflow accepted: %v", err)
	}
	u := sharedUsage(t, f.b, r.Subject)
	if len(u) != 1 || u[0].Used != math.MaxInt64 {
		t.Fatal("overflow modified ledger")
	}
}

func TestSharedPartialAndIndependentlyKnownDimensions(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial", true: "known-tokens-unknown-cost"}[complete], func(t *testing.T) {
			f := newSharedFixture(t)
			f.team(t, keystore.TeamRecord{Name: "alpha", TokensPerDay: 100, BudgetUSDMicros: 10000})
			_, r := f.key(t, "alpha", keystore.KeyOptions{})
			p := sharedReserve(t, f.a, r)
			n := int64(3)
			if err := f.b.FinishShared(context.Background(), p, governance.SharedSettlement{Complete: complete, Tokens: &n}); err != nil {
				t.Fatal(err)
			}
			for _, u := range sharedUsage(t, f.a, r.Subject) {
				if u.Kind == "tokens" && complete {
					if u.Used != 3 || u.Reserved != 0 {
						t.Fatal("known tokens retained bound")
					}
				} else if u.Used != u.Reserved || u.Reserved == 0 {
					t.Fatal("unknown dimension released bound")
				}
			}
		})
	}
}

func TestSharedSoftPolicyCannotRelaxHardTeamOrKey(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", BudgetUSDMicrosPerDay: 1000})
	_, r := f.key(t, "alpha", keystore.KeyOptions{BudgetUSDMicros: 1000})
	putBudget(t, f.db, 1, 1, false, false)
	r.PolicyGeneration = f.generation(t)
	sharedReserve(t, f.a, r)
	if _, err := f.b.ReserveShared(context.Background(), r); sharedStatus(err) != 402 {
		t.Fatalf("soft policy relaxed hard scope: %v", err)
	}
}

func TestSharedChangedModelPolicyRejectsStaleRoutingBeforeBilling(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", TokensPerDay: 20})
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	putDocument(t, f.db, sharedDoc("access", v1alpha1.Subject{Team: "alpha"}, v1alpha1.Rule{
		Name: "allow", FailurePolicy: v1alpha1.FailClosed, ModelAccess: &v1alpha1.ModelAccessRule{Allow: []string{"other-model"}}}))
	if _, err := f.a.ReserveShared(context.Background(), r); sharedStatus(err) != 503 {
		t.Fatalf("changed routing policy ignored: %v", err)
	}
	for _, u := range sharedUsage(t, f.b, r.Subject) {
		if u.Used != 0 {
			t.Fatal("access denial billed")
		}
	}
}

func TestSharedPolicyGenerationRequiredAndFreshUnderLock(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", TokensPerDay: 100})
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	emptyGeneration := r.PolicyGeneration
	r.PolicyGeneration = ""
	if _, err := f.a.ReserveShared(context.Background(), r); sharedStatus(err) != 503 {
		t.Fatalf("missing generation admitted: %v", err)
	}
	r.PolicyGeneration = emptyGeneration
	doc := sharedDoc("quota", v1alpha1.Subject{Team: "alpha"}, sharedQuota("cap", 100, v1alpha1.PeriodCalendarDay))
	putDocument(t, f.db, doc)
	if _, err := f.b.ReserveShared(context.Background(), r); sharedStatus(err) != 503 {
		t.Fatalf("stale generation admitted: %v", err)
	}
	for _, u := range sharedUsage(t, f.a, r.Subject) {
		if u.Used != 0 {
			t.Fatal("generation denial billed")
		}
	}
	r.PolicyGeneration = f.generation(t)
	sharedReserve(t, f.a, r)
}
