package pgstore

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/jackc/pgx/v5/pgxpool"
)

type sharedFixture struct {
	dsn  string
	db   *pgxpool.Pool
	keys *keystore.PostgresStore
	a, b *Store
}

func newSharedFixture(t *testing.T) sharedFixture {
	t.Helper()
	dsn, db := testDatabase(t)
	keys, err := keystore.OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keys.Close() })
	a, b := openStore(t, dsn), openStore(t, dsn)
	for _, s := range []*Store{a, b} {
		if err := s.InitializeShared(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return sharedFixture{dsn, db, keys, a, b}
}

func (f sharedFixture) key(t *testing.T, team string, opts keystore.KeyOptions) (string, governance.SharedRequest) {
	t.Helper()
	plain, _, err := f.keys.CreateWithOptions(context.Background(), team, []string{"*"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return plain, f.request(t, plain)
}

func (f sharedFixture) request(t *testing.T, plain string) governance.SharedRequest {
	t.Helper()
	p, err := f.keys.Resolve(context.Background(), plain)
	if err != nil {
		t.Fatal(err)
	}
	return governance.SharedRequest{Subject: governance.Subject{Team: p.Team, KeyID: p.KeyID, User: p.Owner},
		AuthRevision: p.SharedRevision, PolicyGeneration: f.generation(t), RequestedModel: "model", Model: "model", TokenBound: 10, CostBoundMicroUSD: 1000}
}

func (f sharedFixture) generation(t *testing.T) string {
	t.Helper()
	tx, err := f.db.Begin(context.Background())
	if err != nil {
		t.Fatal("cannot begin test policy read")
	}
	defer rollback(tx)
	docs, err := readPolicies(context.Background(), tx)
	if err != nil {
		t.Fatal("cannot read test policy generation")
	}
	return policy.GenerationOf(docs)
}

func (f sharedFixture) team(t *testing.T, team keystore.TeamRecord) {
	t.Helper()
	if err := f.keys.UpsertTeam(context.Background(), team); err != nil {
		t.Fatal(err)
	}
}

func sharedDoc(name string, subject v1alpha1.Subject, rules ...v1alpha1.Rule) v1alpha1.GovernancePolicy {
	return v1alpha1.GovernancePolicy{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindGovernancePolicy},
		Metadata: v1alpha1.ObjectMeta{Name: name}, Spec: v1alpha1.GovernancePolicySpec{Subject: subject, Rules: rules}}
}

func sharedQuota(name string, amount int64, period v1alpha1.BudgetPeriod) v1alpha1.Rule {
	return v1alpha1.Rule{Name: name, FailurePolicy: v1alpha1.FailClosed,
		TokenQuota: &v1alpha1.TokenQuotaRule{LimitTokens: amount, Period: period}}
}

func sharedStatus(err error) int {
	var denial *governance.SharedDenial
	if errors.As(err, &denial) {
		return denial.Status
	}
	if errors.Is(err, governance.ErrSharedUnavailable) {
		return 503
	}
	return 0
}

func sharedReserve(t *testing.T, s *Store, r governance.SharedRequest) *governance.SharedPermit {
	t.Helper()
	p, err := s.ReserveShared(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func sharedUsage(t *testing.T, s *Store, subject governance.Subject) []governance.SharedLimit {
	t.Helper()
	u, err := s.SharedUsage(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// Replacing durable counters with per-Store state would admit both racers.
func TestSharedIndependentPoolsAtomicAdmission(t *testing.T) {
	for _, dimension := range []string{"team-rpm", "team-tpm", "key-rpm", "key-tpm", "team-quota", "day-quota", "month-quota", "team-money", "key-money", "team-money-month", "key-money-month"} {
		t.Run(dimension, func(t *testing.T) {
			f := newSharedFixture(t)
			team := keystore.TeamRecord{Name: "alpha"}
			opts := keystore.KeyOptions{Owner: "user"}
			switch dimension {
			case "team-rpm":
				team.RPM = 1
			case "team-tpm":
				team.TPM = 10
			case "key-rpm":
				opts.RPM = 1
			case "key-tpm":
				opts.TPM = 10
			case "team-quota":
				team.TokensPerDay = 10
			case "team-money":
				team.BudgetUSDMicrosPerDay = 1000
			case "key-money":
				opts.BudgetUSDMicrosPerDay = 1000
			case "team-money-month":
				team.BudgetUSDMicros = 1000
			case "key-money-month":
				opts.BudgetUSDMicros = 1000
			case "day-quota", "month-quota":
				period := v1alpha1.PeriodCalendarDay
				if dimension == "month-quota" {
					period = v1alpha1.PeriodCalendarMonth
				}
				putDocument(t, f.db, sharedDoc("tokens", v1alpha1.Subject{Team: "alpha"}, sharedQuota("cap", 10, period)))
			}
			f.team(t, team)
			_, req := f.key(t, "alpha", opts)
			start := make(chan struct{})
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for _, s := range []*Store{f.a, f.b} {
				wg.Add(1)
				go func(s *Store) {
					defer wg.Done()
					<-start
					_, err := s.ReserveShared(context.Background(), req)
					results <- err
				}(s)
			}
			close(start)
			wg.Wait()
			close(results)
			admitted, denied := 0, 0
			for err := range results {
				if err == nil {
					admitted++
				} else if sharedStatus(err) == 429 || sharedStatus(err) == 402 {
					denied++
				} else {
					t.Fatal(err)
				}
			}
			if admitted != 1 || denied != 1 {
				t.Fatalf("admitted=%d denied=%d; want exactly one each", admitted, denied)
			}
		})
	}
}

func TestSharedCASAuthAndAtomicRollback(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", RPM: 3, TokensPerDay: 10})
	plain, req := f.key(t, "alpha", keystore.KeyOptions{})
	f.team(t, keystore.TeamRecord{Name: "alpha", RPM: 3, TokensPerDay: 20})
	if _, err := f.a.ReserveShared(context.Background(), req); sharedStatus(err) != 503 {
		t.Fatalf("stale auth: %v", err)
	}
	req = f.request(t, plain)
	req.TokenBound = 21
	if _, err := f.a.ReserveShared(context.Background(), req); sharedStatus(err) != 429 {
		t.Fatalf("quota: %v", err)
	}
	for _, u := range sharedUsage(t, f.b, req.Subject) {
		if u.Used != 0 || u.Reserved != 0 {
			t.Fatalf("denial billed %s: used=%d reserved=%d", u.Kind, u.Used, u.Reserved)
		}
	}
	req.TokenBound = 10
	sharedReserve(t, f.a, req)
	if err := f.keys.Revoke(context.Background(), req.Subject.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.ReserveShared(context.Background(), req); sharedStatus(err) != 503 {
		t.Fatalf("revoked auth: %v", err)
	}
}

func TestSharedUserScopesPolicyCutsAndWarnBlock(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", TokensPerDay: 1, QuotaOnExceeded: "warn"})
	f.team(t, keystore.TeamRecord{Name: "beta"})
	_, a := f.key(t, "alpha", keystore.KeyOptions{Owner: "same-user"})
	_, b := f.key(t, "beta", keystore.KeyOptions{Owner: "same-user"})
	doc := sharedDoc("user-cap", v1alpha1.Subject{User: "same-user"}, sharedQuota("cap", 20, v1alpha1.PeriodCalendarDay))
	putDocument(t, f.db, doc)
	a.PolicyGeneration = f.generation(t)
	b.PolicyGeneration = a.PolicyGeneration
	sharedReserve(t, f.a, a)
	sharedReserve(t, f.b, b)
	doc.Spec.Rules[0].TokenQuota.LimitTokens = 10
	doc.Metadata.Generation++
	putDocument(t, f.db, doc)
	a.PolicyGeneration = f.generation(t)
	if _, err := f.a.ReserveShared(context.Background(), a); sharedStatus(err) != 429 {
		t.Fatalf("cut reset user account: %v", err)
	}
	for _, u := range sharedUsage(t, f.b, b.Subject) {
		if u.Policy == "user-cap" && (u.Used != 20 || u.Limit != 10) {
			t.Fatalf("cut usage: %+v", u)
		}
	}
}

func TestSharedFinishReplayUnknownCancelAndRestart(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", TokensPerDay: 100, BudgetUSDMicros: 10000})
	_, r := f.key(t, "alpha", keystore.KeyOptions{})
	p := sharedReserve(t, f.a, r)
	tokens, cost := int64(4), int64(300)
	done := governance.SharedSettlement{Complete: true, Tokens: &tokens, CostMicroUSD: &cost}
	if err := f.b.FinishShared(context.Background(), p, done); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, f.dsn)
	if err := reopened.InitializeShared(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := reopened.FinishShared(context.Background(), p, done); err != nil {
		t.Fatal(err)
	}
	if err := reopened.CancelShared(context.Background(), p); err == nil {
		t.Fatal("cancel after finish succeeded")
	}
	q := sharedReserve(t, f.a, r)
	if err := f.b.CancelShared(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if err := f.a.CancelShared(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	unknown := sharedReserve(t, f.a, r)
	if err := f.b.FinishShared(context.Background(), unknown, governance.SharedSettlement{}); err != nil {
		t.Fatal(err)
	}
	for _, u := range sharedUsage(t, reopened, r.Subject) {
		switch u.Kind {
		case "tokens":
			if u.Used != 14 || u.Reserved != 10 {
				t.Fatalf("tokens used=%d reserved=%d", u.Used, u.Reserved)
			}
		case "microUSD":
			if u.Used != 1300 || u.Reserved != 1000 {
				t.Fatalf("money used=%d reserved=%d", u.Used, u.Reserved)
			}
		}
	}
	if err := f.a.FinishShared(context.Background(), unknown, done); err == nil {
		t.Fatal("conflicting finish accepted")
	}
}

func TestSharedPolicyMoneyCompetesWithLocalGrants(t *testing.T) {
	for _, sharedFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "shared-first", false: "grant-first"}[sharedFirst], func(t *testing.T) {
			f := newSharedFixture(t)
			f.team(t, keystore.TeamRecord{Name: "alpha"})
			_, r := f.key(t, "alpha", keystore.KeyOptions{})
			putBudget(t, f.db, 1, 1, true, false)
			r.PolicyGeneration = f.generation(t)
			b := currentBudget(t, f.b)
			req := heartbeat("boot")
			req.Requests = []policy.AuthorityGrantRequest{grantRequest(b, 1000)}
			if sharedFirst {
				p := sharedReserve(t, f.a, r)
				resp := syncOK(t, f.b, "node", req)
				if len(resp.Authority.Grants) != 0 {
					t.Fatal("shared reservation minted a second money budget")
				}
				if err := f.b.CancelShared(context.Background(), p); err != nil {
					t.Fatal(err)
				}
				resp = syncOK(t, f.b, "node", req)
				if len(resp.Authority.Grants) != 1 {
					t.Fatal("proven cancellation did not return budget")
				}
			} else {
				resp := syncOK(t, f.b, "node", req)
				if len(resp.Authority.Grants) != 1 {
					t.Fatal("missing local grant")
				}
				if _, err := f.a.ReserveShared(context.Background(), r); sharedStatus(err) != 402 {
					t.Fatalf("local grant ignored: %v", err)
				}
			}
		})
	}
}

func TestSharedInvalidUsageFreezesAndBoundsOverflow(t *testing.T) {
	for _, tokens := range []int64{-1, 11, math.MaxInt64} {
		t.Run(time.Duration(tokens).String(), func(t *testing.T) {
			f := newSharedFixture(t)
			f.team(t, keystore.TeamRecord{Name: "alpha", TokensPerDay: 100})
			_, r := f.key(t, "alpha", keystore.KeyOptions{})
			p := sharedReserve(t, f.a, r)
			err := f.b.FinishShared(context.Background(), p, governance.SharedSettlement{Complete: true, Tokens: &tokens})
			if err == nil {
				t.Fatal("invalid usage succeeded")
			}
			if _, err := f.a.ReserveShared(context.Background(), r); sharedStatus(err) != 503 {
				t.Fatalf("frozen account admitted: %v", err)
			}
		})
	}
}
