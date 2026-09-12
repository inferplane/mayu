// Package pgstore owns durable budget escrow in the policy store's Postgres
// database. A successful Sync commits every reservation before returning it.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/policy"
)

// Separate from policy/analytics/body/telemetry migrations. Transaction-scoped
// advisory locks release on rollback, including cancellation and failed DDL.
const schemaLockKey int64 = 847005

const schema = `
CREATE TABLE IF NOT EXISTS authority_identity (
  singleton BOOLEAN PRIMARY KEY CHECK(singleton),
  namespace TEXT NOT NULL CHECK(length(namespace) = 64)
);
CREATE TABLE IF NOT EXISTS authority_dataplanes (
  owner TEXT PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS authority_accounts (
  budget_key TEXT NOT NULL,
  window_id TEXT NOT NULL,
  hard_encumbered BIGINT NOT NULL DEFAULT 0 CHECK (hard_encumbered >= 0),
  hard_consumed BIGINT NOT NULL DEFAULT 0 CHECK (hard_consumed >= 0),
  soft_consumed BIGINT NOT NULL DEFAULT 0 CHECK (soft_consumed >= 0),
  soft_pending BIGINT NOT NULL DEFAULT 0 CHECK (soft_pending >= 0 AND soft_pending <= soft_consumed),
  accepts_meters BOOLEAN NOT NULL DEFAULT false,
  frozen BOOLEAN NOT NULL DEFAULT false,
  PRIMARY KEY (budget_key, window_id)
);
ALTER TABLE authority_accounts ADD COLUMN IF NOT EXISTS soft_pending BIGINT NOT NULL DEFAULT 0
  CHECK (soft_pending >= 0 AND soft_pending <= soft_consumed);
CREATE TABLE IF NOT EXISTS authority_requests (
  owner TEXT NOT NULL,
  instance TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  budget_key TEXT NOT NULL,
  window_id TEXT NOT NULL,
  revision TEXT NOT NULL,
  want BIGINT NOT NULL CHECK (want >= 0),
  grant_id TEXT NOT NULL UNIQUE,
  amount BIGINT NOT NULL CHECK (amount >= 0),
  expires_at TIMESTAMPTZ,
  denial TEXT NOT NULL DEFAULT '',
  sequence BIGINT NOT NULL DEFAULT 0 CHECK (sequence >= 0),
  consumed BIGINT NOT NULL DEFAULT 0 CHECK (consumed >= 0),
  observed BIGINT NOT NULL DEFAULT 0 CHECK (observed >= 0),
  closed BOOLEAN NOT NULL DEFAULT false,
  overrun BOOLEAN NOT NULL DEFAULT false,
  PRIMARY KEY (owner, instance, request_hash),
  CHECK ((amount > 0 AND expires_at IS NOT NULL AND denial = '') OR
         (amount = 0 AND expires_at IS NULL AND denial <> ''))
);
CREATE TABLE IF NOT EXISTS authority_meters (
  owner TEXT NOT NULL,
  instance TEXT NOT NULL,
  budget_key TEXT NOT NULL,
  window_id TEXT NOT NULL,
  sequence BIGINT NOT NULL CHECK (sequence > 0),
  consumed BIGINT NOT NULL CHECK (consumed >= 0),
  observed BIGINT NOT NULL CHECK (observed >= 0 AND observed <= consumed),
  PRIMARY KEY (owner, instance, budget_key, window_id),
  FOREIGN KEY (budget_key, window_id) REFERENCES authority_accounts (budget_key, window_id)
);
CREATE TABLE IF NOT EXISTS authority_tiers (
  budget_key TEXT NOT NULL,
  window_id TEXT NOT NULL,
  policy_name TEXT NOT NULL,
  rule_name TEXT NOT NULL,
  threshold INTEGER NOT NULL CHECK (threshold BETWEEN 1 AND 100),
  PRIMARY KEY (budget_key, window_id, policy_name, rule_name),
  FOREIGN KEY (budget_key, window_id) REFERENCES authority_accounts (budget_key, window_id)
);`

// Store is safe for concurrent callers and independent control-plane replicas.
// No policy documents, grants, liabilities or tier latches live in process state.
type Store struct{ db *pgxpool.Pool }

// New constructs a lazy pool. Parsing and pool-construction errors deliberately
// omit the underlying error because it may include the secret-bearing DSN.
func New(dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("authority postgres: missing DSN")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("authority postgres: invalid DSN (connection string withheld)")
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "10000"
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	db, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, errors.New("authority postgres: cannot construct pool (connection string withheld)")
	}
	return &Store{db: db}, nil
}

// Initialize migrates only the authority schema. The existing policy store must
// be initialized first in the same database/search_path; missing policies fails.
func (s *Store) Initialize(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return databaseError("begin initialization", err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `LOCK TABLE policies IN SHARE MODE`); err != nil {
		return databaseError("lock policy store for initialization", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaLockKey); err != nil {
		return databaseError("lock schema migration", err)
	}
	if _, err := tx.Exec(ctx, schema); err != nil {
		return databaseError("initialize schema", err)
	}
	if err := initializeIdentity(ctx, tx); err != nil {
		return err
	}
	return databaseError("commit initialization", tx.Commit(ctx))
}

// Close releases the pool. It does not retire or refund any outstanding grant.
func (s *Store) Close() { s.db.Close() }

// Ready checks live database connectivity, including acquiring a pooled
// connection. It respects shorter caller deadlines and otherwise takes at most
// two seconds; successful initialization is not evidence of current readiness.
func (s *Store) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return databaseError("check readiness", s.db.Ping(ctx))
}

// Sync reads fresh policy and atomically applies cumulative reports, meters and
// fixed grant requests. Any malformed element rolls back the entire batch.
func (s *Store) Sync(ctx context.Context, dataplane string, req policy.AuthorityRequest) (policy.SyncResponse, error) {
	var empty policy.SyncResponse
	if err := validateRequest(dataplane, req); err != nil {
		return empty, err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return empty, databaseError("begin sync", err)
	}
	defer rollback(tx)
	// SHARE conflicts with policy writers' ROW EXCLUSIVE locks. READ COMMITTED
	// takes a fresh snapshot AFTER acquiring this lock, so a waiting old CP
	// cannot issue against policy that was replaced while it waited.
	if _, err := tx.Exec(ctx, `LOCK TABLE policies IN SHARE MODE`); err != nil {
		return empty, databaseError("lock policies", err)
	}
	docs, err := readPolicies(ctx, tx)
	if err != nil {
		return empty, err
	}
	// One mutex per authenticated dataplane also serializes conflicting
	// idempotency keys that name different budget accounts.
	if _, err := tx.Exec(ctx, `INSERT INTO authority_dataplanes(owner) VALUES($1) ON CONFLICT DO NOTHING`, dataplane); err != nil {
		return empty, databaseError("ensure dataplane", err)
	}
	var owner string
	if err := tx.QueryRow(ctx, `SELECT owner FROM authority_dataplanes WHERE owner=$1 FOR UPDATE`, dataplane).Scan(&owner); err != nil {
		return empty, databaseError("lock dataplane", err)
	}
	now, err := databaseTime(ctx, tx)
	if err != nil {
		return empty, err
	}
	budgets, err := policy.AuthorityBudgets(docs, now)
	if err != nil {
		return empty, fmt.Errorf("authority postgres: derive budgets: %w", err)
	}
	accounts, err := lockAccounts(ctx, tx, dataplane, req, budgets)
	if err != nil {
		return empty, err
	}
	// Account locks may have waited across a calendar boundary. Never issue
	// stale-window authority or use the transaction-start timestamp as "now".
	now, err = databaseTime(ctx, tx)
	if err != nil {
		return empty, err
	}
	for _, b := range budgets {
		if !now.Before(b.WindowEnd) || now.Before(b.WindowStart) {
			return empty, errors.New("authority postgres: window changed while locking; retry sync")
		}
	}
	generation := policy.GenerationOf(docs)
	authorityID, err := readIdentity(ctx, tx)
	if err != nil {
		return empty, err
	}
	resp := policy.SyncResponse{
		Policies: docs, Generation: generation,
		SyncIntervalSeconds: 10,
		Authority: &policy.AuthorityResponse{
			AuthorityID: authorityID,
			Protocol:    policy.AuthorityProtocol, ServerTime: now, Budgets: budgets,
			Generation: generation, Policies: docs,
		},
	}
	current := make(map[string]policy.AuthorityBudget, len(budgets))
	for _, b := range budgets {
		current[b.Key] = b
		if cadence := max(1, b.LeaseSeconds/3); cadence < resp.SyncIntervalSeconds {
			resp.SyncIntervalSeconds = cadence
		}
	}
	for _, report := range req.Reports {
		if err := applyReport(ctx, tx, accounts, dataplane, req.Instance, report); err != nil {
			return empty, err
		}
		resp.Authority.ReportAcks = append(resp.Authority.ReportAcks, policy.AuthorityAck{
			ID: report.GrantID, Sequence: report.Sequence, Instance: report.Instance,
		})
	}
	for _, meter := range req.Meters {
		if err := applyMeter(ctx, tx, accounts, dataplane, req.Instance, meter); err != nil {
			return empty, err
		}
		resp.Authority.MeterAcks = append(resp.Authority.MeterAcks, policy.AuthorityAck{
			ID: meter.WindowID, Sequence: meter.Sequence, Instance: meter.Instance,
		})
	}
	for _, request := range req.Requests {
		if err := issueGrant(ctx, tx, accounts, current, now, dataplane, req.Instance, request, resp.Authority); err != nil {
			return empty, err
		}
	}
	if err := persistAccounts(ctx, tx, accounts); err != nil {
		return empty, err
	}
	resp.ActiveTiers, err = activeTiers(ctx, tx, docs, budgets, accounts)
	if err != nil {
		return empty, err
	}
	if err := tx.Commit(ctx); err != nil {
		return empty, databaseError("commit sync", err)
	}
	return resp, nil
}

func readPolicies(ctx context.Context, tx pgx.Tx) ([]v1alpha1.GovernancePolicy, error) {
	rows, err := tx.Query(ctx, `SELECT name, doc_yaml FROM policies ORDER BY name`)
	if err != nil {
		return nil, databaseError("read policies", err)
	}
	defer rows.Close()
	docs := make([]v1alpha1.GovernancePolicy, 0)
	for rows.Next() {
		var name, body string
		if err := rows.Scan(&name, &body); err != nil {
			return nil, databaseError("scan policy", err)
		}
		parsed, err := policy.ParseWireDocs([]byte(body))
		if err != nil {
			return nil, errors.New("authority postgres: stored policy failed validation")
		}
		if len(parsed) != 1 || parsed[0].Metadata.Name != name || name == "" {
			return nil, errors.New("authority postgres: stored policy identity mismatch")
		}
		docs = append(docs, parsed[0])
	}
	return docs, databaseError("iterate policies", rows.Err())
}

func databaseTime(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, databaseError("read database clock", err)
	}
	return now.UTC(), nil
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// Postgres errors can contain row values or credentials in Detail and Message.
// Preserve only the SQLSTATE in a fresh error; context errors remain unwrap-able.
func databaseError(operation string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, context.Canceled):
		err = context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		err = context.DeadlineExceeded
	case errors.As(err, &pgErr):
		err = &pgconn.PgError{Code: pgErr.Code, Message: "database operation failed"}
	default:
		err = errors.New("database unavailable or operation failed")
	}
	return fmt.Errorf("authority postgres: %s: %w", operation, err)
}
