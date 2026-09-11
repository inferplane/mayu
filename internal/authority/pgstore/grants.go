package pgstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/inferplane/inferplane/internal/policy"
)

type grantState struct {
	key, window, revision, id, denial     string
	want, amount, seq, consumed, observed int64
	expires                               *time.Time
	closed, overrun                       bool
}

const grantColumns = `budget_key,window_id,revision,grant_id,denial,want,amount,sequence,consumed,observed,expires_at,closed,overrun`

func scanGrant(row pgx.Row) (grantState, error) {
	var g grantState
	err := row.Scan(&g.key, &g.window, &g.revision, &g.id, &g.denial, &g.want, &g.amount,
		&g.seq, &g.consumed, &g.observed, &g.expires, &g.closed, &g.overrun)
	return g, err
}

func capabilityHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func reportGrant(ctx context.Context, tx pgx.Tx, owner string, r policy.AuthorityReport) (grantState, error) {
	g, err := scanGrant(tx.QueryRow(ctx, `SELECT `+grantColumns+` FROM authority_requests
		WHERE grant_id=$1 AND owner=$2 AND instance=$3 AND request_hash=$4 AND amount>0`,
		r.GrantID, owner, r.Instance, capabilityHash(r.RequestID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return g, errors.New("authority postgres: unknown grant or invalid report capability")
	}
	return g, databaseError("read reported grant", err)
}

func applyReport(ctx context.Context, tx pgx.Tx, accounts map[accountKey]*account, owner, currentInstance string, r policy.AuthorityReport) error {
	// The dataplane and corresponding account rows are already locked. Every
	// writer of this grant holds those same locks; the identity never changes.
	g, err := reportGrant(ctx, tx, owner, r)
	if err != nil {
		return err
	}
	// A restored journal may predate a closure that Postgres already accepted.
	// Startup burns its old grant in full, but that synthetic burn is not new
	// spend and cannot undo a proven refund. Authenticate the original grant
	// first, then acknowledge only a terminal old-boot copy or full burn.
	// New observation may refine already-consumed uncertainty, never exceed
	// settled consumption. Explicit overruns take the accounting path below.
	if g.closed && r.Closed && r.Instance != currentInstance && !r.Overrun &&
		(r.Consumed == g.amount || r.Consumed == g.consumed) &&
		r.Observed <= max(g.consumed, g.observed) {
		if _, err := tx.Exec(ctx, `UPDATE authority_requests
			SET sequence=GREATEST(sequence,$2),observed=GREATEST(observed,$3)
			WHERE grant_id=$1`, g.id, r.Sequence, r.Observed); err != nil {
			return databaseError("acknowledge closed grant recovery", err)
		}
		// The caller acknowledges the incoming sequence, not the server's
		// possibly newer checkpoint, so the old outbox can retire this copy.
		return nil
	}
	// A restored OPEN copy can also lag a newer OPEN server checkpoint.
	// Burning its full amount cannot refund any encumbered authority. Keep
	// the server's higher sequence/observation and close through the normal
	// accounting path, even at an equal (or maximum) sequence. The caller
	// still acknowledges the original incoming sequence, not this maximum.
	recoveryBurn := !g.closed && r.Closed && r.Instance != currentInstance &&
		!r.Overrun && r.Consumed == g.amount
	if recoveryBurn {
		r.Sequence = max(r.Sequence, g.seq)
		r.Observed = max(r.Observed, g.observed)
	}
	same := r.Consumed == g.consumed && r.Observed == g.observed && r.Closed == g.closed && r.Overrun == g.overrun
	if (!recoveryBurn && (r.Sequence < g.seq || (r.Sequence == g.seq && !same))) ||
		r.Consumed < g.consumed || r.Observed < g.observed ||
		(g.closed && !same && !(r.Closed && r.Overrun)) {
		return errors.New("authority postgres: nonmonotonic or conflicting grant report")
	}
	if !r.Overrun && r.Consumed > g.amount {
		return errors.New("authority postgres: report exceeds grant without a closed overrun")
	}
	if r.Sequence == g.seq && !recoveryBurn {
		return nil
	}
	a := accounts[accountKey{g.key, g.window}]
	newConsumed := max(r.Consumed, r.Observed)
	oldConsumed := max(g.consumed, g.observed)
	if a.hardConsumed, err = checkedAdd(a.hardConsumed, newConsumed-oldConsumed); err != nil {
		return err
	}
	if r.Closed {
		// An open grant contributes its full amount. A closed grant already
		// contributes only settled consumption (or at least its full amount
		// after an overrun). A late overrun adds only the difference from that
		// previous contribution, including any previously proven refund.
		previous := g.amount
		if g.closed {
			previous = oldConsumed
			if g.overrun {
				previous = max(previous, g.amount)
			}
		}
		retained := newConsumed
		if r.Overrun {
			retained = max(retained, g.amount)
			a.frozen = true
		}
		if retained < previous {
			if g.closed {
				return errors.New("authority postgres: closed grant cannot refund twice")
			}
			refund := previous - retained
			if refund > a.hardEncumbered {
				return errors.New("authority postgres: inconsistent refund accounting")
			}
			a.hardEncumbered -= refund
		} else if a.hardEncumbered, err = checkedAdd(a.hardEncumbered, retained-previous); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE authority_requests SET sequence=$2,consumed=$3,observed=$4,closed=$5,overrun=$6 WHERE grant_id=$1`,
		g.id, r.Sequence, r.Consumed, r.Observed, r.Closed, r.Overrun); err != nil {
		return databaseError("persist grant report", err)
	}
	return nil
}

func applyMeter(ctx context.Context, tx pgx.Tx, accounts map[accountKey]*account, owner, currentInstance string, m policy.AuthorityMeter) error {
	a := accounts[accountKey{m.Key, m.WindowID}]
	if a == nil || !a.acceptsMeters {
		return errors.New("authority postgres: meter has no soft budget window")
	}
	var seq, consumed, observed int64
	err := tx.QueryRow(ctx, `SELECT sequence,consumed,observed FROM authority_meters
		WHERE owner=$1 AND instance=$2 AND budget_key=$3 AND window_id=$4`,
		owner, m.Instance, m.Key, m.WindowID).Scan(&seq, &consumed, &observed)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return databaseError("read meter", err)
	}
	if m.Instance != currentInstance {
		// A restored old-boot outbox can predate a checkpoint already accepted
		// by another replica. Reconcile each dimension independently; only
		// newly retained consumption may add to the account, never subtract.
		// Sync acknowledges the incoming checkpoint so this copy can retire.
		m.Sequence = max(m.Sequence, seq)
		m.Consumed = max(m.Consumed, consumed)
		m.Observed = max(m.Observed, observed)
	} else if m.Sequence < seq || m.Consumed < consumed || m.Observed < observed ||
		(m.Sequence == seq && (m.Consumed != consumed || m.Observed != observed)) {
		return errors.New("authority postgres: nonmonotonic or conflicting meter report")
	}
	if m.Sequence == seq && m.Consumed == consumed && m.Observed == observed {
		return nil
	}
	if a.softConsumed, err = checkedAdd(a.softConsumed, m.Consumed-consumed); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO authority_meters(owner,instance,budget_key,window_id,sequence,consumed,observed)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(owner,instance,budget_key,window_id)
		DO UPDATE SET sequence=excluded.sequence,consumed=excluded.consumed,observed=excluded.observed`,
		owner, m.Instance, m.Key, m.WindowID, m.Sequence, m.Consumed, m.Observed); err != nil {
		return databaseError("persist meter", err)
	}
	return nil
}

func issueGrant(ctx context.Context, tx pgx.Tx, accounts map[accountKey]*account, current map[string]policy.AuthorityBudget,
	now time.Time, owner, instance string, r policy.AuthorityGrantRequest, resp *policy.AuthorityResponse) error {
	hash := capabilityHash(r.RequestID)
	g, err := scanGrant(tx.QueryRow(ctx, `SELECT `+grantColumns+` FROM authority_requests WHERE owner=$1 AND instance=$2 AND request_hash=$3`,
		owner, instance, hash))
	existing := err == nil
	if err == nil {
		if g.key != r.Key || g.window != r.WindowID || g.revision != r.Revision || g.want != r.WantMicroUSD {
			return errors.New("authority postgres: conflicting grant request replay")
		}
		if g.amount > 0 {
			appendGrantResult(resp, r, g)
			return nil
		}
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return databaseError("read grant request", err)
	}
	// A denial binds the intent but reserves no authority. Reevaluate it so
	// an unchanged pending request can recover after a proven refund. Once
	// issued, the early-return branch above makes the grant immutable.
	g = grantState{key: r.Key, window: r.WindowID, revision: r.Revision, want: r.WantMicroUSD, id: g.id}
	b, ok := current[r.Key]
	switch {
	case !ok:
		g.denial = "unknown_budget"
	case b.Revision != r.Revision || b.WindowID != r.WindowID:
		g.denial = "stale_budget"
	case !b.HardCap:
		g.denial = "soft_budget"
	default:
		a := accounts[accountKey{b.Key, b.WindowID}]
		encumbered, err := checkedAdd(a.hardEncumbered, a.softConsumed)
		if err != nil {
			return err
		}
		switch {
		case a.frozen:
			g.denial = "frozen_window"
		case encumbered >= b.LimitMicroUSD || b.LimitMicroUSD-encumbered < r.WantMicroUSD:
			g.denial = "exhausted"
		default:
			g.amount = min(max(b.GrantMicroUSD, r.WantMicroUSD), b.LimitMicroUSD-encumbered)
			if g.amount <= 0 {
				return errors.New("authority postgres: invalid configured grant amount")
			}
			// Clamp before converting seconds to time.Duration: the window
			// always ends within a calendar month, even for huge lease input.
			expires := b.WindowEnd
			if int64(b.LeaseSeconds) <= int64(b.WindowEnd.Sub(now)/time.Second) {
				expires = now.Add(time.Duration(b.LeaseSeconds) * time.Second)
			}
			g.expires = &expires
			if a.hardEncumbered, err = checkedAdd(a.hardEncumbered, g.amount); err != nil {
				return err
			}
		}
	}
	if existing {
		if _, err := tx.Exec(ctx, `UPDATE authority_requests SET amount=$4,expires_at=$5,denial=$6
			WHERE owner=$1 AND instance=$2 AND request_hash=$3`,
			owner, instance, hash, g.amount, g.expires, g.denial); err != nil {
			return databaseError("retry denied grant request", err)
		}
	} else {
		var id [24]byte
		if _, err := rand.Read(id[:]); err != nil {
			return errors.New("authority postgres: generate grant identity")
		}
		g.id = hex.EncodeToString(id[:])
		if _, err := tx.Exec(ctx, `INSERT INTO authority_requests
			(owner,instance,request_hash,budget_key,window_id,revision,want,grant_id,amount,expires_at,denial)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			owner, instance, hash, g.key, g.window, g.revision, g.want, g.id, g.amount, g.expires, g.denial); err != nil {
			return databaseError("persist grant request", err)
		}
	}
	appendGrantResult(resp, r, g)
	return nil
}

func appendGrantResult(resp *policy.AuthorityResponse, r policy.AuthorityGrantRequest, g grantState) {
	if g.denial != "" || g.closed {
		reason := g.denial
		if g.closed {
			reason = "closed_grant"
		}
		resp.Denied = append(resp.Denied, policy.AuthorityDenial{RequestID: r.RequestID, Key: r.Key, Reason: reason})
		return
	}
	resp.Grants = append(resp.Grants, policy.AuthorityGrant{
		ID: g.id, RequestID: r.RequestID, Key: g.key, Revision: g.revision, WindowID: g.window,
		Amount: g.amount, ExpiresAt: g.expires.UTC(),
	})
}
