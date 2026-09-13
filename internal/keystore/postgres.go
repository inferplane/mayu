package keystore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const postgresTimeout = 5 * time.Second

// Distinct from policy and monetary-authority schema migration locks.
const postgresSchemaLock int64 = 0x6b657973 // "keys"

// PostgresStore is the synchronous key authority for the explicit shared
// gateway profile. It keeps no cached identities or team permissions.
type PostgresStore struct{ db *pgxpool.Pool }

// OpenPostgres initializes the schema transactionally before returning. The
// default SQLite constructor never calls this or opens a Postgres connection.
func OpenPostgres(ctx context.Context, dsn string) (*PostgresStore, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	if strings.TrimSpace(dsn) == "" {
		return nil, ErrStoreUnavailable
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, postgresError("parse connection", err)
	}
	// Bound pool capacity and all network/lock waits even when a DSN supplies
	// larger values. Preserve a shorter requested connection timeout.
	cfg.MaxConns, cfg.MinConns, cfg.MinIdleConns = 4, 0, 0
	cfg.MaxConnLifetime, cfg.MaxConnIdleTime = 30*time.Minute, time.Minute
	if cfg.ConnConfig.ConnectTimeout <= 0 || cfg.ConnConfig.ConnectTimeout > postgresTimeout {
		cfg.ConnConfig.ConnectTimeout = postgresTimeout
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "5000"
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "5000"
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, postgresError("open pool", err)
	}
	s := &PostgresStore{db: db}
	if err := s.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// postgresError wraps only our safe sentinel. Driver errors (including their
// unwrap chains) can contain a DSN, hash, SQL argument, or server detail.
func postgresError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("keystore postgres: %s: %w", operation, ErrStoreUnavailable)
}

func rollbackPostgres(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func postgresNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	return now, postgresError("read database time", err)
}

func (s *PostgresStore) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	_, err := s.db.Exec(ctx, `SELECT k.key_id, t.name FROM keys k LEFT JOIN teams t ON k.team=t.name LIMIT 0`)
	return postgresError("check readiness", err)
}

func (s *PostgresStore) Close() error {
	s.db.Close()
	return nil
}

func (s *PostgresStore) Create(ctx context.Context, team string, models []string) (string, Principal, error) {
	return s.CreateWithOptions(ctx, team, models, KeyOptions{})
}

func (s *PostgresStore) CreateWithOptions(ctx context.Context, team string, models []string, opts KeyOptions) (string, Principal, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	plain, hash, id, err := generateKey()
	if err != nil {
		return "", Principal{}, postgresError("generate key", err)
	}
	p, err := s.writeKey(ctx, hash, id, team, models, opts, false)
	if err != nil {
		return "", Principal{}, err
	}
	return plain, p, nil
}

// EnsureKey preserves the existing upsert contract. Replica bootstrap uses
// Seed instead, which rejects conflicting declarations and never updates rows.
func (s *PostgresStore) EnsureKey(ctx context.Context, plaintext, team string, models []string, opts KeyOptions) (Principal, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	hash := hashKey(plaintext)
	return s.writeKey(ctx, hash, "ik_"+hash[:12], team, models, opts, true)
}

const postgresInsertKey = `INSERT INTO keys
(key_id, key_hash, team, allowed_models, created_at, revoked, budget_usd_micros,
 tpm, rpm, expires_at, owner, metadata, budget_usd_micros_per_day)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`

func (s *PostgresStore) writeKey(ctx context.Context, hash, id, team string, models []string, opts KeyOptions, upsert bool) (Principal, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return Principal{}, postgresError("begin key write", err)
	}
	defer rollbackPostgres(tx)
	now, err := postgresNow(ctx, tx)
	if err != nil {
		return Principal{}, err
	}
	k, err := newPostgresKey(hash, id, team, models, opts, now)
	if err != nil {
		return Principal{}, err
	}
	query := postgresInsertKey
	if upsert {
		query += ` ON CONFLICT(key_hash) DO UPDATE SET
team=excluded.team, allowed_models=excluded.allowed_models,
budget_usd_micros=excluded.budget_usd_micros, tpm=excluded.tpm, rpm=excluded.rpm,
expires_at=excluded.expires_at, owner=excluded.owner, metadata=excluded.metadata,
budget_usd_micros_per_day=excluded.budget_usd_micros_per_day`
	}
	if _, err := tx.Exec(ctx, query, k.values()...); err != nil {
		return Principal{}, postgresError("write key", err)
	}
	// Creating an already expired key or ensuring a revoked key is allowed,
	// matching SQLite. Only Resolve/admission authenticate the returned data.
	p, _, err := scanPostgresSnapshot(tx.QueryRow(ctx, postgresSnapshotQuery+`WHERE k.key_hash=$1`, hash))
	if err != nil {
		return Principal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Principal{}, postgresError("commit key write", err)
	}
	return p, nil
}

func (s *PostgresStore) Resolve(ctx context.Context, plaintext string) (Principal, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Principal{}, postgresError("begin resolve", err)
	}
	defer rollbackPostgres(tx)
	p, now, err := scanPostgresSnapshot(tx.QueryRow(ctx,
		postgresSnapshotQuery+`WHERE k.key_hash=$1 AND k.revoked=0`, hashKey(plaintext)))
	if err != nil {
		return Principal{}, err
	}
	if err := checkPostgresExpiry(p, now); err != nil {
		return Principal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Principal{}, postgresError("commit resolve", err)
	}
	return p, nil
}

func (s *PostgresStore) List(ctx context.Context) ([]Principal, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, postgresError("begin list", err)
	}
	defer rollbackPostgres(tx)
	rows, err := tx.Query(ctx, postgresSnapshotQuery+`WHERE k.revoked=0 ORDER BY k.created_at, k.key_id`)
	if err != nil {
		return nil, postgresError("list keys", err)
	}
	defer rows.Close()
	var out []Principal
	for rows.Next() {
		p, _, err := scanPostgresSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, postgresError("read listed keys", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, postgresError("commit list", err)
	}
	return out, nil
}

func (s *PostgresStore) Revoke(ctx context.Context, keyID string) error {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	tag, err := s.db.Exec(ctx, `UPDATE keys SET revoked=1 WHERE key_id=$1`, keyID)
	if err != nil {
		return postgresError("revoke key", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// Lock both tables, in keys/teams order, before reading. Row locks alone cannot
// protect the absence of a team from an insertion during authorization.
func lockPostgresIdentity(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `LOCK TABLE keys, teams IN SHARE ROW EXCLUSIVE MODE`)
	return postgresError("lock identity tables", err)
}

func (s *PostgresStore) RevokeSnapshot(ctx context.Context, authorized Principal) error {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	if authorized.SharedRevision == "" || !authorized.TeamSnapshotLoaded {
		return ErrSnapshotChanged
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return postgresError("begin conditional revoke", err)
	}
	defer rollbackPostgres(tx)
	if err := lockPostgresIdentity(ctx, tx); err != nil {
		return err
	}
	current, _, err := scanPostgresSnapshot(tx.QueryRow(ctx,
		postgresSnapshotQuery+`WHERE k.key_id=$1 AND k.revoked=0`, authorized.KeyID))
	if err != nil {
		return err
	}
	if current.Team != authorized.Team || current.SharedRevision != authorized.SharedRevision {
		return ErrSnapshotChanged
	}
	if _, err := tx.Exec(ctx, `UPDATE keys SET revoked=1 WHERE key_id=$1`, authorized.KeyID); err != nil {
		return postgresError("conditional revoke", err)
	}
	return postgresError("commit conditional revoke", tx.Commit(ctx))
}

const postgresInsertTeam = `INSERT INTO teams (` + teamColumns + `)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`

func (s *PostgresStore) UpsertTeam(ctx context.Context, team TeamRecord) error {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return postgresError("begin team write", err)
	}
	defer rollbackPostgres(tx)
	now, err := postgresNow(ctx, tx)
	if err != nil {
		return err
	}
	t := newPostgresTeam(team, now)
	_, err = tx.Exec(ctx, postgresInsertTeam+` ON CONFLICT(name) DO UPDATE SET
allowed_models=excluded.allowed_models, rpm=excluded.rpm, tpm=excluded.tpm,
tokens_per_day=excluded.tokens_per_day, quota_on_exceeded=excluded.quota_on_exceeded,
budget_usd_micros=excluded.budget_usd_micros, budget_on_exceeded=excluded.budget_on_exceeded,
budget_usd_micros_per_day=excluded.budget_usd_micros_per_day, guardrail_id=excluded.guardrail_id,
guardrail_version=excluded.guardrail_version, allowed_regions=excluded.allowed_regions,
updated_at=excluded.updated_at`, t.values()...)
	if err != nil {
		return postgresError("upsert team", err)
	}
	return postgresError("commit team write", tx.Commit(ctx))
}

func (s *PostgresStore) GetTeam(ctx context.Context, name string) (TeamRecord, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	t, err := scanTeam(s.db.QueryRow(ctx, `SELECT `+teamColumns+` FROM teams WHERE name=$1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamRecord{}, false, nil
	}
	if err != nil {
		return TeamRecord{}, false, postgresError("get team", err)
	}
	return t, true, nil
}

func (s *PostgresStore) ListTeams(ctx context.Context) ([]TeamRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	rows, err := s.db.Query(ctx, `SELECT `+teamColumns+` FROM teams ORDER BY name`)
	if err != nil {
		return nil, postgresError("list teams", err)
	}
	defer rows.Close()
	var out []TeamRecord
	for rows.Next() {
		t, err := scanTeam(rows)
		if err != nil {
			return nil, postgresError("read listed team", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, postgresError("read listed teams", err)
	}
	return out, nil
}

func (s *PostgresStore) DeleteTeam(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	tag, err := s.db.Exec(ctx, `DELETE FROM teams WHERE name=$1`, name)
	if err != nil {
		return postgresError("delete team", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTeamNotFound
	}
	return nil
}

var _ Store = (*PostgresStore)(nil)
var _ TeamStore = (*PostgresStore)(nil)
var _ KeyEnsurer = (*PostgresStore)(nil)
