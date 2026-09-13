package pgstore

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/jackc/pgx/v5"
)

var _ governance.SharedAuthority = (*Store)(nil)

const sharedTimeout = 5 * time.Second

// The shared profile has its own admission ledger. Policy MONEY alone is also
// booked in authority_accounts, under the same locks as ADR-045 Sync.
const sharedSchema = `
CREATE TABLE IF NOT EXISTS shared_counters (
  counter_key TEXT NOT NULL,
  window_id TEXT NOT NULL,
  used BIGINT NOT NULL DEFAULT 0 CHECK (used >= 0),
  reserved BIGINT NOT NULL DEFAULT 0 CHECK (reserved >= 0 AND reserved <= used),
  debt NUMERIC(60,0) NOT NULL DEFAULT 0 CHECK (debt >= 0),
  refilled NUMERIC(60,0) NOT NULL DEFAULT 0 CHECK (refilled >= 0),
  rate BIGINT NOT NULL DEFAULT 0 CHECK (rate >= 0),
  updated_at TIMESTAMPTZ NOT NULL,
  frozen BOOLEAN NOT NULL DEFAULT false,
  PRIMARY KEY (counter_key,window_id)
);
CREATE TABLE IF NOT EXISTS shared_permits (
  permit_hash TEXT PRIMARY KEY,
  token_bound BIGINT NOT NULL CHECK (token_bound >= 0),
  cost_bound BIGINT NOT NULL CHECK (cost_bound >= 0),
  booking_count INTEGER NOT NULL CHECK (booking_count >= 0),
  terminal TEXT NOT NULL DEFAULT '',
  fingerprint TEXT NOT NULL DEFAULT '',
  invalid BOOLEAN NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS shared_bookings (
  permit_hash TEXT NOT NULL REFERENCES shared_permits(permit_hash),
  counter_key TEXT NOT NULL,
  window_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('rpm','tpm','tokens','microUSD')),
  amount BIGINT NOT NULL CHECK (amount >= 0),
  money_key TEXT NOT NULL DEFAULT '',
  hard BOOLEAN NOT NULL,
  PRIMARY KEY (permit_hash,counter_key,window_id),
  FOREIGN KEY (counter_key,window_id) REFERENCES shared_counters(counter_key,window_id)
);
CREATE TABLE IF NOT EXISTS shared_rate_segments (
  segment_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  permit_hash TEXT NOT NULL,
  counter_key TEXT NOT NULL,
  window_id TEXT NOT NULL,
  remaining NUMERIC(60,0) NOT NULL CHECK (remaining >= 0),
  uncertain BOOLEAN NOT NULL DEFAULT true,
  UNIQUE (permit_hash,counter_key,window_id),
  FOREIGN KEY (permit_hash,counter_key,window_id)
    REFERENCES shared_bookings(permit_hash,counter_key,window_id)
);
CREATE INDEX IF NOT EXISTS shared_rate_segments_counter
  ON shared_rate_segments(counter_key,window_id,segment_id
);`

// InitializeShared requires the policy and key stores in this same schema. It
// never creates a second identity store or seeds per-request limit definitions.
func (s *Store) InitializeShared(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, sharedTimeout)
	defer cancel()
	if err := s.Initialize(ctx); err != nil {
		return sharedError(err)
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return sharedError(err)
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `LOCK TABLE policies,keys,teams IN SHARE MODE`); err != nil {
		return sharedError(err)
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(847007)`); err != nil {
		return sharedError(err)
	}
	if _, err = tx.Exec(ctx, sharedSchema); err != nil {
		return sharedError(err)
	}
	return sharedError(tx.Commit(ctx))
}

func sharedError(err error) error {
	if err == nil {
		return nil
	}
	// Do not wrap a driver/key/policy error: it may contain SQL parameters,
	// identity values, a document body, or the secret-bearing connection string.
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %w", governance.ErrSharedUnavailable, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", governance.ErrSharedUnavailable, context.DeadlineExceeded)
	default:
		return governance.ErrSharedUnavailable
	}
}

func sharedChanged() error {
	return &governance.SharedDenial{Status: 503, Reason: "shared authentication snapshot changed", RetryAfter: 1}
}

func sharedSubjectMatches(p keystore.Principal, subject governance.Subject) bool {
	return p.Team == subject.Team && p.KeyID == subject.KeyID && p.Owner == subject.User
}

// sharedSnapshot acquires definition locks BEFORE reading fresh READ COMMITTED
// snapshots. SHARE prevents key/team/policy updates and phantoms through commit.
// All shared operations order locks: definitions, authority accounts, counters.
func sharedSnapshot(ctx context.Context, tx pgx.Tx, subject governance.Subject) (keystore.Principal, time.Time, error) {
	if !validText(subject.KeyID) || !validText(subject.Team) ||
		(subject.User != "" && !validText(subject.User)) {
		return keystore.Principal{}, time.Time{}, sharedChanged()
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE policies,keys,teams IN SHARE MODE`); err != nil {
		return keystore.Principal{}, time.Time{}, sharedError(err)
	}
	now, err := databaseTime(ctx, tx)
	if err != nil {
		return keystore.Principal{}, now, sharedError(err)
	}
	p, err := keystore.ReadPostgresPrincipal(ctx, tx, subject.KeyID, now)
	if err != nil {
		return p, now, sharedError(err)
	}
	if !sharedSubjectMatches(p, subject) || !p.TeamSnapshotLoaded || p.TeamSnapshot == nil ||
		p.TeamSnapshot.Name != p.Team || p.SharedRevision == "" {
		return p, now, sharedChanged()
	}
	return p, now, nil
}

func (s *Store) ReserveShared(ctx context.Context, req governance.SharedRequest) (*governance.SharedPermit, error) {
	ctx, cancel := context.WithTimeout(ctx, sharedTimeout)
	defer cancel()
	if req.TokenBound < 0 || req.CostBoundMicroUSD < 0 || !validText(req.AuthRevision) ||
		!validText(req.Model) || !validText(req.RequestedModel) {
		return nil, governance.ErrSharedUnavailable
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, sharedError(err)
	}
	defer rollback(tx)
	p, now, err := sharedSnapshot(ctx, tx, req.Subject)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(p.SharedRevision), []byte(req.AuthRevision)) != 1 {
		return nil, sharedChanged()
	}
	defs, budgets, err := sharedDefinitions(ctx, tx, p, now, &req)
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
	var denial error
	for _, d := range defs {
		c := counters[d.id]
		if err := refillSharedRate(ctx, tx, d.id, c, now, d.rate()); err != nil {
			return nil, sharedError(err)
		}
		if c.frozen || (d.moneyKey != "" && accounts[accountKey{d.moneyKey, d.id.window}].frozen) {
			return nil, governance.ErrSharedUnavailable
		}
		amount := d.bound(req)
		if err := sharedCheckCapacity(d, c, accounts, amount); err != nil {
			// Evaluate all scopes first: a soft source never masks a hard one.
			if sharedIsDenial(err) {
				if denial == nil {
					denial = err
				}
			} else {
				return nil, err
			}
		}
	}
	if denial != nil {
		return nil, denial
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, sharedError(err)
	}
	permit := &governance.SharedPermit{ID: hex.EncodeToString(raw[:]), TokenBound: req.TokenBound, CostBoundMicroUSD: req.CostBoundMicroUSD}
	hash := capabilityHash(permit.ID)
	if _, err := tx.Exec(ctx, `INSERT INTO shared_permits(permit_hash,token_bound,cost_bound,booking_count) VALUES($1,$2,$3,$4)`,
		hash, permit.TokenBound, permit.CostBoundMicroUSD, len(defs)); err != nil {
		return nil, sharedError(err)
	}
	for _, d := range defs {
		c := counters[d.id]
		amount := d.bound(req)
		if err := sharedBook(d, c, accounts, amount); err != nil {
			return nil, sharedError(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO shared_bookings
			(permit_hash,counter_key,window_id,kind,amount,money_key,hard)
			VALUES($1,$2,$3,$4,$5,$6,$7)`,
			hash, d.id.key, d.id.window, d.kind, amount, d.moneyKey, d.hard); err != nil {
			return nil, sharedError(err)
		}
		if d.rate() > 0 && amount > 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO shared_rate_segments(permit_hash,counter_key,window_id,remaining)
				VALUES($1,$2,$3,$4::numeric)`, hash, d.id.key, d.id.window, sharedScaled(amount).String()); err != nil {
				return nil, sharedError(err)
			}
		}
	}
	if err := persistAccounts(ctx, tx, accounts); err != nil {
		return nil, sharedError(err)
	}
	if err := persistSharedCounters(ctx, tx, counters); err != nil {
		return nil, sharedError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		// An ambiguous commit is never automatically refunded or retried.
		return nil, sharedError(err)
	}
	return permit, nil
}

func sharedIsDenial(err error) bool {
	var d *governance.SharedDenial
	return errors.As(err, &d)
}

func sharedCheckClock(p keystore.Principal, defs []sharedDefinition, now time.Time) error {
	if p.ExpiresAt != nil && !now.Before(*p.ExpiresAt) {
		return sharedChanged()
	}
	for _, d := range defs {
		if !d.end.IsZero() && (now.Before(d.start) || !now.Before(d.end)) {
			return governance.ErrSharedUnavailable // caller retries a fresh window
		}
	}
	return nil
}
