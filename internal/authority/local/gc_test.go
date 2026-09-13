package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/policy"
)

func readJournal(t *testing.T, s *Store) (*journal, int) {
	t.Helper()
	var raw []byte
	if err := s.db.QueryRow(`SELECT state FROM local_escrow WHERE id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var j journal
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatal(err)
	}
	return &j, len(raw)
}

func TestCompletedReceiptRetentionIsBoundedWithoutLosingAccounting(t *testing.T) {
	s, _ := openTest(t)
	hard, soft := definition("hard", true), definition("soft", false)
	hard.GrantMicroUSD, hard.LimitMicroUSD = 1_000_000, 2_000_000
	funded(t, s, 2, 1_000_000, hard, soft)
	var oldest, latest *governance.BudgetPermit
	for i := range 768 {
		p := reserveTest(t, s, 2)
		if i == 0 {
			oldest = p
		}
		latest = p
		finishTest(t, s, p, amount(1), true)
	}
	pending := reserveTest(t, s, 9)
	j, size := readJournal(t, s)
	if len(j.Permits) > 257 || size > 128<<10 {
		t.Fatalf("retained lifetime receipts: permits=%d bytes=%d", len(j.Permits), size)
	}
	if j.Permits[pending.ID] == nil || j.Permits[pending.ID].Done {
		t.Fatal("receipt eviction removed pending liability")
	}
	before := requestTest(t, s)
	if len(before.Reports) != 1 || before.Reports[0].Consumed != 768 ||
		len(before.Meters) != 1 || before.Meters[0].Consumed != 768 {
		t.Fatalf("receipt collection changed accounting: %#v", before)
	}
	finishTest(t, s, latest, amount(1), true)
	if err := s.Finish(context.Background(), oldest, amount(1), true); !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("ancient receipt not rejected: %v", err)
	}
	after := requestTest(t, s)
	if after.Reports[0].Consumed != 768 || after.Meters[0].Consumed != 768 {
		t.Fatal("late finish duplicated debit or refund")
	}
	finishTest(t, s, pending, nil, false)
}

func TestAcknowledgedGrantHistoryIsCollectedAndNeverResurrected(t *testing.T) {
	s, _ := openTest(t)
	b := definition("hard", true)
	b.GrantMicroUSD = 1
	b.LimitMicroUSD = 10_000
	r := response(b)
	applyTest(t, s, r)
	var first, last policy.AuthorityResponse
	var consumed int64
	for i := range 1100 {
		if _, err := s.Reserve(context.Background(), testSubject, 1); !errors.Is(err, governance.ErrAuthorityUnavailable) {
			t.Fatalf("cycle %d did not exhaust previous grant: %v", i, err)
		}
		req := requestTest(t, s)
		if len(req.Requests) != 1 {
			t.Fatalf("cycle %d: wants=%d", i, len(req.Requests))
		}
		r = response(b)
		r.ServerTime = r.ServerTime.Add(time.Duration(i+1) * time.Microsecond)
		for _, report := range req.Reports {
			if !report.Closed || report.Consumed != 1 || report.Observed != 1 {
				t.Fatalf("false refund: %#v", report)
			}
			consumed += report.Consumed
			r.ReportAcks = append(r.ReportAcks, policy.AuthorityAck{ID: report.GrantID, Instance: report.Instance, Sequence: report.Sequence})
		}
		want := req.Requests[0]
		r.Grants = []policy.AuthorityGrant{{ID: fmt.Sprintf("grant-%04d", i), RequestID: want.RequestID,
			Key: want.Key, Revision: want.Revision, WindowID: want.WindowID, Amount: 1,
			ExpiresAt: r.ServerTime.Add(time.Minute)}}
		applyTest(t, s, r)
		if i == 0 {
			first = r
		}
		last = r
		p := reserveTest(t, s, 1)
		finishTest(t, s, p, amount(1), true)
	}
	_, _ = s.Reserve(context.Background(), testSubject, 1)
	req := requestTest(t, s)
	r = response(b)
	r.ServerTime = r.ServerTime.Add(time.Second)
	for _, report := range req.Reports {
		consumed += report.Consumed
		r.ReportAcks = append(r.ReportAcks, policy.AuthorityAck{ID: report.GrantID, Instance: report.Instance, Sequence: report.Sequence})
	}
	applyTest(t, s, r)
	j, size := readJournal(t, s)
	if len(j.Grants) != 0 || len(j.Requests) != 1 || len(j.Permits) > 256 ||
		len(j.Receipts) > 1024 || len(j.ReceiptOrder) > 1024 || size > 600<<10 || len(s.deadlines) != 0 {
		t.Fatalf("unbounded history: grants=%d wants=%d permits=%d bytes=%d deadlines=%d",
			len(j.Grants), len(j.Requests), len(j.Permits), size, len(s.deadlines))
	}
	if consumed != 1100 {
		t.Fatalf("duplicate/missing consumption: %d", consumed)
	}
	t.Logf("1100 grants finalized: active=%d requests=%d completed=%d replay=%d bytes=%d",
		len(j.Grants), len(j.Requests), len(j.Permits), len(j.Receipts), size)
	// The retained receipt still recognizes this replay, but its old policy
	// snapshot cannot be published as current by the syncer.
	if err := s.Apply(context.Background(), last, 0); !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("stale finalized replay reported successful installation: %v", err)
	}
	if err := s.Apply(context.Background(), first, 0); !errors.Is(err, governance.ErrAuthorityInvalid) {
		t.Fatalf("ancient grant replay unexpectedly accepted: %v", err)
	}
	if p, err := s.Reserve(context.Background(), testSubject, 1); p != nil || !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("collected credit resurrected: %v %v", p, err)
	}
}

func TestOldMeterGCWaitsForAckAndPendingPermits(t *testing.T) {
	s, _ := openTest(t)
	b := definition("soft", false)
	r := response(b)
	applyTest(t, s, r)
	pending := reserveTest(t, s, 7)
	for i := range 48 {
		next := b
		next.WindowStart = b.WindowStart.AddDate(0, i+1, 0)
		next.WindowEnd = next.WindowStart.AddDate(0, 1, 0)
		next.WindowID = fmt.Sprintf("month-%d", i)
		r = response(next)
		r.ServerTime = next.WindowStart.Add(time.Hour)
		applyTest(t, s, r)
		p := reserveTest(t, s, 2)
		finishTest(t, s, p, amount(1), true)
		req := requestTest(t, s)
		r.MeterAcks = nil
		for _, m := range req.Meters {
			r.MeterAcks = append(r.MeterAcks, policy.AuthorityAck{Instance: m.Instance, ID: m.WindowID, Sequence: m.Sequence})
		}
		applyTest(t, s, r)
	}
	j, _ := readJournal(t, s)
	if len(j.Meters) != 2 || j.Permits[pending.ID] == nil {
		t.Fatalf("old meters grew or pending meter vanished: %d", len(j.Meters))
	}
	finishTest(t, s, pending, amount(3), true)
	req := requestTest(t, s)
	if len(req.Meters) != 1 || req.Meters[0].WindowID != b.WindowID || req.Meters[0].Consumed != 3 {
		t.Fatalf("pending old window was not settled: %#v", req.Meters)
	}
	r.MeterAcks = []policy.AuthorityAck{{Instance: req.Instance, ID: b.WindowID, Sequence: req.Meters[0].Sequence}}
	applyTest(t, s, r)
	j, _ = readJournal(t, s)
	if len(j.Meters) != 1 {
		t.Fatalf("acknowledged old window not collected: %d", len(j.Meters))
	}
}

func TestObsoleteDeniedRequestsCollectedButUnansweredCapabilitiesSurviveWithinWindow(t *testing.T) {
	s, _ := openTest(t)
	b := definition("hard", true)
	applyTest(t, s, response(b))
	_, _ = s.Reserve(context.Background(), testSubject, 1)
	unanswered := requestTest(t, s).Requests[0]
	for i := range 48 {
		next := b
		next.Revision = fmt.Sprintf("denied-revision-%d", i)
		r := response(next)
		r.ServerTime = r.ServerTime.Add(time.Duration(i+1) * time.Second)
		applyTest(t, s, r)
		_, _ = s.Reserve(context.Background(), testSubject, 1)
		req := requestTest(t, s)
		if len(req.Requests) != 1 {
			t.Fatalf("current wants: %d", len(req.Requests))
		}
		want := req.Requests[0]
		r.Denied = []policy.AuthorityDenial{{RequestID: want.RequestID, Key: want.Key, Reason: "exhausted"}}
		applyTest(t, s, r)
	}
	j, _ := readJournal(t, s)
	if len(j.Requests) != 2 || j.Requests[unanswered.RequestID] == nil {
		t.Fatalf("denied history grew or unanswered liability lost: %d", len(j.Requests))
	}
	retried := requestTest(t, s).Requests[0]
	removed := response()
	removed.ServerTime = j.ServerTime.Add(time.Second)
	applyTest(t, s, removed)
	j, _ = readJournal(t, s)
	if j.Requests[retried.RequestID] == nil || j.Requests[unanswered.RequestID] == nil {
		t.Fatal("retry outstanding after an earlier denial was collected")
	}
}
