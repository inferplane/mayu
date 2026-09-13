package pgstore

import (
	"context"
	"errors"

	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/jackc/pgx/v5"
)

type sharedBooking struct {
	id             accountKey
	kind, moneyKey string
	amount         int64
	hard           bool
}

// FinishShared accepts precisely one terminal outcome. Nil observations and
// partial results retain their original bounds; expiry never releases money.
func (s *Store) FinishShared(ctx context.Context, p *governance.SharedPermit, usage governance.SharedSettlement) error {
	return s.sharedTerminal(ctx, p, "finish", usage)
}

// CancelShared is an explicit pre-dispatch cancellation. Its capability comes
// only from ReserveShared; it does not infer that a lost/crashed call was idle.
func (s *Store) CancelShared(ctx context.Context, p *governance.SharedPermit) error {
	return s.sharedTerminal(ctx, p, "cancel", governance.SharedSettlement{})
}

func (s *Store) sharedTerminal(ctx context.Context, p *governance.SharedPermit, terminal string, usage governance.SharedSettlement) error {
	ctx, cancel := context.WithTimeout(ctx, sharedTimeout)
	defer cancel()
	if p == nil || !validCapability(p.ID) || p.TokenBound < 0 || p.CostBoundMicroUSD < 0 {
		return governance.ErrSharedUnavailable
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return sharedError(err)
	}
	defer rollback(tx)
	hash := capabilityHash(p.ID)
	var tokens, cost int64
	var bookingCount int
	var oldTerminal, oldFingerprint string
	var invalid bool
	err = tx.QueryRow(ctx, `SELECT token_bound,cost_bound,booking_count,terminal,fingerprint,invalid FROM shared_permits
		WHERE permit_hash=$1 FOR UPDATE`, hash).Scan(&tokens, &cost, &bookingCount, &oldTerminal, &oldFingerprint, &invalid)
	if err != nil {
		return sharedError(err)
	}
	if tokens != p.TokenBound || cost != p.CostBoundMicroUSD {
		return governance.ErrSharedUnavailable
	}
	fingerprint := sharedHash(struct {
		Terminal string
		Usage    governance.SharedSettlement
	}{terminal, usage})
	if oldTerminal != "" {
		if oldTerminal != terminal || oldFingerprint != fingerprint || invalid {
			return governance.ErrSharedUnavailable
		}
		return nil
	}
	bookings, err := readSharedBookings(ctx, tx, hash)
	if err != nil {
		return sharedError(err)
	}
	if len(bookings) != bookingCount {
		return governance.ErrSharedUnavailable
	}
	// Original account/window identities survive policy deletion, team moves,
	// midnight/month boundaries and restarts. No current-policy lookup here.
	var refs policy.AuthorityRequest
	seen := map[accountKey]bool{}
	var defs []sharedDefinition
	for _, b := range bookings {
		defs = append(defs, sharedDefinition{id: b.id})
		if b.moneyKey != "" {
			id := accountKey{b.moneyKey, b.id.window}
			if !seen[id] {
				seen[id] = true
				refs.Meters = append(refs.Meters, policy.AuthorityMeter{Key: id.key, WindowID: id.window})
			}
		}
	}
	accounts, err := lockAccounts(ctx, tx, "", refs, nil)
	if err != nil {
		return sharedError(err)
	}
	now, err := databaseTime(ctx, tx)
	if err != nil {
		return sharedError(err)
	}
	counters, err := lockSharedCounters(ctx, tx, defs, now, false)
	if err != nil {
		return sharedError(err)
	}
	invalid = usage.InvalidUsage || sharedInvalidObservation(usage.Tokens, tokens) || sharedInvalidObservation(usage.CostMicroUSD, cost)
	for _, b := range bookings {
		c := counters[b.id]
		if b.moneyKey != "" && !b.hard {
			// Terminal fingerprint replay is checked before this loop. Clear
			// the original pending amount exactly once, including unknown or
			// invalid terminals whose full bound remains consumed.
			a := accounts[accountKey{b.moneyKey, b.id.window}]
			if a.softPending < b.amount {
				return governance.ErrSharedUnavailable
			}
			a.softPending -= b.amount
		}
		// Only admission/usage knows the fresh configured rate. Settlement
		// changes the captured debt without refilling at an obsolete rate;
		// the next admission applies all elapsed time at its current limit.
		if invalid {
			c.frozen = true
			if b.moneyKey != "" {
				accounts[accountKey{b.moneyKey, b.id.window}].frozen = true
			}
			continue // no refund from any part of an invalid provider outcome
		}
		retained, known := sharedRetained(b, terminal, usage)
		if err := settleSharedBooking(ctx, tx, hash, b, c, accounts, retained, known); err != nil {
			return sharedError(err)
		}
	}
	if err := persistAccounts(ctx, tx, accounts); err != nil {
		return sharedError(err)
	}
	if err := persistSharedCounters(ctx, tx, counters); err != nil {
		return sharedError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE shared_permits SET terminal=$2,fingerprint=$3,invalid=$4 WHERE permit_hash=$1`,
		hash, terminal, fingerprint, invalid); err != nil {
		return sharedError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return sharedError(err)
	}
	if invalid {
		return governance.ErrSharedUnavailable
	}
	return nil
}

func sharedInvalidObservation(observation *int64, bound int64) bool {
	return observation != nil && (*observation < 0 || *observation > bound)
}

func readSharedBookings(ctx context.Context, tx pgx.Tx, hash string) ([]sharedBooking, error) {
	rows, err := tx.Query(ctx, `SELECT counter_key,window_id,kind,amount,money_key,hard
		FROM shared_bookings WHERE permit_hash=$1 ORDER BY counter_key,window_id`, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sharedBooking
	for rows.Next() {
		var b sharedBooking
		if err := rows.Scan(&b.id.key, &b.id.window, &b.kind, &b.amount, &b.moneyKey, &b.hard); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func sharedRetained(b sharedBooking, terminal string, u governance.SharedSettlement) (int64, bool) {
	if terminal == "cancel" {
		return 0, true
	}
	if b.kind == "rpm" {
		return b.amount, true
	} // dispatched attempt consumes one request
	if !u.Complete {
		return b.amount, false
	}
	value := u.Tokens
	if b.kind == "microUSD" {
		value = u.CostMicroUSD
	}
	if value == nil {
		return b.amount, false
	}
	return *value, true
}

func settleSharedBooking(ctx context.Context, tx pgx.Tx, hash string, b sharedBooking, c *sharedCounter, accounts map[accountKey]*account, retained int64, known bool) error {
	if retained < 0 || retained > b.amount || c.reserved < b.amount || c.used < b.amount {
		return errors.New("inconsistent shared accounting")
	}
	refund := b.amount - retained
	if b.kind == "rpm" || b.kind == "tpm" {
		if err := returnSharedRate(ctx, tx, hash, b, c, refund, known); err != nil {
			return err
		}
		// Rate used/reserved counts open permits only. Segment debt retains
		// both known consumption and unknown terminal outcomes until refill.
		c.used -= b.amount
		c.reserved -= b.amount
	} else {
		c.used -= refund
		if known {
			c.reserved -= b.amount
		}
	}
	if b.moneyKey != "" {
		a := accounts[accountKey{b.moneyKey, b.id.window}]
		if b.hard {
			if a.hardEncumbered < refund {
				return errors.New("inconsistent monetary refund")
			}
			a.hardEncumbered -= refund
			var err error
			if a.hardConsumed, err = checkedAdd(a.hardConsumed, retained); err != nil {
				return err
			}
		} else {
			if a.softConsumed < refund {
				return errors.New("inconsistent monetary refund")
			}
			a.softConsumed -= refund
		}
	}
	return nil
}

// SharedUsage reauthenticates the subject and derives only its current scopes.
// It never enumerates counters by an untrusted team/user selector.
func (s *Store) SharedUsage(ctx context.Context, subject governance.Subject) ([]governance.SharedLimit, error) {
	ctx, cancel := context.WithTimeout(ctx, sharedTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, sharedError(err)
	}
	defer rollback(tx)
	p, now, err := sharedSnapshot(ctx, tx, subject)
	if err != nil {
		return nil, err
	}
	defs, budgets, err := sharedDefinitions(ctx, tx, p, now, nil)
	if err != nil {
		return nil, err
	}
	accounts, err := lockAccounts(ctx, tx, "", policy.AuthorityRequest{}, budgets)
	if err != nil {
		return nil, sharedError(err)
	}
	counters, err := lockSharedCounters(ctx, tx, defs, now, true)
	if err != nil {
		return nil, sharedError(err)
	}
	now, err = databaseTime(ctx, tx)
	if err != nil {
		return nil, sharedError(err)
	}
	if err := sharedCheckClock(p, defs, now); err != nil {
		return nil, err
	}
	out := make([]governance.SharedLimit, 0, len(defs))
	for _, d := range defs {
		c := counters[d.id]
		if err := refillSharedRate(ctx, tx, d.id, c, now, d.rate()); err != nil {
			return nil, sharedError(err)
		}
		used, reserved := c.used, c.reserved
		if d.rate() > 0 {
			used, err = sharedCeil(&c.debt)
			if err != nil {
				return nil, sharedError(err)
			}
			reserved, err = sharedRateReserved(ctx, tx, d.id)
			if err != nil {
				return nil, sharedError(err)
			}
		}
		if d.moneyKey != "" {
			a := accounts[accountKey{d.moneyKey, d.id.window}]
			used, err = checkedAdd(a.hardEncumbered, a.softConsumed)
			if err != nil {
				return nil, sharedError(err)
			}
			// ADR-045 open-grant liability is also reserved authority. Closed
			// uncertainty is consumption there; shared uncertainty remains
			// explicitly reserved in the shared ledger.
			var localReserved int64
			err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(GREATEST(amount-consumed,0)),0)::bigint
				FROM authority_requests WHERE budget_key=$1 AND window_id=$2 AND NOT closed`,
				d.moneyKey, d.id.window).Scan(&localReserved)
			if err != nil {
				return nil, sharedError(err)
			}
			reserved, err = checkedAdd(reserved, localReserved)
			if err != nil {
				return nil, sharedError(err)
			}
		}
		out = append(out, governance.SharedLimit{Scope: d.scope, Kind: d.kind, Policy: d.policy, Rule: d.rule,
			Window: d.window, Limit: d.limit, Used: used, Reserved: reserved, Remaining: max(0, d.limit-used), ResetsAt: d.end, Hard: d.hard})
	}
	// Read-only accounting: rollback also discards rows ensured for previously
	// unused scopes. A usage request can never replenish or spend authority.
	return out, nil
}
