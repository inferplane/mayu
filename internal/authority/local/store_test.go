package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/policy"
)

var testSubject = governance.Subject{Team: "team", User: "user"}

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "authority.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func definition(key string, hard bool) policy.AuthorityBudget {
	return policy.AuthorityBudget{
		Key: key, Policy: "policy", Rule: key, Team: "team", Revision: "revision",
		WindowID: "window-" + key, WindowStart: time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC),
		WindowEnd:     time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC),
		LimitMicroUSD: 1000, GrantMicroUSD: 100, LeaseSeconds: 300, HardCap: hard,
	}
}

func response(budgets ...policy.AuthorityBudget) policy.AuthorityResponse {
	return policy.AuthorityResponse{Protocol: policy.AuthorityProtocol,
		ServerTime: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC), Budgets: budgets}
}

func applyTest(t *testing.T, s *Store, r policy.AuthorityResponse) {
	t.Helper()
	if err := s.Apply(context.Background(), r, 0); err != nil {
		t.Fatal(err)
	}
}

func requestTest(t *testing.T, s *Store) policy.AuthorityRequest {
	t.Helper()
	r, err := s.Request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func reserveTest(t *testing.T, s *Store, bound int64) *governance.BudgetPermit {
	t.Helper()
	p, err := s.Reserve(context.Background(), testSubject, bound)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.ID == "" || p.BoundMicroUSD != bound {
		t.Fatalf("invalid permit: %#v", p)
	}
	return p
}

func finishTest(t *testing.T, s *Store, p *governance.BudgetPermit, actual *int64, complete bool) {
	t.Helper()
	if err := s.Finish(context.Background(), p, actual, complete); err != nil {
		t.Fatal(err)
	}
}

func amount(n int64) *int64 { return &n }

func funded(t *testing.T, s *Store, bound, credit int64, budgets ...policy.AuthorityBudget) policy.AuthorityResponse {
	t.Helper()
	r := response(budgets...)
	applyTest(t, s, r)
	if p, err := s.Reserve(context.Background(), testSubject, bound); p != nil || !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("missing authority: permit=%v err=%v", p, err)
	}
	req := requestTest(t, s)
	for _, want := range req.Requests {
		r.Grants = append(r.Grants, policy.AuthorityGrant{ID: "grant-" + want.Key,
			RequestID: want.RequestID, Key: want.Key, Revision: want.Revision,
			WindowID: want.WindowID, Amount: credit, ExpiresAt: r.ServerTime.Add(time.Minute)})
	}
	applyTest(t, s, r)
	return r
}

func TestInitialSyncAndPrivateJournal(t *testing.T) {
	s, path := openTest(t)
	if _, err := s.Reserve(context.Background(), testSubject, 0); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("uninitialized admission: %v", err)
	}
	applyTest(t, s, response())
	p := reserveTest(t, s, 0)
	finishTest(t, s, p, amount(0), true)
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("journal mode: %v %v", fi, err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if fi, err := os.Stat(path + suffix); err == nil && fi.Mode().Perm() != 0600 {
			t.Fatalf("private sidecar %s: %v", suffix, fi.Mode())
		}
	}
}

func TestAtomicAcrossScopesAndConcurrentNoOverspend(t *testing.T) {
	s, _ := openTest(t)
	funded(t, s, 10, 100, definition("daily", true), definition("monthly", true))
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, err := s.Reserve(context.Background(), testSubject, 10); err == nil {
				admitted.Add(1)
				if err := s.Finish(context.Background(), p, nil, false); err != nil {
					t.Error(err)
				}
			} else if !errors.Is(err, governance.ErrAuthorityUnavailable) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 10 {
		t.Fatalf("admitted %d; want exactly ten", admitted.Load())
	}
	r := requestTest(t, s)
	if len(r.Reports) != 2 {
		t.Fatalf("reports: %#v", r.Reports)
	}
	for _, report := range r.Reports {
		if report.Consumed != 100 || report.Observed != 0 {
			t.Fatalf("overspent or released uncertain authority: %#v", report)
		}
	}
}

func TestMissingScopeDoesNotConsumeOtherScope(t *testing.T) {
	s, _ := openTest(t)
	a, b := definition("a", true), definition("b", true)
	r := response(a, b)
	applyTest(t, s, r)
	_, _ = s.Reserve(context.Background(), testSubject, 60)
	req := requestTest(t, s)
	var bWant policy.AuthorityGrantRequest
	for _, want := range req.Requests {
		if want.Key == "a" {
			r.Grants = []policy.AuthorityGrant{{ID: "a", RequestID: want.RequestID, Key: want.Key,
				Revision: want.Revision, WindowID: want.WindowID, Amount: 100, ExpiresAt: r.ServerTime.Add(time.Minute)}}
		} else {
			bWant = want
		}
	}
	applyTest(t, s, r)
	for range 3 {
		if p, err := s.Reserve(context.Background(), testSubject, 60); p != nil || !errors.Is(err, governance.ErrAuthorityUnavailable) {
			t.Fatalf("partial admission: %v %v", p, err)
		}
	}
	r.Grants = []policy.AuthorityGrant{{ID: "b", RequestID: bWant.RequestID, Key: bWant.Key,
		Revision: bWant.Revision, WindowID: bWant.WindowID, Amount: 100, ExpiresAt: r.ServerTime.Add(time.Minute)}}
	applyTest(t, s, r)
	reserveTest(t, s, 60)
}

func TestFinishReleasesOnlyProvenUnusedAndIsIdempotent(t *testing.T) {
	s, _ := openTest(t)
	funded(t, s, 60, 100, definition("hard", true), definition("soft", false))
	p := reserveTest(t, s, 60)
	finishTest(t, s, p, amount(20), true)
	first := requestTest(t, s)
	finishTest(t, s, p, amount(20), true)
	if second := requestTest(t, s); !reflect.DeepEqual(first, second) {
		t.Fatalf("duplicate finish changed counters: %#v / %#v", first, second)
	}
	if err := s.Finish(context.Background(), p, amount(21), true); !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("conflicting finish accepted: %v", err)
	}
	p2 := reserveTest(t, s, 80)
	finishTest(t, s, p2, amount(10), false)
	r := requestTest(t, s)
	if len(r.Reports) != 1 || r.Reports[0].Consumed != 100 || r.Reports[0].Observed != 30 {
		t.Fatalf("hard report: %#v", r.Reports)
	}
	if len(r.Meters) != 1 || r.Meters[0].Consumed != 100 || r.Meters[0].Observed != 30 {
		t.Fatalf("soft meter: %#v", r.Meters)
	}
}

func TestReopenBurnsFullOldGrantAndFencesLiveHandle(t *testing.T) {
	s, path := openTest(t)
	funded(t, s, 20, 100, definition("hard", true), definition("soft", false))
	done := reserveTest(t, s, 20)
	finishTest(t, s, done, amount(5), true)
	pending := reserveTest(t, s, 30)
	before := requestTest(t, s)
	next, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	after := requestTest(t, next)
	if before.Instance == after.Instance || after.Instance == "" {
		t.Fatal("boot identity reused")
	}
	if len(after.Reports) != 1 || after.Reports[0].Instance != before.Instance ||
		after.Reports[0].Consumed != 100 || after.Reports[0].Observed != 5 || !after.Reports[0].Closed {
		t.Fatalf("restart refunded old authority: %#v", after.Reports)
	}
	if len(after.Meters) != 1 || after.Meters[0].Instance != before.Instance ||
		after.Meters[0].Consumed != 35 || after.Meters[0].Observed != 5 {
		t.Fatalf("unfinished old meter not retained: %#v", after.Meters)
	}
	if _, err := s.Reserve(context.Background(), testSubject, 1); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("old handle not fenced: %v", err)
	}
	if err := s.Finish(context.Background(), pending, amount(0), true); err == nil {
		t.Fatal("fenced handle refunded a reservation")
	}
	if _, err := next.Reserve(context.Background(), testSubject, 1); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("restart reused authority: %v", err)
	}
}

func TestOverrunPoisonsAndNeverRefunds(t *testing.T) {
	s, _ := openTest(t)
	funded(t, s, 50, 100, definition("hard", true), definition("soft", false))
	p := reserveTest(t, s, 50)
	finishTest(t, s, p, amount(120), true)
	r := requestTest(t, s)
	if len(r.Reports) != 1 || !r.Reports[0].Overrun || !r.Reports[0].Closed ||
		r.Reports[0].Consumed != 120 || r.Reports[0].Observed != 120 {
		t.Fatalf("overrun hidden: %#v", r.Reports)
	}
	if _, err := s.Reserve(context.Background(), testSubject, 0); !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("poisoned authority admitted: %v", err)
	}
}

func TestInvalidBatchCannotInstallCreditOrClearHardBudget(t *testing.T) {
	s, _ := openTest(t)
	r := funded(t, s, 10, 100, definition("hard", true))
	bad := r
	bad.ServerTime = bad.ServerTime.Add(time.Second)
	bad.Budgets = nil
	bad.Grants = append(append([]policy.AuthorityGrant(nil), r.Grants...), policy.AuthorityGrant{ID: "unrequested"})
	if err := s.Apply(context.Background(), bad, 0); !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("invalid response accepted: %v", err)
	}
	reserveTest(t, s, 100)
	if _, err := s.Reserve(context.Background(), testSubject, 1); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("invalid batch loosened budget: %v", err)
	}
}

func TestGrantReplayDoesNotRefill(t *testing.T) {
	s, _ := openTest(t)
	r := funded(t, s, 100, 100, definition("hard", true))
	p := reserveTest(t, s, 100)
	finishTest(t, s, p, nil, false)
	applyTest(t, s, r)
	if _, err := s.Reserve(context.Background(), testSubject, 1); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("duplicate response rearmed grant: %v", err)
	}
}
