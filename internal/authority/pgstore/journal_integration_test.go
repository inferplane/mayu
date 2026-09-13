package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/authority/local"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/policy"
)

// Exercise the actual independently implemented journal and Postgres protocol,
// including acknowledgements for reports from the journal's previous boot.
func TestTwoJournalsTwoReplicasAndJournalRestart(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 10, true, false)
	cpA, cpB := openStore(t, dsn), openStore(t, dsn)
	snapshot := syncOK(t, cpA, "seed", heartbeat("boot"))
	doc := snapshot.Policies[0]
	doc.Spec.Rules[0].Budget.Lease.RenewInterval = "30s"
	putDocument(t, db, doc)
	doc.Metadata.Name = "soft-accounting"
	doc.Spec.Rules[0].Budget.HardCap = false
	putDocument(t, db, doc)

	ctx := context.Background()
	pathA := filepath.Join(t.TempDir(), "authority.sqlite")
	nodeA, err := local.Open(pathA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nodeA.Close() })
	nodeB, err := local.Open(filepath.Join(t.TempDir(), "authority.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nodeB.Close() })
	syncJournal(t, cpA, nodeA, "node-a")
	syncJournal(t, cpB, nodeB, "node-b")
	subject := governance.Subject{Team: "alpha", User: "opaque-user"}
	for _, item := range []struct {
		node *local.Store
		cp   *Store
		name string
		want int64
	}{{nodeA, cpA, "node-a", 60_000}, {nodeB, cpB, "node-b", 40_000}} {
		if permit, err := item.node.Reserve(ctx, subject, item.want); permit != nil || !errors.Is(err, governance.ErrAuthorityUnavailable) {
			t.Fatalf("unfunded journal did not queue authority: %v", err)
		}
		syncJournal(t, item.cp, item.node, item.name)
		permit, err := item.node.Reserve(ctx, subject, item.want)
		if err != nil {
			t.Fatal(err)
		}
		actual := int64(10_000)
		if item.node == nodeB {
			actual = 20_000
		}
		if err := item.node.Finish(ctx, permit, &actual, true); err != nil {
			t.Fatal(err)
		}
		syncJournal(t, item.cp, item.node, item.name)
	}
	before, err := nodeA.Request(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Reports) != 0 || len(before.Meters) != 0 {
		t.Fatal("real journal did not accept report and meter acknowledgements")
	}
	if err := nodeA.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := local.Open(pathA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	pending, err := recovered.Request(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending.Reports) != 1 || pending.Reports[0].Instance != before.Instance ||
		pending.Reports[0].Consumed != 60_000 || !pending.Reports[0].Closed {
		t.Fatal("restart did not retain the entire original grant")
	}
	cpA.Close()
	reopenedCP := openStore(t, dsn)
	syncJournal(t, reopenedCP, recovered, "node-a")
	after, err := recovered.Request(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Reports) != 0 {
		t.Fatal("recovery report's original-boot acknowledgement was not accepted")
	}
	if _, err := recovered.Reserve(ctx, subject, 1); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("recovered node reused old authority: %v", err)
	}
	syncJournal(t, cpB, recovered, "node-a")
	if _, err := recovered.Reserve(ctx, subject, 1); !errors.Is(err, governance.ErrAuthorityExhausted) {
		t.Fatalf("restart returned unproven unused global escrow: %v", err)
	}
	// The other node's existing finite authority remains independently usable.
	permit, err := nodeB.Reserve(ctx, subject, 1)
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if err := nodeB.Finish(ctx, permit, &zero, true); err != nil {
		t.Fatal(err)
	}
}

func syncJournal(t *testing.T, cp *Store, node *local.Store, owner string) policy.SyncResponse {
	t.Helper()
	req, err := node.Request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := cp.Sync(context.Background(), owner, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Apply(context.Background(), *resp.Authority, time.Since(start)); err != nil {
		t.Fatal(err)
	}
	return resp
}

// VACUUM INTO includes committed WAL contents while the original process is
// alive. Reopening this private copy exercises startup recovery without editing
// the journal's encoded state or reconstructing a synthetic outbox.
func snapshotJournal(t *testing.T, path string) string {
	t.Helper()
	copyPath := filepath.Join(t.TempDir(), "snapshot.sqlite")
	f, err := os.OpenFile(copyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `VACUUM INTO ?`, copyPath); err != nil {
		t.Fatal(err)
	}
	return copyPath
}

func TestJournalReopenAfterRefundDoesNotRechargeClosedGrant(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 100, 100, true, false)
	cp := openStore(t, dsn)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "authority.sqlite")
	node, err := local.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = node.Close() })
	syncJournal(t, cp, node, "node")
	subject := governance.Subject{Team: "alpha"}
	if _, err := node.Reserve(ctx, subject, 30_000); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("unfunded journal did not request credit: %v", err)
	}
	granted := syncJournal(t, cp, node, "node")
	if len(granted.Authority.Grants) != 1 || granted.Authority.Grants[0].Amount != 100_000 {
		t.Fatal("expected one full grant before normal settlement")
	}
	// Preserve an OPEN copy before settlement, the server's refund and local
	// acknowledgement GC. Reopening the current journal cannot reproduce this.
	stalePath := snapshotJournal(t, path)
	permit, err := node.Reserve(ctx, subject, 30_000)
	if err != nil {
		t.Fatal(err)
	}
	actual := int64(30_000)
	if err := node.Finish(ctx, permit, &actual, true); err != nil {
		t.Fatal(err)
	}
	// A grant-size edit retires the old local grant without changing the
	// current window or hard limit. The next heartbeat proves its 70000 refund.
	putBudget(t, db, 100, 50, true, false)
	syncJournal(t, cp, node, "node")
	finalized := syncJournal(t, cp, node, "node")
	before, err := node.Request(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Reports) != 0 {
		t.Fatal("normal close was not acknowledged before journal reopen")
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := local.Open(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	req, err := recovered.Request(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Reports) != 1 || req.Reports[0].Instance != before.Instance ||
		req.Reports[0].Consumed != 100_000 || !req.Reports[0].Closed {
		t.Fatal("actual journal did not emit the original-boot synthetic full burn")
	}
	var serverSequence int64
	var serverClosed bool
	if err := db.QueryRow(ctx, `SELECT sequence,closed FROM authority_requests WHERE grant_id=$1`,
		granted.Authority.Grants[0].ID).Scan(&serverSequence, &serverClosed); err != nil {
		t.Fatal(err)
	}
	if !serverClosed || req.Reports[0].Sequence >= serverSequence {
		t.Fatal("fixture did not restore a checkpoint older than the finalized server grant")
	}
	cp.Close()
	reopenedCP := openStore(t, dsn)
	resp := syncJournal(t, reopenedCP, recovered, "node")
	if len(resp.Authority.Grants) != 0 {
		t.Fatal("recovery unexpectedly re-armed grant authority")
	}
	after, err := recovered.Request(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Reports) != 0 {
		t.Fatal("server acknowledgement failed to retire the recovery outbox")
	}
	b := finalized.Authority.Budgets[0]
	if g := issue(t, reopenedCP, "other", "boot", grantRequest(b, 70_000)); g.Amount != 70_000 {
		t.Fatal("journal reopen recharged already-refunded credit")
	}
	extra := heartbeat("boot")
	extra.Requests = []policy.AuthorityGrantRequest{grantRequest(b, 1)}
	if got := syncOK(t, reopenedCP, "third", extra); len(got.Authority.Grants) != 0 {
		t.Fatal("journal reopen returned the same refund again")
	}
}

func TestStaleJournalBurnClosesNewerOpenServerGrant(t *testing.T) {
	dsn, db := testDatabase(t)
	putBudget(t, db, 200, 100, true, false)
	cp := openStore(t, dsn)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "authority.sqlite")
	node, err := local.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = node.Close() })
	syncJournal(t, cp, node, "node")
	subject := governance.Subject{Team: "alpha"}
	if _, err := node.Reserve(ctx, subject, 30_000); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("unfunded journal did not request credit: %v", err)
	}
	granted := syncJournal(t, cp, node, "node")
	stalePath := snapshotJournal(t, path)
	for _, actual := range []int64{10_000, 20_000} {
		permit, err := node.Reserve(ctx, subject, 30_000)
		if err != nil {
			t.Fatal(err)
		}
		if err := node.Finish(ctx, permit, &actual, true); err != nil {
			t.Fatal(err)
		}
		syncJournal(t, cp, node, "node")
	}
	recovered, err := local.Open(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	req, err := recovered.Request(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Reports) != 1 || req.Reports[0].Consumed != 100_000 || req.Reports[0].Observed != 0 ||
		!req.Reports[0].Closed {
		t.Fatal("stale snapshot did not produce a full old-boot burn")
	}
	var sequence, observed int64
	var closed bool
	if err := db.QueryRow(ctx, `SELECT sequence,observed,closed FROM authority_requests WHERE grant_id=$1`,
		req.Reports[0].GrantID).Scan(&sequence, &observed, &closed); err != nil {
		t.Fatal(err)
	}
	if closed || sequence <= req.Reports[0].Sequence || observed != 30_000 {
		t.Fatal("fixture did not retain a newer OPEN checkpoint on the server")
	}
	syncJournal(t, cp, recovered, "node")
	after, err := recovered.Request(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Reports) != 0 {
		t.Fatal("full burn acknowledgement did not drain the restored outbox")
	}
	// Initial synchronization now succeeds; a fresh grant can use only the
	// other 100000, never the retired grant's unspent 70000.
	if _, err := recovered.Reserve(ctx, subject, 100_000); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("retired authority was reused: %v", err)
	}
	syncJournal(t, cp, recovered, "node")
	permit, err := recovered.Reserve(ctx, subject, 100_000)
	if err != nil {
		t.Fatalf("recovered node could not obtain fresh authority: %v", err)
	}
	if err := recovered.Finish(ctx, permit, nil, false); err != nil {
		t.Fatal(err)
	}
	extra := heartbeat("boot")
	extra.Requests = []policy.AuthorityGrantRequest{grantRequest(granted.Authority.Budgets[0], 1)}
	if got := syncOK(t, cp, "other", extra); len(got.Authority.Grants) != 0 {
		t.Fatal("stale OPEN recovery refunded the burned grant")
	}
}
