package local

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/policy"
)

// Spending between every acknowledged batch keeps the first batch dirty. A
// fixed sorted prefix must not hide the remaining scopes from the control plane.
func TestReportAndMeterRotationUnderContinuousSpending(t *testing.T) {
	for _, hard := range []bool{true, false} {
		name := "meters"
		if hard {
			name = "reports"
		}
		t.Run(name, func(t *testing.T) {
			s, _ := openTest(t)
			r := fundManyScopes(t, s, 342, hard)
			seen := map[string]bool{}
			for range 4 {
				p := reserveTest(t, s, 1)
				finishTest(t, s, p, amount(1), true)
				req := requestTest(t, s)
				if len(req.Requests)+len(req.Reports)+len(req.Meters) > 1024 {
					t.Fatal("rotation exceeded the protocol batch limit")
				}
				r.ReportAcks, r.MeterAcks = nil, nil
				for _, report := range req.Reports {
					if report.Consumed <= 0 || report.Consumed != report.Observed {
						t.Fatalf("report lost settled cost: %#v", report)
					}
					seen[report.GrantID] = true
					r.ReportAcks = append(r.ReportAcks, policy.AuthorityAck{
						Instance: report.Instance, ID: report.GrantID, Sequence: report.Sequence,
					})
				}
				for _, m := range req.Meters {
					if m.Consumed <= 0 || m.Consumed != m.Observed {
						t.Fatalf("meter lost settled cost: %#v", m)
					}
					seen[m.Key] = true
					r.MeterAcks = append(r.MeterAcks, policy.AuthorityAck{
						Instance: m.Instance, ID: m.WindowID, Sequence: m.Sequence,
					})
				}
				applyTest(t, s, r)
			}
			if len(seen) != 342 {
				t.Fatalf("busy scopes starved the backlog: reported %d/342 scopes", len(seen))
			}
		})
	}
}

func TestReportAndMeterRotationSurvivesRestartWithoutAcknowledgements(t *testing.T) {
	for _, hard := range []bool{true, false} {
		name := "meters"
		if hard {
			name = "reports"
		}
		t.Run(name, func(t *testing.T) {
			s, path := openTest(t)
			fundManyScopes(t, s, 342, hard)
			p := reserveTest(t, s, 1)
			finishTest(t, s, p, amount(1), true)
			seen := map[string]bool{}
			record := func(req policy.AuthorityRequest) {
				for _, report := range req.Reports {
					seen[report.GrantID] = true
				}
				for _, m := range req.Meters {
					seen[m.Key] = true
				}
			}
			record(requestTest(t, s))
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			next, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			record(requestTest(t, next))
			if len(seen) != 342 {
				t.Fatalf("restart repeated the same prefix: reported %d/342 scopes", len(seen))
			}
		})
	}
}

// This uses real reservations and the wire protocol, including multiple grant
// batches, so the fairness tests also verify that no sequence/debit is invented.
func fundManyScopes(t *testing.T, s *Store, count int, hard bool) policy.AuthorityResponse {
	t.Helper()
	budgets := make([]policy.AuthorityBudget, count)
	for i := range budgets {
		budgets[i] = definition(fmt.Sprintf("scope-%04d", i), hard)
	}
	r := response(budgets...)
	applyTest(t, s, r)
	if !hard {
		return r
	}
	if _, err := s.Reserve(context.Background(), testSubject, 1); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("unfunded scopes admitted: %v", err)
	}
	for issued := 0; issued < count; {
		req := requestTest(t, s)
		if len(req.Requests) == 0 {
			t.Fatal("grant backlog stopped progressing")
		}
		r.Grants, r.ReportAcks = nil, nil
		for _, want := range req.Requests {
			r.Grants = append(r.Grants, policy.AuthorityGrant{
				ID: "grant-" + want.Key, RequestID: want.RequestID, Key: want.Key,
				Revision: want.Revision, WindowID: want.WindowID, Amount: 100,
				ExpiresAt: r.ServerTime.Add(time.Minute),
			})
			issued++
		}
		for _, report := range req.Reports {
			r.ReportAcks = append(r.ReportAcks, policy.AuthorityAck{
				Instance: report.Instance, ID: report.GrantID, Sequence: report.Sequence,
			})
		}
		applyTest(t, s, r)
	}
	r.Grants, r.ReportAcks = nil, nil
	return r
}

func TestObsoleteUnansweredCapabilitiesExpireWithoutRefund(t *testing.T) {
	s, path := openTest(t)
	b := definition("hard", true)
	var oldest policy.AuthorityGrantRequest
	for i := range 64 {
		next := b
		next.WindowStart = b.WindowStart.AddDate(0, i, 0)
		next.WindowEnd = next.WindowStart.AddDate(0, 1, 0)
		next.WindowID = fmt.Sprintf("lost-window-%d", i)
		r := response(next)
		r.ServerTime = next.WindowStart.Add(time.Hour)
		applyTest(t, s, r)
		_, _ = s.Reserve(context.Background(), testSubject, 1)
		req := requestTest(t, s)
		if len(req.Requests) != 1 {
			t.Fatalf("unexpected current wants: %#v", req.Requests)
		}
		if i == 0 {
			oldest = req.Requests[0]
		}
	}
	expired := response()
	expired.ServerTime = b.WindowStart.AddDate(10, 0, 0)
	applyTest(t, s, expired)
	j, size := readJournal(t, s)
	if len(j.Requests) != 0 {
		t.Fatalf("expired unanswered history retained: requests=%d bytes=%d", len(j.Requests), size)
	}
	if req := requestTest(t, s); len(req.Reports) != 0 || len(req.Requests) != 0 {
		t.Fatalf("abandoning unknown escrow created a refund or reissued credit: %#v", req)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	late := response(b)
	late.Grants = []policy.AuthorityGrant{{
		ID: "forgotten", RequestID: oldest.RequestID, Key: oldest.Key, Revision: oldest.Revision,
		WindowID: oldest.WindowID, Amount: 100, ExpiresAt: late.ServerTime.Add(time.Minute),
	}}
	if err := reopened.Apply(context.Background(), late, 0); !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("forgotten capability replay was not refused: %v", err)
	}
	if req := requestTest(t, reopened); len(req.Reports) != 0 {
		t.Fatalf("forgotten capability replay created a refund: %#v", req.Reports)
	}
	if p, err := reopened.Reserve(context.Background(), testSubject, 1); p != nil || err == nil {
		t.Fatalf("forgotten capability rearmed after restart: %v %v", p, err)
	}
}

func TestObsoleteUnansweredCapabilitiesBoundedWithinOneWindow(t *testing.T) {
	s, _ := openTest(t)
	const count = 1100
	budgets := make([]policy.AuthorityBudget, count)
	for i := range budgets {
		budgets[i] = definition(fmt.Sprintf("lost-%04d", i), true)
	}
	r := response(budgets...)
	applyTest(t, s, r)
	_, _ = s.Reserve(context.Background(), testSubject, 1)
	sent := map[string]policy.AuthorityGrantRequest{}
	for range 4 {
		for _, want := range requestTest(t, s).Requests {
			sent[want.RequestID] = want
		}
	}
	if len(sent) != count {
		t.Fatalf("fixture did not send every capability: %d", len(sent))
	}
	removed := response()
	removed.ServerTime = r.ServerTime.Add(time.Second)
	applyTest(t, s, removed)
	j, size := readJournal(t, s)
	if len(j.Requests) == 0 || len(j.Requests) > 1024 {
		t.Fatalf("obsolete late-recovery history is not bounded: requests=%d bytes=%d", len(j.Requests), size)
	}
	var forgotten, retained policy.AuthorityGrantRequest
	for id, want := range sent {
		if j.Requests[id] == nil {
			forgotten = want
		} else {
			retained = want
		}
	}
	reply := func(want policy.AuthorityGrantRequest) policy.AuthorityResponse {
		out := removed
		out.Grants = []policy.AuthorityGrant{{
			ID: "late-" + want.Key, RequestID: want.RequestID, Key: want.Key, Revision: want.Revision,
			WindowID: want.WindowID, Amount: 100, ExpiresAt: r.ServerTime.Add(time.Minute),
		}}
		return out
	}
	if err := s.Apply(context.Background(), reply(forgotten), 0); !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("forgotten recovery accepted a grant: %v", err)
	}
	applyTest(t, s, reply(retained))
	reports := requestTest(t, s).Reports
	if len(reports) != 1 || !reports[0].Closed || reports[0].Consumed != 0 || reports[0].Observed != 0 {
		t.Fatalf("retained late reply did not retire proven unused authority: %#v", reports)
	}
}

func TestObsoleteCapabilityGCDoesNotDropKnownPendingLiabilities(t *testing.T) {
	s, _ := openTest(t)
	hard, soft := definition("known", true), definition("meter", false)
	funded(t, s, 10, 100, hard, soft)
	pending := reserveTest(t, s, 10)
	unanswered := definition("unanswered", true)
	r := response(hard, soft, unanswered)
	r.ServerTime = r.ServerTime.Add(time.Second)
	applyTest(t, s, r)
	_, _ = s.Reserve(context.Background(), testSubject, 1)
	if len(requestTest(t, s).Requests) != 1 {
		t.Fatal("fixture did not send an unanswered request")
	}
	r = response()
	r.ServerTime = hard.WindowEnd
	applyTest(t, s, r)
	j, _ := readJournal(t, s)
	if len(j.Requests) != 1 || len(j.Grants) != 1 || len(j.Meters) != 1 || j.Permits[pending.ID] == nil {
		t.Fatalf("collection lost known accounting or retained expired capability: requests=%d grants=%d meters=%d",
			len(j.Requests), len(j.Grants), len(j.Meters))
	}
	finishTest(t, s, pending, nil, false)
	req := requestTest(t, s)
	if len(req.Reports) != 1 || !req.Reports[0].Closed || req.Reports[0].Consumed != 10 ||
		len(req.Meters) != 1 || req.Meters[0].Consumed != 10 {
		t.Fatalf("collection changed pending settlement: %#v", req)
	}
}

func TestInvalidUsageRetainsAuthorityAndPoisonsWithoutInventingActual(t *testing.T) {
	s, path := openTest(t)
	funded(t, s, 20, 100, definition("hard", true), definition("soft", false))
	p := reserveTest(t, s, 20)
	p.InvalidUsage = true
	finishTest(t, s, p, nil, true)
	before := requestTest(t, s)
	if len(before.Reports) != 1 || !before.Reports[0].Overrun || !before.Reports[0].Closed ||
		before.Reports[0].Consumed != 20 || before.Reports[0].Observed != 0 ||
		len(before.Meters) != 1 || before.Meters[0].Consumed != 20 || before.Meters[0].Observed != 0 {
		t.Fatalf("invalid usage lost authority, overrun, or fabricated dollars: %#v", before)
	}
	finishTest(t, s, p, nil, true)
	if after := requestTest(t, s); !reflect.DeepEqual(before, after) {
		t.Fatal("identical invalid-usage retry changed its outcome")
	}
	unflagged := *p
	unflagged.InvalidUsage = false
	if err := s.Finish(context.Background(), &unflagged, nil, false); !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("invalid-usage flag was not part of immutable settlement: %v", err)
	}
	if next, err := s.Reserve(context.Background(), testSubject, 0); next != nil || !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("invalid usage did not poison admission: %v %v", next, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	finishTest(t, reopened, p, nil, true)
	applyTest(t, reopened, response())
	if next, err := reopened.Reserve(context.Background(), testSubject, 0); next != nil || !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("restart cleared invalid-usage poison: %v %v", next, err)
	}
}

func TestInvalidUsageWaitsForOtherPendingPermitBeforeOverrunReport(t *testing.T) {
	s, _ := openTest(t)
	funded(t, s, 20, 100, definition("hard", true))
	bad, pending := reserveTest(t, s, 20), reserveTest(t, s, 20)
	bad.InvalidUsage = true
	finishTest(t, s, bad, amount(3), true)
	if req := requestTest(t, s); len(req.Reports) != 0 {
		t.Fatalf("invalid usage reported an overrun before all work settled: %#v", req.Reports)
	}
	finishTest(t, s, pending, amount(5), true)
	reports := requestTest(t, s).Reports
	if len(reports) != 1 || !reports[0].Overrun || !reports[0].Closed ||
		reports[0].Consumed != 25 || reports[0].Observed != 8 {
		t.Fatalf("invalid usage lost bound or known observations: %#v", reports)
	}
}
