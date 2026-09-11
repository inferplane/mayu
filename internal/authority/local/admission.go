package local

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/policy"
)

// Reserve durably reserves the entire bound from one grant per matching hard
// budget, atomically across all scopes. Missing credit only queues a background
// request; admission never waits on, or makes, a network call.
func (s *Store) Reserve(ctx context.Context, subject governance.Subject, bound int64) (*governance.BudgetPermit, error) {
	if bound < 0 || subject.Team == "" {
		return nil, invalid("invalid admission subject or bound")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var result *governance.BudgetPermit
	var denied error
	var expires time.Time
	err := s.transaction(ctx, func(j *journal) error {
		if j.Poisoned {
			return invalid("admission poisoned by an observed bound overrun")
		}
		if !j.Ready {
			return fmt.Errorf("%w: initial sync required", governance.ErrAuthorityUnavailable)
		}
		now := s.now()
		if err := s.closeInactive(j, now); err != nil {
			return err
		}
		var chosen []string
		var soft []policy.AuthorityBudget
		for _, key := range sortedKeys(j.Budgets) {
			b := j.Budgets[key]
			if !matches(b, subject) {
				continue
			}
			expires = earlier(expires, s.windows[key])
			if !now.Before(s.windows[key]) {
				denied = governance.ErrAuthorityUnavailable
				s.signal()
				continue
			}
			if !b.HardCap {
				soft = append(soft, b)
				continue
			}
			id := ""
			for _, grantID := range sortedKeys(j.Grants) {
				g := j.Grants[grantID]
				if !s.usable(g, b, now) {
					continue
				}
				available := g.Wire.Amount - g.Consumed - g.Reserved
				if available > 0 && available >= bound {
					id = grantID
					break
				}
				// Only a settled grant proves that its unused tail is safe to
				// return. A reservation of zero still counts as pending work.
				if g.Pending == 0 {
					if err := closeGrant(g); err != nil {
						return err
					}
				}
			}
			if id != "" {
				chosen = append(chosen, id)
				expires = earlier(expires, s.deadlines[id])
				continue
			}
			exhausted, err := s.queue(j, b, bound)
			if err != nil {
				return err
			}
			if exhausted {
				denied = governance.ErrAuthorityExhausted
			} else if denied == nil {
				denied = governance.ErrAuthorityUnavailable
			}
			s.signal()
		}
		if denied != nil {
			// Commit queued wants and proven idle closes, but reserve nothing.
			return nil
		}
		id, err := nonce()
		if err != nil {
			return err
		}
		p := &permit{Instance: s.instance, Bound: bound, Grants: chosen}
		for _, grantID := range chosen {
			g := j.Grants[grantID]
			g.Reserved, err = add(g.Reserved, bound)
			if err != nil {
				return err
			}
			if err := bump(&g.Pending); err != nil {
				return err
			}
		}
		for _, b := range soft {
			mID := meterID(s.instance, b.Key, b.WindowID)
			if j.Meters[mID] == nil {
				j.Meters[mID] = &meter{WindowEnd: b.WindowEnd, Wire: policy.AuthorityMeter{
					Instance: s.instance, Key: b.Key, WindowID: b.WindowID, Sequence: 1}}
			}
			p.Meters = append(p.Meters, mID)
		}
		j.Permits[id] = p
		result = &governance.BudgetPermit{ID: id, BoundMicroUSD: bound}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if denied != nil {
		return nil, denied
	}
	if !expires.IsZero() && !s.now().Before(expires) {
		// No caller has seen this permit, so this reservation provably did not
		// dispatch. Record that fact before releasing it. A failed cleanup
		// retains the bound durably and still cannot return a permit.
		err := s.transaction(ctx, func(j *journal) error {
			p := j.Permits[result.ID]
			if p == nil || p.Done {
				return invalid("missing undispatched permit")
			}
			for _, id := range p.Grants {
				g := j.Grants[id]
				if g.Pending <= 0 || g.Reserved < p.Bound || g.Closed {
					return invalid("inconsistent undispatched reservation")
				}
				g.Pending--
				g.Reserved -= p.Bound
			}
			zero := int64(0)
			p.Done, p.Complete, p.Actual = true, true, &zero
			return s.closeInactive(j, s.now())
		})
		s.signal()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: authority expired while recording reservation", governance.ErrAuthorityUnavailable)
	}
	return result, nil
}

func earlier(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

// Finish stores one immutable outcome. A complete, known cost releases only the
// bound's proven unused portion. Incomplete outcomes retain the entire bound,
// with independently observed cost preserved for reporting. The latest 256
// completed outcomes support idempotent retries; older forgotten permits fail
// closed without changing any debit or refund.
func (s *Store) Finish(ctx context.Context, authorization *governance.BudgetPermit, actual *int64, complete bool) error {
	if authorization == nil || authorization.ID == "" || authorization.BoundMicroUSD < 0 || (actual != nil && *actual < 0) {
		return invalid("invalid settlement")
	}
	if authorization.InvalidUsage {
		// Invalid token bounds/totals cannot prove unused authority even when
		// transport completed. Preserve independently known dollars, if any.
		complete = false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var settlementErr error
	err := s.transaction(ctx, func(j *journal) error {
		p := j.Permits[authorization.ID]
		if p == nil || p.Bound != authorization.BoundMicroUSD || p.Retired {
			return invalid("unknown or retired permit")
		}
		if p.Done {
			if p.Complete != complete || p.InvalidUsage != authorization.InvalidUsage || !equalActual(p.Actual, actual) {
				return invalid("conflicting settlement")
			}
			return nil
		}
		if p.Instance != s.instance {
			return invalid("permit belongs to another boot")
		}
		consumed, observed := p.Bound, int64(0)
		if actual != nil {
			observed = *actual
			if complete || observed > consumed {
				consumed = observed
			}
		}
		overrun := authorization.InvalidUsage || observed > p.Bound
		// Check the whole settlement before changing any cumulative value. If
		// an observed overrun cannot fit in int64, retain its exact per-permit
		// observation and durably poison admission; never roll the poison back
		// with the failed aggregate update.
		check := func(consumedBefore, observedBefore, sequence int64) error {
			if _, err := add(consumedBefore, consumed); err != nil {
				return err
			}
			if _, err := add(observedBefore, observed); err != nil {
				return err
			}
			_, err := add(sequence, 1)
			return err
		}
		poison := func(err error) {
			j.Poisoned, p.Retired = true, true
			p.InvalidUsage = authorization.InvalidUsage
			if p.InvalidUsage {
				for _, id := range p.Grants {
					j.Grants[id].Overrun = true
				}
			}
			if actual != nil {
				n := *actual
				p.Actual = &n
			}
			settlementErr = err
		}
		for _, id := range p.Grants {
			g := j.Grants[id]
			if g.Closed || g.Pending <= 0 || g.Reserved < p.Bound {
				return invalid("inconsistent reservation")
			}
			if err := check(g.Consumed, g.Observed, g.Sequence); err != nil {
				poison(err)
				return nil
			}
		}
		for _, id := range p.Meters {
			m := j.Meters[id]
			if err := check(m.Wire.Consumed, m.Wire.Observed, m.Wire.Sequence); err != nil {
				poison(err)
				return nil
			}
		}
		for _, id := range p.Grants {
			g := j.Grants[id]
			g.Consumed += consumed
			g.Observed += observed
			g.Reserved -= p.Bound
			g.Pending--
			g.Overrun = g.Overrun || overrun
			if g.Overrun && g.Pending == 0 {
				g.Closed = true
			}
			g.Sequence++
		}
		for _, id := range p.Meters {
			m := j.Meters[id]
			m.Wire.Consumed += consumed
			m.Wire.Observed += observed
			m.Wire.Sequence++
		}
		p.Done, p.Complete, p.InvalidUsage = true, complete, authorization.InvalidUsage
		if actual != nil {
			n := *actual
			p.Actual = &n
		}
		j.Poisoned = j.Poisoned || overrun
		return s.closeInactive(j, s.now())
	})
	if err == nil {
		s.signal()
		return settlementErr
	}
	return err
}

func matches(b policy.AuthorityBudget, subject governance.Subject) bool {
	return (b.Team == "" || b.Team == subject.Team) && (b.User == "" || b.User == subject.User)
}

func equalActual(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func (s *Store) usable(g *grant, b policy.AuthorityBudget, now time.Time) bool {
	return g.Instance == s.instance && !g.Closed && !g.Overrun &&
		g.Wire.Key == b.Key && g.Wire.Revision == b.Revision && g.Wire.WindowID == b.WindowID &&
		g.Consumed <= g.Wire.Amount && g.Reserved <= g.Wire.Amount-g.Consumed &&
		now.Before(s.deadlines[g.Wire.ID])
}

func (s *Store) closeInactive(j *journal, now time.Time) error {
	for _, g := range j.Grants {
		if g.Closed || g.Pending != 0 {
			continue
		}
		b, ok := j.Budgets[g.Wire.Key]
		if !ok || !b.HardCap || !s.usable(g, b, now) {
			if err := closeGrant(g); err != nil {
				return err
			}
		}
	}
	return nil
}

func closeGrant(g *grant) error {
	if g.Closed {
		return nil
	}
	if g.Pending != 0 || g.Reserved != 0 {
		return invalid("cannot close pending authority")
	}
	g.Closed = true
	return bump(&g.Sequence)
}

func (s *Store) queue(j *journal, b policy.AuthorityBudget, bound int64) (bool, error) {
	want := max(bound, int64(1))
	// Unsent requests can coalesce to the largest observed bound. Once sent,
	// a capability's parameters are immutable even if the reply was lost.
	for _, id := range sortedKeys(j.Requests) {
		r := j.Requests[id]
		if r.Instance != s.instance || r.Obsolete || r.GrantID != "" || !sameDefinition(r.Budget, b) {
			continue
		}
		if r.Wire.WantMicroUSD >= want {
			return r.Denied == "exhausted" || r.Denied == "frozen_window", nil
		}
		if !r.Sent {
			r.Wire.WantMicroUSD = want
			return false, nil
		}
	}
	id, err := nonce()
	if err != nil {
		return false, err
	}
	j.Requests[id] = &grantRequest{Instance: s.instance, Budget: b, Wire: policy.AuthorityGrantRequest{
		RequestID: id, Key: b.Key, Revision: b.Revision, WindowID: b.WindowID, WantMicroUSD: want}}
	return false, nil
}

// This private index is not sent on the wire. Sync acknowledgements are matched
// through the shared protocol's meter identity.
func meterID(instance, key, window string) string {
	b, _ := json.Marshal([3]string{instance, key, window})
	return string(b)
}
