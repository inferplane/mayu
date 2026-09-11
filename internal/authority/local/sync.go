package local

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/inferplane/inferplane/internal/policy"
)

// Request snapshots durable cumulative reports and persisted grant capabilities.
// Retries preserve each item's sequence and values; fair batch selection may
// rotate the returned items when a stream has more work than fits in one batch.
func (s *Store) Request(ctx context.Context) (policy.AuthorityRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := policy.AuthorityRequest{Protocol: policy.AuthorityProtocol, Instance: s.instance}
	err := s.transaction(ctx, func(j *journal) error {
		if err := s.closeInactive(j, s.now()); err != nil {
			return err
		}
		// The authority accepts at most 1024 entries per request. Allocate a
		// share to each stream so neither a report backlog nor denied wants
		// starves the other kinds of synchronization work.
		const perStream = 1024 / 3
		for _, id := range rotatingKeys(j.Requests, j.RequestCursor) {
			if len(out.Requests) == perStream {
				break
			}
			r := j.Requests[id]
			b, ok := j.Budgets[r.Wire.Key]
			if r.Instance != s.instance || r.Obsolete || r.GrantID != "" || !ok || !sameDefinition(r.Budget, b) {
				continue
			}
			r.Sent = true
			r.PendingReply = true
			out.Requests = append(out.Requests, r.Wire)
			j.RequestCursor = id
		}
		for _, id := range rotatingKeys(j.Grants, j.ReportCursor) {
			if len(out.Reports) == perStream {
				break
			}
			g := j.Grants[id]
			if g.Ack == g.Sequence || (g.Overrun && !g.Closed) {
				continue
			}
			g.Sent = g.Sequence
			out.Reports = append(out.Reports, policy.AuthorityReport{
				Instance: g.Instance, GrantID: g.Wire.ID, RequestID: g.Wire.RequestID,
				Sequence: g.Sequence, Consumed: g.Consumed, Observed: g.Observed,
				Closed: g.Closed, Overrun: g.Overrun})
			j.ReportCursor = id
		}
		for _, id := range rotatingKeys(j.Meters, j.MeterCursor) {
			if len(out.Meters) == perStream {
				break
			}
			m := j.Meters[id]
			if m.Ack == m.Wire.Sequence {
				continue
			}
			m.Sent = m.Wire.Sequence
			out.Meters = append(out.Meters, m.Wire)
			j.MeterCursor = id
		}
		return nil
	})
	if err != nil {
		return policy.AuthorityRequest{}, err
	}
	return out, nil
}

func rotatingKeys[V any](values map[string]V, cursor string) []string {
	keys := sortedKeys(values)
	start := sort.Search(len(keys), func(i int) bool { return keys[i] > cursor })
	return append(keys[start:], keys[:start]...)
}

// Apply validates and commits the complete response atomically. Its measured
// round trip is subtracted in full from server-clock lease/window lifetimes.
// Deadlines are never persisted or reconstructed from the machine's wall clock.
// The syncer validates the bundle and publishes its policies only after Apply
// succeeds. A stale snapshot may retire late grants/reports but returns an error
// so the caller cannot publish financial constraints the journal discarded.
func (s *Store) Apply(ctx context.Context, response policy.AuthorityResponse, roundTrip time.Duration) error {
	// Anchor at method entry, before validation and mutex/SQLite waits. Local
	// contention must consume lease lifetime, never extend it.
	received := s.now()
	if response.Protocol != policy.AuthorityProtocol || !utcTime(response.ServerTime) || roundTrip < 0 {
		return invalid("invalid protocol, server clock, or round trip")
	}
	budgets, err := validateBudgets(response)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	deadlines := cloneTimes(s.deadlines)
	windows := cloneTimes(s.windows)
	var applied *journal
	var staleSnapshot bool
	err = s.transaction(ctx, func(j *journal) error {
		applied = j
		stale := !j.ServerTime.IsZero() && response.ServerTime.Before(j.ServerTime)
		staleSnapshot = stale
		advance := !j.Ready || response.ServerTime.After(j.ServerTime)
		if !stale {
			// Equal server timestamps may add grants, but cannot revise a
			// snapshot or extend its deadlines on a delayed duplicate reply.
			if !advance && !equalBudgets(j.Budgets, budgets) {
				return invalid("conflicting snapshot at the same server time")
			}
			if advance {
				j.Budgets = budgets
				j.ServerTime = response.ServerTime
				j.Ready = true
				windows = make(map[string]time.Time, len(budgets))
				for key, b := range budgets {
					windows[key] = deadline(received, response.ServerTime, b.WindowEnd, roundTrip)
				}
			}
		}
		seen := map[string]bool{}
		grantIDs := map[string]bool{}
		for _, wire := range response.Grants {
			if seen[wire.RequestID] || grantIDs[wire.ID] {
				return invalid("duplicate grant or grant request in response")
			}
			seen[wire.RequestID], grantIDs[wire.ID] = true, true
			if receipt, ok := j.Receipts[reportReceiptID(wire.ID)]; ok {
				if receipt.GrantHash != grantFingerprint(wire) {
					return invalid("conflicting finalized grant replay")
				}
				continue
			}
			r := j.Requests[wire.RequestID]
			if err := validateGrant(wire, r, response.ServerTime); err != nil {
				return err
			}
			if old := j.Grants[wire.ID]; old != nil {
				if !equalGrant(old.Wire, wire) {
					return invalid("conflicting grant replay")
				}
				// No amounts, closure state or deadlines are reset on replay.
				continue
			}
			if r.GrantID != "" {
				return invalid("request capability already issued a different grant")
			}
			g := &grant{Wire: wire, Instance: r.Instance, Sequence: 1}
			expiry := wire.ExpiresAt
			if expiry.After(r.Budget.WindowEnd) {
				expiry = r.Budget.WindowEnd
			}
			until := deadline(received, response.ServerTime, expiry, roundTrip)
			b, ok := j.Budgets[wire.Key]
			if r.Instance != s.instance {
				// A stale restored journal may not know that the previous boot
				// already received and spent this grant. Burn it, never refund.
				g.Consumed, g.Closed = wire.Amount, true
			} else if stale || r.Obsolete || !ok || !b.HardCap || !sameDefinition(r.Budget, b) || !received.Before(until) {
				g.Closed = true
			} else {
				deadlines[wire.ID] = until
			}
			j.Grants[wire.ID] = g
			r.GrantID, r.Denied = wire.ID, ""
			r.PendingReply = false
		}
		for _, denial := range response.Denied {
			r := j.Requests[denial.RequestID]
			if seen[denial.RequestID] || r == nil || !r.Sent || r.Wire.Key != denial.Key || !validString(denial.Reason) {
				return invalid("invalid grant denial")
			}
			seen[denial.RequestID] = true
			// A delayed denial never takes away already-issued local credit.
			if r.GrantID == "" && !stale {
				r.Denied = denial.Reason
				r.PendingReply = false
			}
		}
		if err := applyAcks(j, response); err != nil {
			return err
		}
		for _, r := range j.Requests {
			b, ok := j.Budgets[r.Wire.Key]
			if !ok || !sameDefinition(r.Budget, b) {
				r.Obsolete = true
			}
		}
		for id, g := range j.Grants {
			b, ok := j.Budgets[g.Wire.Key]
			if !g.Closed && g.Pending == 0 && (!ok || !b.HardCap ||
				g.Wire.Revision != b.Revision || g.Wire.WindowID != b.WindowID ||
				!received.Before(deadlines[id])) {
				if err := closeGrant(g); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil {
		pruneDeadlines(deadlines, applied)
		s.deadlines, s.windows = deadlines, windows
		if staleSnapshot {
			return invalid("authority snapshot predates the installed snapshot")
		}
	}
	return err
}

func validateBudgets(r policy.AuthorityResponse) (map[string]policy.AuthorityBudget, error) {
	result := make(map[string]policy.AuthorityBudget, len(r.Budgets))
	windows := map[string]bool{}
	for _, b := range r.Budgets {
		_, duplicate := result[b.Key]
		if duplicate || windows[b.WindowID] || !validString(b.Key) || !validString(b.Policy) ||
			!validString(b.Rule) || !validString(b.Revision) || !validString(b.WindowID) ||
			(b.Team == "" && b.User == "") || !optionalString(b.Team) || !optionalString(b.User) ||
			b.LimitMicroUSD < 0 || b.GrantMicroUSD < 0 || b.LeaseSeconds <= 0 ||
			int64(b.LeaseSeconds) > math.MaxInt64/int64(time.Second) ||
			(b.HardCap && b.GrantMicroUSD == 0) || !utcTime(b.WindowStart) || !utcTime(b.WindowEnd) ||
			!b.WindowStart.Before(b.WindowEnd) || r.ServerTime.Before(b.WindowStart) || !r.ServerTime.Before(b.WindowEnd) {
			return nil, invalid("invalid budget definition")
		}
		result[b.Key], windows[b.WindowID] = b, true
	}
	return result, nil
}

func validateGrant(g policy.AuthorityGrant, r *grantRequest, serverTime time.Time) error {
	if r == nil || !r.Sent || !r.Budget.HardCap || !validString(g.ID) || g.RequestID != r.Wire.RequestID ||
		g.Key != r.Wire.Key || g.Revision != r.Wire.Revision || g.WindowID != r.Wire.WindowID ||
		g.Amount < r.Wire.WantMicroUSD || g.Amount <= 0 || g.Amount > max(r.Budget.GrantMicroUSD, r.Wire.WantMicroUSD) ||
		g.Amount > r.Budget.LimitMicroUSD || !utcTime(g.ExpiresAt) || !g.ExpiresAt.After(r.Budget.WindowStart) ||
		g.ExpiresAt.After(serverTime.Add(time.Duration(r.Budget.LeaseSeconds)*time.Second)) {
		return invalid("invalid or unrequested grant")
	}
	return nil
}

func applyAcks(j *journal, r policy.AuthorityResponse) error {
	seen := map[string]bool{}
	for _, ack := range r.ReportAcks {
		g := j.Grants[ack.ID]
		if g == nil {
			receipt, ok := j.Receipts[reportReceiptID(ack.ID)]
			if !ok || seen[ack.ID] || ack.Sequence <= 0 || ack.Sequence > receipt.Sequence ||
				(ack.Instance != "" && ack.Instance != receipt.Instance) {
				return invalid("invalid finalized report acknowledgement")
			}
			seen[ack.ID] = true
			continue
		}
		if seen[ack.ID] || g == nil || (ack.Instance != "" && ack.Instance != g.Instance) ||
			ack.Sequence <= 0 || ack.Sequence > g.Sent {
			return invalid("invalid report acknowledgement")
		}
		seen[ack.ID] = true
		g.Ack = max(g.Ack, ack.Sequence)
	}
	seen = map[string]bool{}
	for _, ack := range r.MeterAcks {
		if ack.Instance == "" || ack.ID == "" || ack.Sequence <= 0 {
			return invalid("meter acknowledgement requires original instance and window")
		}
		receiptID := meterReceiptID(ack.Instance, ack.ID)
		if receipt, ok := j.Receipts[receiptID]; ok {
			if seen[receiptID] || ack.Sequence > receipt.Sequence {
				return invalid("invalid finalized meter acknowledgement")
			}
			seen[receiptID] = true
			continue
		}
		var found *meter
		var id string
		for mID, m := range j.Meters {
			if m.Wire.Instance == ack.Instance && m.Wire.WindowID == ack.ID {
				if found != nil {
					return invalid("ambiguous meter acknowledgement")
				}
				found, id = m, mID
			}
		}
		if found == nil || seen[id] || ack.Sequence > found.Sent {
			return invalid("invalid meter acknowledgement")
		}
		seen[id] = true
		found.Ack = max(found.Ack, ack.Sequence)
	}
	return nil
}

func deadline(now, serverTime, expiry time.Time, rtt time.Duration) time.Time {
	remaining := expiry.Sub(serverTime)
	if remaining <= 0 || rtt >= remaining {
		return now
	}
	return now.Add(remaining - rtt)
}

func cloneTimes(in map[string]time.Time) map[string]time.Time {
	out := make(map[string]time.Time, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func equalGrant(a, b policy.AuthorityGrant) bool {
	return a.ID == b.ID && a.RequestID == b.RequestID && a.Key == b.Key &&
		a.Revision == b.Revision && a.WindowID == b.WindowID && a.Amount == b.Amount && a.ExpiresAt.Equal(b.ExpiresAt)
}

func equalBudgets(a, b map[string]policy.AuthorityBudget) bool {
	if len(a) != len(b) {
		return false
	}
	for key, x := range a {
		y, ok := b[key]
		if !ok || x.Key != y.Key || x.Policy != y.Policy || x.Rule != y.Rule ||
			x.Team != y.Team || x.User != y.User || x.Revision != y.Revision ||
			x.WindowID != y.WindowID || !x.WindowStart.Equal(y.WindowStart) || !x.WindowEnd.Equal(y.WindowEnd) ||
			x.LimitMicroUSD != y.LimitMicroUSD || x.GrantMicroUSD != y.GrantMicroUSD ||
			x.LeaseSeconds != y.LeaseSeconds || x.HardCap != y.HardCap {
			return false
		}
	}
	return true
}

func utcTime(t time.Time) bool {
	_, offset := t.Zone()
	return !t.IsZero() && offset == 0
}

func validString(s string) bool {
	return s != "" && len(s) <= 4096 && !strings.ContainsAny(s, "\x00\r\n")
}

func optionalString(s string) bool { return s == "" || validString(s) }
