package local

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/inferplane/inferplane/internal/policy"
)

const (
	completedReceiptLimit = 256
	// Covers more than a complete sync batch, including both ack streams.
	replayReceiptLimit = 1024
	// A lost reply can leave remote escrow but no local grant or reservation.
	// Bound the opportunity for late recovery, even within one busy window.
	obsoleteRequestLimit = 1024
)

// replayReceipt proves only that a replay was already finalized. It contains
// no request capability, usable credit, or refund instruction.
type replayReceipt struct {
	Instance  string
	Sequence  int64
	GrantHash string `json:",omitempty"`
}

func grantFingerprint(g policy.AuthorityGrant) string {
	raw, _ := json.Marshal(g)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func reportReceiptID(id string) string { return "grant:" + id }

func meterReceiptID(instance, window string) string {
	raw, _ := json.Marshal([2]string{instance, window})
	return "meter:" + string(raw)
}

func remember(j *journal, id string, receipt replayReceipt) {
	if j.Receipts == nil {
		j.Receipts = map[string]replayReceipt{}
	}
	if _, exists := j.Receipts[id]; !exists {
		j.ReceiptOrder = append(j.ReceiptOrder, id)
	}
	j.Receipts[id] = receipt
	for len(j.ReceiptOrder) > replayReceiptLimit {
		delete(j.Receipts, j.ReceiptOrder[0])
		j.ReceiptOrder = j.ReceiptOrder[1:]
	}
}

// collect drops finalized history, never pending reservations or known
// unacknowledged accounting. Completed receipts are a bounded retry convenience:
// forgetting one makes an ancient Finish fail closed, not repeat a debit or
// refund. Unknown remote escrow may be abandoned without ever requesting a refund.
func collect(j *journal, instance string) {
	seen := map[string]bool{}
	order := make([]string, 0, len(j.Completed))
	for _, id := range j.Completed {
		if p := j.Permits[id]; p != nil && p.Done && !seen[id] {
			seen[id] = true
			order = append(order, id)
		}
	}
	pendingGrants, pendingMeters := map[string]bool{}, map[string]bool{}
	for _, id := range sortedKeys(j.Permits) {
		p := j.Permits[id]
		if p.Done {
			// The counters own accounting after settlement. A retry receipt
			// needs only the immutable outcome, not references to old ledgers.
			p.Grants, p.Meters = nil, nil
			if !seen[id] {
				order = append(order, id)
			}
		} else {
			for _, id := range p.Grants {
				pendingGrants[id] = true
			}
			for _, id := range p.Meters {
				pendingMeters[id] = true
			}
		}
	}
	if excess := len(order) - completedReceiptLimit; excess > 0 {
		for _, id := range order[:excess] {
			delete(j.Permits, id)
		}
		order = order[excess:]
	}
	j.Completed = order
	for _, id := range sortedKeys(j.Grants) {
		g := j.Grants[id]
		if !g.Closed || g.Pending != 0 || g.Reserved != 0 || g.Ack != g.Sequence || pendingGrants[id] {
			continue
		}
		remember(j, reportReceiptID(id), replayReceipt{
			Instance: g.Instance, Sequence: g.Sequence, GrantHash: grantFingerprint(g.Wire)})
		delete(j.Requests, g.Wire.RequestID)
		delete(j.Grants, id)
	}
	for _, id := range sortedKeys(j.Meters) {
		m := j.Meters[id]
		// An acknowledged CURRENT window cannot be forgotten: a later request
		// must continue its cumulative counters even after delete/re-add or a
		// soft/hard policy edit. Only server time can prove that window ended.
		old := m.Wire.Instance != instance || (!m.WindowEnd.IsZero() &&
			!j.ServerTime.IsZero() && !j.ServerTime.Before(m.WindowEnd))
		if old && !pendingMeters[id] && m.Ack == m.Wire.Sequence {
			remember(j, meterReceiptID(m.Wire.Instance, m.Wire.WindowID), replayReceipt{
				Instance: m.Wire.Instance, Sequence: m.Wire.Sequence})
			delete(j.Meters, id)
		}
	}
	collectObsoleteRequests(j)
}

// An obsolete unanswered capability can accept a late reply only until its
// database-owned window ends, subject to a bounded oldest-first recovery queue.
// It cannot be retried under a new boot identity or used for admission. Dropping
// it never sends a close/refund: Postgres continues retaining the entire unknown
// grant. A reply after collection fails closed as an unrequested grant.
func collectObsoleteRequests(j *journal) {
	remaining := map[string]bool{}
	for id, r := range j.Requests {
		if !r.Obsolete || r.GrantID != "" {
			continue
		}
		expired := !j.ServerTime.IsZero() && !r.Budget.WindowEnd.IsZero() &&
			!j.ServerTime.Before(r.Budget.WindowEnd)
		if !r.Sent || (r.Denied != "" && !r.PendingReply) || expired {
			delete(j.Requests, id)
			continue
		}
		remaining[id] = true
	}
	order := make([]string, 0, len(remaining))
	for _, id := range j.ObsoleteRequests {
		if remaining[id] {
			order = append(order, id)
			delete(remaining, id)
		}
	}
	order = append(order, sortedKeys(remaining)...)
	if excess := len(order) - obsoleteRequestLimit; excess > 0 {
		for _, id := range order[:excess] {
			delete(j.Requests, id)
		}
		order = order[excess:]
	}
	j.ObsoleteRequests = order
}

func pruneDeadlines(deadlines map[string]time.Time, j *journal) {
	for id := range deadlines {
		if g := j.Grants[id]; g == nil || g.Closed {
			delete(deadlines, id)
		}
	}
}
