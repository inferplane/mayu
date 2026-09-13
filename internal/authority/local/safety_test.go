package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/policy"
)

func TestExpirySubtractsFullRTTAndReplayNeverExtends(t *testing.T) {
	s, _ := openTest(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	r := response(definition("hard", true))
	applyTest(t, s, r)
	_, _ = s.Reserve(context.Background(), testSubject, 1)
	want := requestTest(t, s).Requests[0]
	r.Grants = []policy.AuthorityGrant{{ID: "grant", RequestID: want.RequestID,
		Key: want.Key, Revision: want.Revision, WindowID: want.WindowID,
		Amount: 100, ExpiresAt: r.ServerTime.Add(10 * time.Second)}}
	if err := s.Apply(context.Background(), r, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Second)
	p := reserveTest(t, s, 1)
	finishTest(t, s, p, amount(1), true)
	applyTest(t, s, r)
	now = now.Add(time.Second)
	if p, err := s.Reserve(context.Background(), testSubject, 1); p != nil || !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("full RTT/replay expiry violated: %v %v", p, err)
	}
	reports := requestTest(t, s).Reports
	if len(reports) != 1 || !reports[0].Closed || reports[0].Consumed != 1 {
		t.Fatalf("idle expiry cannot prove unused remainder: %#v", reports)
	}
}

func TestWindowEndBoundsLeaseAndClockIgnoresLocalDate(t *testing.T) {
	s, _ := openTest(t)
	now := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	b := definition("hard", true)
	r := response(b)
	r.ServerTime = b.WindowEnd.Add(-2 * time.Second)
	applyTest(t, s, r)
	_, _ = s.Reserve(context.Background(), testSubject, 10)
	want := requestTest(t, s).Requests[0]
	r.Grants = []policy.AuthorityGrant{{ID: "grant", RequestID: want.RequestID,
		Key: want.Key, Revision: want.Revision, WindowID: want.WindowID,
		Amount: 100, ExpiresAt: r.ServerTime.Add(time.Minute)}}
	applyTest(t, s, r)
	reserveTest(t, s, 10)
	now = now.Add(2 * time.Second)
	if _, err := s.Reserve(context.Background(), testSubject, 1); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("grant extended past server window: %v", err)
	}
}

func TestPendingGrantCannotCloseUntilOutcomeIsDurable(t *testing.T) {
	s, _ := openTest(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	funded(t, s, 80, 100, definition("hard", true))
	p := reserveTest(t, s, 80)
	if _, err := s.Reserve(context.Background(), testSubject, 30); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	r := requestTest(t, s)
	if len(r.Reports) != 1 || r.Reports[0].Closed {
		t.Fatalf("pending authority refunded: %#v", r.Reports)
	}
	finishTest(t, s, p, nil, false)
	r = requestTest(t, s)
	if !r.Reports[0].Closed || r.Reports[0].Consumed != 80 {
		t.Fatalf("uncertain expiry settled incorrectly: %#v", r.Reports)
	}
}

func TestIdleInsufficientTailClosesWithoutDuplicateRefund(t *testing.T) {
	s, _ := openTest(t)
	funded(t, s, 80, 100, definition("hard", true))
	p := reserveTest(t, s, 80)
	finishTest(t, s, p, amount(70), true)
	if _, err := s.Reserve(context.Background(), testSubject, 40); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatal(err)
	}
	r := requestTest(t, s)
	if len(r.Reports) != 1 || !r.Reports[0].Closed || r.Reports[0].Consumed != 70 ||
		len(r.Requests) != 1 || r.Requests[0].WantMicroUSD < 40 {
		t.Fatalf("idle retirement/renewal: %#v", r)
	}
	if again := requestTest(t, s); !reflect.DeepEqual(r, again) {
		t.Fatal("retry changed a closed grant report")
	}
}

func TestGrantRequestsArePersistedCapabilitiesAndCoalesceMaxBound(t *testing.T) {
	s, path := openTest(t)
	applyTest(t, s, response(definition("hard", true)))
	for _, n := range []int64{5, 90, 40} {
		_, _ = s.Reserve(context.Background(), testSubject, n)
	}
	first := requestTest(t, s)
	if len(first.Requests) != 1 || first.Requests[0].WantMicroUSD != 90 || len(first.Requests[0].RequestID) != 64 {
		t.Fatalf("coalesced request: %#v", first.Requests)
	}
	if again := requestTest(t, s); !reflect.DeepEqual(first, again) {
		t.Fatal("retry changed request nonce or amount")
	}
	_, _ = s.Reserve(context.Background(), testSubject, 120)
	larger := requestTest(t, s)
	if len(larger.Requests) != 2 {
		t.Fatalf("mutated a sent capability: %#v", larger.Requests)
	}
	next, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	applyTest(t, next, response(definition("hard", true)))
	_, _ = next.Reserve(context.Background(), testSubject, 90)
	fresh := requestTest(t, next)
	if len(fresh.Requests) != 1 || fresh.Requests[0].RequestID == first.Requests[0].RequestID {
		t.Fatal("restarted process reused old request capability")
	}
}

func TestDeniedCreditIs402AndZeroBoundStillRequiresPositiveGrant(t *testing.T) {
	s, _ := openTest(t)
	r := response(definition("hard", true))
	applyTest(t, s, r)
	if _, err := s.Reserve(context.Background(), testSubject, 0); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("zero cost bypassed authority: %v", err)
	}
	want := requestTest(t, s).Requests[0]
	if want.WantMicroUSD < 1 {
		t.Fatal("zero grant requested")
	}
	r.Denied = []policy.AuthorityDenial{{RequestID: want.RequestID, Key: want.Key, Reason: "exhausted"}}
	applyTest(t, s, r)
	if _, err := s.Reserve(context.Background(), testSubject, 0); !errors.Is(err, governance.ErrAuthorityExhausted) {
		t.Fatalf("authoritative exhaustion not preserved: %v", err)
	}
	r.Denied = nil
	r.Grants = []policy.AuthorityGrant{{ID: "positive", RequestID: want.RequestID, Key: want.Key,
		Revision: want.Revision, WindowID: want.WindowID, Amount: 100, ExpiresAt: r.ServerTime.Add(time.Minute)}}
	applyTest(t, s, r)
	reserveTest(t, s, 0)
}

func TestSoftOnlyAndUserScopes(t *testing.T) {
	s, _ := openTest(t)
	soft, hard := definition("soft", false), definition("user-hard", true)
	soft.Team, soft.User = "", "user"
	hard.Team, hard.User = "", "other"
	applyTest(t, s, response(soft, hard))
	for _, team := range []string{"one", "two"} {
		p, err := s.Reserve(context.Background(), governance.Subject{Team: team, User: "user"}, 30)
		if err != nil {
			t.Fatal(err)
		}
		finishTest(t, s, p, amount(10), true)
	}
	r := requestTest(t, s)
	if len(r.Reports) != 0 || len(r.Requests) != 0 || len(r.Meters) != 1 ||
		r.Meters[0].Consumed != 20 || r.Meters[0].Observed != 20 {
		t.Fatalf("user-only meter not shared across teams: %#v", r)
	}
	if _, err := s.Reserve(context.Background(), governance.Subject{Team: "another", User: "other"}, 1); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("user-only hard cap bypassed across teams: %v", err)
	}
}

func TestOldWindowAndBootMetersPersistUntilTheirOwnAck(t *testing.T) {
	s, path := openTest(t)
	b := definition("soft", false)
	applyTest(t, s, response(b))
	p := reserveTest(t, s, 30)
	finishTest(t, s, p, amount(10), true)
	old := requestTest(t, s)
	next, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	applyTest(t, next, response(b))
	p = reserveTest(t, next, 30)
	finishTest(t, next, p, amount(20), true)
	both := requestTest(t, next)
	if len(both.Meters) != 2 {
		t.Fatalf("old meter lost: %#v", both.Meters)
	}
	ack := response(b)
	ack.MeterAcks = []policy.AuthorityAck{{Instance: old.Instance, ID: b.WindowID, Sequence: old.Meters[0].Sequence}}
	applyTest(t, next, ack)
	left := requestTest(t, next)
	if len(left.Meters) != 1 || left.Meters[0].Instance != left.Instance || left.Meters[0].Observed != 20 {
		t.Fatalf("acknowledged wrong boot: %#v", left.Meters)
	}
	newWindow := b
	newWindow.WindowID = "next-window"
	newWindow.WindowStart = b.WindowEnd
	newWindow.WindowEnd = b.WindowEnd.AddDate(0, 0, 1)
	r := response(newWindow)
	r.ServerTime = newWindow.WindowStart.Add(time.Hour)
	applyTest(t, next, r)
	p = reserveTest(t, next, 30)
	finishTest(t, next, p, amount(5), true)
	if meters := requestTest(t, next).Meters; len(meters) != 2 {
		t.Fatalf("rollover lost unacknowledged previous window: %#v", meters)
	}
}

func TestAcknowledgementsCannotDropUnsentOrNewerReports(t *testing.T) {
	s, _ := openTest(t)
	r := funded(t, s, 20, 100, definition("hard", true))
	p := reserveTest(t, s, 20)
	finishTest(t, s, p, amount(10), true)
	old := requestTest(t, s)
	p = reserveTest(t, s, 20)
	finishTest(t, s, p, amount(10), true)
	r.ReportAcks = []policy.AuthorityAck{{ID: old.Reports[0].GrantID, Sequence: old.Reports[0].Sequence + 1}}
	if err := s.Apply(context.Background(), r, 0); !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("acked unsent sequence: %v", err)
	}
	r.ReportAcks[0].Sequence--
	applyTest(t, s, r)
	newer := requestTest(t, s)
	if len(newer.Reports) != 1 || newer.Reports[0].Consumed != 20 ||
		newer.Reports[0].Sequence <= old.Reports[0].Sequence {
		t.Fatalf("old ack swallowed new consumption: %#v", newer.Reports)
	}
}

func TestRestartDoesNotResurrectCollectedAcknowledgedGrant(t *testing.T) {
	s, path := openTest(t)
	r := funded(t, s, 80, 100, definition("hard", true))
	p := reserveTest(t, s, 80)
	finishTest(t, s, p, amount(80), true)
	_, _ = s.Reserve(context.Background(), testSubject, 30)
	before := requestTest(t, s)
	r.ReportAcks = []policy.AuthorityAck{{ID: before.Reports[0].GrantID, Sequence: before.Reports[0].Sequence}}
	applyTest(t, s, r)
	if reports := requestTest(t, s).Reports; len(reports) != 0 {
		t.Fatal("ack not applied")
	}
	next, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	after := requestTest(t, next)
	if len(after.Reports) != 0 {
		t.Fatalf("collected final report was recreated: %#v", after.Reports)
	}
	applyTest(t, next, r)
	if p, err := next.Reserve(context.Background(), testSubject, 1); p != nil || !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("collected old grant rearmed: %v %v", p, err)
	}
}

func TestOutOfOrderResponseCannotRestoreOldDefinitionOrActivateLateGrant(t *testing.T) {
	s, _ := openTest(t)
	old := response(definition("hard", true))
	applyTest(t, s, old)
	_, _ = s.Reserve(context.Background(), testSubject, 1)
	want := requestTest(t, s).Requests[0]
	newer := response(definition("hard", true))
	newer.ServerTime = newer.ServerTime.Add(time.Second)
	newer.Budgets[0].Revision = "new-revision"
	applyTest(t, s, newer)
	old.Grants = []policy.AuthorityGrant{{ID: "late", RequestID: want.RequestID, Key: want.Key,
		Revision: want.Revision, WindowID: want.WindowID, Amount: 100, ExpiresAt: old.ServerTime.Add(time.Minute)}}
	if err := s.Apply(context.Background(), old, 0); !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("stale snapshot reported successful installation: %v", err)
	}
	if _, err := s.Reserve(context.Background(), testSubject, 1); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("stale response authorized: %v", err)
	}
	req := requestTest(t, s)
	if len(req.Reports) != 1 || !req.Reports[0].Closed || req.Reports[0].Consumed != 0 {
		t.Fatalf("unused late grant not retired: %#v", req.Reports)
	}
	for _, w := range req.Requests {
		if w.Revision != "new-revision" {
			t.Fatal("stale snapshot replaced definitions")
		}
	}
}

func TestLateGrantFromPreviousBootIsBurned(t *testing.T) {
	s, path := openTest(t)
	r := response(definition("hard", true))
	applyTest(t, s, r)
	_, _ = s.Reserve(context.Background(), testSubject, 1)
	old := requestTest(t, s)
	w := old.Requests[0]
	next, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	r.Grants = []policy.AuthorityGrant{{ID: "late-old", RequestID: w.RequestID, Key: w.Key, Revision: w.Revision,
		WindowID: w.WindowID, Amount: 100, ExpiresAt: r.ServerTime.Add(time.Minute)}}
	applyTest(t, next, r)
	after := requestTest(t, next)
	if len(after.Reports) != 1 || after.Reports[0].Consumed != 100 || !after.Reports[0].Closed ||
		after.Reports[0].Instance != old.Instance {
		t.Fatalf("late old-boot grant refunded: %#v", after.Reports)
	}
}

func TestRejectsPrivatePathViolationsAndSQLiteURIsAreLiteral(t *testing.T) {
	for _, special := range []string{"", ":memory:"} {
		if s, err := Open(special); err == nil {
			s.Close()
			t.Fatalf("nonpersistent journal accepted: %q", special)
		}
	}
	dir := t.TempDir()
	public := filepath.Join(dir, "public")
	if err := os.WriteFile(public, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(public); err == nil {
		s.Close()
		t.Fatal("public journal accepted")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(public, link); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(link); err == nil {
		s.Close()
		t.Fatal("symlink journal accepted")
	}
	path := filepath.Join(dir, "literal?mode=memory&cache=shared")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		t.Fatalf("URI interpreted instead of private file: %v %v", info, err)
	}
}

func TestDiskErrorNeverReturnsPermitOrFallsBackToMemory(t *testing.T) {
	s, _ := openTest(t)
	funded(t, s, 10, 100, definition("hard", true))
	if _, err := s.db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Reserve(context.Background(), testSubject, 10); p != nil || err == nil {
		t.Fatalf("read-only journal admitted: %v %v", p, err)
	}
	if _, err := s.db.Exec(`PRAGMA query_only=OFF`); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Reserve(context.Background(), testSubject, 10); p != nil || err == nil {
		t.Fatalf("disk failure fell back to memory/rearmed: %v %v", p, err)
	}
}

func TestUncertainOutcomesAndCanceledFinishNeverRelease(t *testing.T) {
	for _, tc := range []struct {
		name     string
		actual   *int64
		complete bool
		cancel   bool
	}{
		{"missing-usage", nil, true, false},
		{"interrupted", amount(4), false, false},
		{"unknown-error", nil, false, false},
		{"cancelled", amount(0), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTest(t)
			funded(t, s, 100, 100, definition("hard", true))
			p := reserveTest(t, s, 100)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			err := s.Finish(ctx, p, tc.actual, tc.complete)
			if err != nil && !tc.cancel {
				t.Fatal(err)
			}
			if p, err := s.Reserve(context.Background(), testSubject, 1); p != nil || err == nil {
				t.Fatalf("uncertain/cancelled outcome returned authority: %v %v", p, err)
			}
		})
	}
}

func TestIncompleteJournalCannotDefaultToNoBudgets(t *testing.T) {
	s, _ := openTest(t)
	funded(t, s, 100, 100, definition("hard", true))
	var raw []byte
	if err := s.db.QueryRow(`SELECT state FROM local_escrow`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "Budgets")
	raw, _ = json.Marshal(fields)
	if _, err := s.db.Exec(`UPDATE local_escrow SET state=?`, raw); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Reserve(context.Background(), testSubject, 1); p != nil || !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("corrupt journal defaulted to allow: %v %v", p, err)
	}
}

func TestOverrunOverflowStillDurablyPoisonsAdmission(t *testing.T) {
	s, path := openTest(t)
	funded(t, s, 10, 100, definition("hard", true))
	p := reserveTest(t, s, 10)
	finishTest(t, s, p, amount(10), true)
	p = reserveTest(t, s, 10)
	if err := s.Finish(context.Background(), p, amount(math.MaxInt64), true); !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("overflow should fail closed: %v", err)
	}
	if p, err := s.Reserve(context.Background(), testSubject, 1); p != nil || err == nil {
		t.Fatalf("overrun overflow failed open: %v %v", p, err)
	}
	next, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	applyTest(t, next, response())
	if p, err := next.Reserve(context.Background(), testSubject, 0); p != nil || !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("restart cleared overrun poison: %v %v", p, err)
	}
}

func TestMalformedResponseFieldsLeaveJournalUntouched(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*policy.AuthorityResponse)
	}{
		{"protocol", func(r *policy.AuthorityResponse) { r.Protocol = "legacy" }},
		{"clock", func(r *policy.AuthorityResponse) { r.ServerTime = time.Time{} }},
		{"negative-limit", func(r *policy.AuthorityResponse) { r.Budgets[0].LimitMicroUSD = -1 }},
		{"duplicate-budget", func(r *policy.AuthorityResponse) { r.Budgets = append(r.Budgets, r.Budgets[0]) }},
		{"zero-amount", func(r *policy.AuthorityResponse) { r.Grants[0].Amount = 0 }},
		{"negative-amount", func(r *policy.AuthorityResponse) { r.Grants[0].Amount = -1 }},
		{"unknown-nonce", func(r *policy.AuthorityResponse) { r.Grants[0].RequestID = strings.Repeat("0", 64) }},
		{"changed-amount", func(r *policy.AuthorityResponse) { r.Grants[0].Amount-- }},
		{"unknown-ack", func(r *policy.AuthorityResponse) { r.ReportAcks = []policy.AuthorityAck{{ID: "missing", Sequence: 1}} }},
		{"unknown-denial", func(r *policy.AuthorityResponse) {
			r.Denied = []policy.AuthorityDenial{{RequestID: "missing", Key: "hard", Reason: "exhausted"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTest(t)
			r := funded(t, s, 10, 100, definition("hard", true))
			before := requestTest(t, s)
			tc.mutate(&r)
			if err := s.Apply(context.Background(), r, 0); !errors.Is(err, governance.ErrAuthorityInvalid) {
				t.Fatalf("invalid response accepted: %v", err)
			}
			if after := requestTest(t, s); !reflect.DeepEqual(before, after) {
				t.Fatal("invalid batch partially committed")
			}
		})
	}
}

func TestOverrunWithOtherPendingPermitWaitsToCloseAndReport(t *testing.T) {
	s, _ := openTest(t)
	funded(t, s, 20, 100, definition("hard", true))
	p1 := reserveTest(t, s, 20)
	p2 := reserveTest(t, s, 20)
	finishTest(t, s, p1, amount(110), true)
	r := requestTest(t, s)
	if len(r.Reports) != 0 {
		t.Fatalf("reported overrun before remaining work settled: %#v", r.Reports)
	}
	finishTest(t, s, p2, nil, false)
	r = requestTest(t, s)
	if len(r.Reports) != 1 || !r.Reports[0].Closed || !r.Reports[0].Overrun ||
		r.Reports[0].Consumed != 130 || r.Reports[0].Observed != 110 {
		t.Fatalf("pending overrun accounting: %#v", r.Reports)
	}
}

func TestFrozenAuthorityDenialIsExhausted(t *testing.T) {
	s, _ := openTest(t)
	r := response(definition("hard", true))
	applyTest(t, s, r)
	_, _ = s.Reserve(context.Background(), testSubject, 1)
	want := requestTest(t, s).Requests[0]
	r.Denied = []policy.AuthorityDenial{{RequestID: want.RequestID, Key: want.Key, Reason: "frozen_window"}}
	applyTest(t, s, r)
	if _, err := s.Reserve(context.Background(), testSubject, 1); !errors.Is(err, governance.ErrAuthorityExhausted) {
		t.Fatalf("frozen hard authority should be exhausted: %v", err)
	}
}

func TestSnapshotCopyBurnsFullAuthorityWhileOriginalProcessLives(t *testing.T) {
	s, _ := openTest(t)
	funded(t, s, 100, 100, definition("hard", true))
	copyPath := filepath.Join(t.TempDir(), "restored.sqlite")
	if err := os.WriteFile(copyPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`VACUUM INTO ?`, copyPath); err != nil {
		t.Fatal(err)
	}
	p := reserveTest(t, s, 90)
	finishTest(t, s, p, amount(90), true)
	restored, err := Open(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	report := requestTest(t, restored)
	if len(report.Reports) != 1 || !report.Reports[0].Closed ||
		report.Reports[0].Consumed != 100 || report.Reports[0].Observed != 0 {
		t.Fatalf("stale snapshot returned credit spent by original: %#v", report.Reports)
	}
	reserveTest(t, s, 10)
}

func TestCloseThenReopenRetainsOutcomesAndBurnsRemainingCredit(t *testing.T) {
	s, path := openTest(t)
	funded(t, s, 30, 100, definition("hard", true))
	p := reserveTest(t, s, 30)
	finishTest(t, s, p, amount(10), true)
	old := requestTest(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	r := requestTest(t, next)
	if r.Instance == old.Instance || len(r.Reports) != 1 ||
		r.Reports[0].Consumed != 100 || r.Reports[0].Observed != 10 {
		t.Fatalf("same-file reopen lost outcome/burn: %#v", r)
	}
	finishTest(t, next, p, amount(10), true)
	if err := next.Finish(context.Background(), p, amount(9), true); err == nil {
		t.Fatal("reopen forgot immutable settlement")
	}
}

func TestSQLiteFullFailsBeforePermitAndPreventsReuse(t *testing.T) {
	s, _ := openTest(t)
	applyTest(t, s, response())
	var pages int
	if err := s.db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA max_page_count=%d", pages)); err != nil {
		t.Fatal(err)
	}
	failed := false
	for range 100 {
		p, err := s.Reserve(context.Background(), testSubject, 1)
		if err != nil {
			if p != nil || !errors.Is(err, governance.ErrAuthorityInvalid) {
				t.Fatalf("full journal returned permit or wrong error: %v %v", p, err)
			}
			failed = true
			break
		}
	}
	if !failed {
		t.Fatal("test did not exhaust actual SQLite storage")
	}
	if p, err := s.Reserve(context.Background(), testSubject, 0); p != nil || err == nil {
		t.Fatalf("full journal fell back to memory: %v %v", p, err)
	}
}

func TestSyncBacklogIsBoundedAndDrainsWithoutLosingMeters(t *testing.T) {
	s, _ := openTest(t)
	const count = 1030
	budgets := make([]policy.AuthorityBudget, count)
	for i := range budgets {
		budgets[i] = definition(fmt.Sprintf("soft-%04d", i), false)
	}
	r := response(budgets...)
	applyTest(t, s, r)
	p := reserveTest(t, s, 10)
	finishTest(t, s, p, amount(5), true)
	seen := map[string]bool{}
	for attempts := 0; attempts < 8 && len(seen) < count; attempts++ {
		req := requestTest(t, s)
		if len(req.Meters)+len(req.Requests)+len(req.Reports) > 1024 {
			t.Fatalf("backend rejects oversized batch: %d", len(req.Meters))
		}
		r.MeterAcks = nil
		for _, m := range req.Meters {
			if seen[m.WindowID] || m.Consumed != 5 || m.Observed != 5 {
				t.Fatalf("meter lost/repeated/changed: %#v", m)
			}
			seen[m.WindowID] = true
			r.MeterAcks = append(r.MeterAcks, policy.AuthorityAck{Instance: m.Instance, ID: m.WindowID, Sequence: m.Sequence})
		}
		applyTest(t, s, r)
	}
	if len(seen) != count {
		t.Fatalf("only %d/%d meters drained", len(seen), count)
	}
}

func TestApplyLockWaitCannotExtendAuthority(t *testing.T) {
	s, _ := openTest(t)
	r := response(definition("hard", true))
	applyTest(t, s, r)
	_, _ = s.Reserve(context.Background(), testSubject, 1)
	want := requestTest(t, s).Requests[0]
	r.Grants = []policy.AuthorityGrant{{ID: "delayed", RequestID: want.RequestID, Key: want.Key,
		Revision: want.Revision, WindowID: want.WindowID, Amount: 100, ExpiresAt: r.ServerTime.Add(time.Second)}}
	s.mu.Lock()
	started := make(chan struct{})
	var once sync.Once
	s.now = func() time.Time {
		once.Do(func() { close(started) })
		return time.Now()
	}
	done := make(chan error, 1)
	go func() { done <- s.Apply(context.Background(), r, 0) }()
	// If Apply cannot sample the clock until after acquiring the lock, the
	// lease is incorrectly extended by this local wait.
	early := true
	select {
	case <-started:
	case <-time.After(100 * time.Millisecond):
		early = false
	}
	s.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !early {
		t.Fatal("Apply sampled its clock only after waiting for the lock")
	}
}

func TestDeadlineCrossedDuringCommitNeverReturnsPermit(t *testing.T) {
	s, _ := openTest(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	funded(t, s, 10, 100, definition("hard", true))
	calls := 0
	s.now = func() time.Time {
		calls++
		if calls == 1 {
			return now
		}
		return now.Add(2 * time.Minute)
	}
	if p, err := s.Reserve(context.Background(), testSubject, 10); p != nil || !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("permit returned after its deadline: %v %v", p, err)
	}
	reports := requestTest(t, s).Reports
	if len(reports) != 1 || !reports[0].Closed || reports[0].Consumed != 0 || reports[0].Observed != 0 {
		t.Fatalf("undispatched reservation not released durably: %#v", reports)
	}
}
