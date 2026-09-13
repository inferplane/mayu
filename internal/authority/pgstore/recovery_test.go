package pgstore

import (
	"context"
	"math"
	"testing"

	"github.com/inferplane/inferplane/internal/policy"
)

func TestOpenGrantStaleRecoveryBurnPreservesAccounting(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		serverSeq, seq, observed int64
		wantSeq, wantObserved    int64
	}{
		{"older checkpoint", 10, 2, 10_000, 10, 20_000},
		{"equal checkpoint", 10, 10, 10_000, 10, 20_000},
		{"newer checkpoint older observation", 10, 12, 10_000, 12, 20_000},
		{"new observation", 10, 2, 25_000, 10, 25_000},
		{"maximum checkpoint", math.MaxInt64, 2, 10_000, math.MaxInt64, 20_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, db := testDatabase(t)
			putBudget(t, db, 200, 100, true, false)
			s := openStore(t, dsn)
			b := currentBudget(t, s)
			intent := grantRequest(b, 1)
			g := issue(t, s, "node", "original-boot", intent)
			original := heartbeat("original-boot")
			original.Reports = []policy.AuthorityReport{{
				Instance: "original-boot", GrantID: g.ID, RequestID: g.RequestID,
				Sequence: tc.serverSeq, Consumed: 30_000, Observed: 20_000,
			}}
			syncOK(t, s, "node", original)

			recovery := heartbeat("recovery-boot")
			recovery.Reports = []policy.AuthorityReport{{
				Instance: "original-boot", GrantID: g.ID, RequestID: g.RequestID,
				Sequence: tc.seq, Consumed: 100_000, Observed: tc.observed, Closed: true,
			}}
			for i := 0; i < 2; i++ {
				resp := syncOK(t, s, "node", recovery)
				if len(resp.Authority.ReportAcks) != 1 ||
					resp.Authority.ReportAcks[0].Sequence != tc.seq ||
					resp.Authority.ReportAcks[0].Instance != "original-boot" {
					t.Fatal("recovery did not acknowledge the incoming outbox checkpoint")
				}
			}
			var consumed, observed, sequence, encumbered, totalConsumed int64
			var closed, frozen bool
			if err := db.QueryRow(context.Background(), `SELECT consumed,observed,sequence,closed
				FROM authority_requests WHERE grant_id=$1`, g.ID).
				Scan(&consumed, &observed, &sequence, &closed); err != nil {
				t.Fatal(err)
			}
			if consumed != 100_000 || observed != tc.wantObserved || sequence != tc.wantSeq || !closed {
				t.Fatalf("incorrect recovered checkpoint: consumed=%d observed=%d sequence=%d closed=%t",
					consumed, observed, sequence, closed)
			}
			if err := db.QueryRow(context.Background(), `SELECT hard_encumbered,hard_consumed,frozen
				FROM authority_accounts WHERE budget_key=$1 AND window_id=$2`, b.Key, b.WindowID).
				Scan(&encumbered, &totalConsumed, &frozen); err != nil {
				t.Fatal(err)
			}
			if encumbered != 100_000 || totalConsumed != 100_000 || frozen {
				t.Fatalf("burn refunded, duplicated or froze liability: encumbered=%d consumed=%d frozen=%t",
					encumbered, totalConsumed, frozen)
			}

			s.Close()
			reopened := openStore(t, dsn)
			syncOK(t, reopened, "node", recovery)
			replay := heartbeat("original-boot")
			replay.Requests = []policy.AuthorityGrantRequest{intent}
			resp := syncOK(t, reopened, "node", replay)
			if len(resp.Authority.Grants) != 0 || len(resp.Authority.Denied) != 1 ||
				resp.Authority.Denied[0].Reason != "closed_grant" {
				t.Fatal("recovered grant was re-armed")
			}
			issue(t, reopened, "other", "boot", grantRequest(b, 100_000))
			extra := heartbeat("boot")
			extra.Requests = []policy.AuthorityGrantRequest{grantRequest(b, 1)}
			if resp := syncOK(t, reopened, "third", extra); len(resp.Authority.Grants) != 0 {
				t.Fatal("recovery returned unused old authority")
			}
		})
	}
}

func TestOpenGrantRecoveryRequiresCapabilityAndFullOldBootBurn(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 100, true, false)
	s := openStore(t, dsn)
	b := currentBudget(t, s)
	g := issue(t, s, "node", "original-boot", grantRequest(b, 1))
	original := heartbeat("original-boot")
	original.Reports = []policy.AuthorityReport{{
		Instance: "original-boot", GrantID: g.ID, RequestID: g.RequestID,
		Sequence: 10, Consumed: 30_000, Observed: 20_000,
	}}
	syncOK(t, s, "node", original)
	for _, tc := range []struct {
		name   string
		change func(*policy.AuthorityRequest, *string)
	}{
		{"wrong owner", func(_ *policy.AuthorityRequest, owner *string) { *owner = "other" }},
		{"wrong capability", func(r *policy.AuthorityRequest, _ *string) { r.Reports[0].RequestID = nonce() }},
		{"wrong original boot", func(r *policy.AuthorityRequest, _ *string) { r.Reports[0].Instance = "other" }},
		{"same boot", func(r *policy.AuthorityRequest, _ *string) { r.Instance = "original-boot" }},
		{"partial burn", func(r *policy.AuthorityRequest, _ *string) { r.Reports[0].Consumed-- }},
		{"open copy", func(r *policy.AuthorityRequest, _ *string) { r.Reports[0].Closed = false }},
		{"explicit overrun", func(r *policy.AuthorityRequest, _ *string) { r.Reports[0].Overrun = true }},
		{"later invalid meter", func(r *policy.AuthorityRequest, _ *string) {
			r.Meters = []policy.AuthorityMeter{{Instance: "recovery-boot", Key: b.Key, WindowID: b.WindowID, Sequence: 1}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := "node"
			req := heartbeat("recovery-boot")
			req.Reports = []policy.AuthorityReport{{
				Instance: "original-boot", GrantID: g.ID, RequestID: g.RequestID,
				Sequence: 2, Consumed: 100_000, Observed: 10_000, Closed: true,
			}}
			tc.change(&req, &owner)
			if _, err := s.Sync(context.Background(), owner, req); err == nil {
				t.Fatal("invalid recovery batch accepted")
			}
			var consumed, observed, sequence, encumbered, totalConsumed int64
			var closed bool
			if err := db.QueryRow(context.Background(), `SELECT r.consumed,r.observed,r.sequence,r.closed,
				a.hard_encumbered,a.hard_consumed FROM authority_requests r
				JOIN authority_accounts a USING(budget_key,window_id) WHERE r.grant_id=$1`, g.ID).
				Scan(&consumed, &observed, &sequence, &closed, &encumbered, &totalConsumed); err != nil {
				t.Fatal(err)
			}
			if consumed != 30_000 || observed != 20_000 || sequence != 10 || closed ||
				encumbered != 100_000 || totalConsumed != 30_000 {
				t.Fatal("rejected recovery batch changed grant or account state")
			}
		})
	}
}
