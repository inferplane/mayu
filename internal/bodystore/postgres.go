package bodystore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaLockKey is a Postgres advisory-lock key distinct from analytics'
// (847001) and providerstore's — arbitrary but fixed, so concurrent replica
// boots serialize migration instead of racing CREATE TABLE.
const schemaLockKey int64 = 847002

const pgSchema = `
CREATE TABLE IF NOT EXISTS bodies (
  ref TEXT PRIMARY KEY, record_id TEXT NOT NULL DEFAULT '', team TEXT NOT NULL DEFAULT '',
  created_ts TEXT NOT NULL DEFAULT '', expires_ts TEXT NOT NULL DEFAULT '',
  size BIGINT NOT NULL DEFAULT 0,
  wrapped_key_nonce BYTEA NOT NULL, wrapped_key_ct BYTEA NOT NULL,
  req_nonce BYTEA NOT NULL, req_ct BYTEA NOT NULL,
  resp_nonce BYTEA, resp_ct BYTEA
);
CREATE INDEX IF NOT EXISTS bodies_expires ON bodies(expires_ts);`

// PostgresStore is the shared HA backend (opt-in, D4/ADR-018). Every replica
// writes only rows it minted (collision-free ULID refs) and Purge's deletes
// are idempotent, so — unlike analytics Mode B — no lease/fencing is needed:
// any number of replicas may run Purge concurrently without coordination.
type PostgresStore struct {
	db *pgxpool.Pool
}

var _ Store = (*PostgresStore)(nil)

// NewPostgres opens the store and ensures its schema exists. dsn is never
// echoed in an error — a pgx parse failure embeds the connection string
// (which may carry a password), so that failure path returns a fixed message
// instead of wrapping (same rule as internal/analytics/pgstore.New).
func NewPostgres(ctx context.Context, dsn string) (*PostgresStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("open bodystore postgres: invalid dsn (check dsn_ref resolves to a valid postgres:// connection string)")
	}
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open bodystore postgres: %w", err)
	}
	if err := db.Ping(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping bodystore postgres: %w", err)
	}
	s := &PostgresStore{db: db}
	if err := s.ensureSchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *PostgresStore) Close() error {
	s.db.Close()
	return nil
}

// ensureSchema serializes migration via a session-level advisory lock, held
// across lock+migrate+unlock on ONE acquired connection (pgxpool precedent:
// internal/analytics/pgstore.Store.ensureSchema — three separate pool
// operations would not actually serialize anything).
func (s *PostgresStore) ensureSchema(ctx context.Context) error {
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire bodystore schema migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, schemaLockKey); err != nil {
		return fmt.Errorf("lock bodystore schema migration: %w", err)
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, schemaLockKey)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin bodystore schema migration: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, pgSchema); err != nil {
		return fmt.Errorf("create bodystore schema: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) Put(ctx context.Context, row Row) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO bodies (ref, record_id, team, created_ts, expires_ts, size,
			wrapped_key_nonce, wrapped_key_ct, req_nonce, req_ct, resp_nonce, resp_ct)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (ref) DO NOTHING`,
		row.Ref, row.RecordID, row.Team, row.CreatedTS, row.ExpiresTS, row.Size,
		row.WrappedKeyNonce, row.WrappedKeyCT, row.ReqNonce, row.ReqCT, row.RespNonce, row.RespCT)
	return err
}

func (s *PostgresStore) Get(ctx context.Context, ref string) (Row, error) {
	var row Row
	err := s.db.QueryRow(ctx, `
		SELECT ref, record_id, team, created_ts, expires_ts, size,
			wrapped_key_nonce, wrapped_key_ct, req_nonce, req_ct, resp_nonce, resp_ct
		FROM bodies WHERE ref = $1`, ref).Scan(
		&row.Ref, &row.RecordID, &row.Team, &row.CreatedTS, &row.ExpiresTS, &row.Size,
		&row.WrappedKeyNonce, &row.WrappedKeyCT, &row.ReqNonce, &row.ReqCT, &row.RespNonce, &row.RespCT)
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, ErrGone
	}
	if err != nil {
		return Row{}, err
	}
	return row, nil
}

func (s *PostgresStore) Delete(ctx context.Context, ref string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM bodies WHERE ref = $1`, ref)
	return err
}

func (s *PostgresStore) ListWrappedKeys(ctx context.Context) ([]WrappedKeyRow, error) {
	rows, err := s.db.Query(ctx, `SELECT ref, wrapped_key_nonce, wrapped_key_ct FROM bodies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WrappedKeyRow
	for rows.Next() {
		var r WrappedKeyRow
		if err := rows.Scan(&r.Ref, &r.Nonce, &r.CT); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UpdateWrappedKey(ctx context.Context, ref string, oldNonce, oldCT, newNonce, newCT []byte) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE bodies SET wrapped_key_nonce=$1, wrapped_key_ct=$2 WHERE ref=$3 AND wrapped_key_nonce=$4 AND wrapped_key_ct=$5`,
		newNonce, newCT, ref, oldNonce, oldCT)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Purge runs safely on every replica concurrently (idempotent deletes, no
// lease needed — see the package/type doc).
func (s *PostgresStore) Purge(ctx context.Context, now time.Time, maxBytes int64) (int, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM bodies WHERE expires_ts <= $1`, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("bodystore: TTL purge: %w", err)
	}
	deleted := int(tag.RowsAffected())

	var total int64
	if err := s.db.QueryRow(ctx, `SELECT COALESCE(SUM(size),0) FROM bodies`).Scan(&total); err != nil {
		return deleted, fmt.Errorf("bodystore: size query: %w", err)
	}
	for total > maxBytes {
		var ref string
		var size int64
		err := s.db.QueryRow(ctx, `SELECT ref, size FROM bodies ORDER BY created_ts ASC LIMIT 1`).Scan(&ref, &size)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return deleted, fmt.Errorf("bodystore: oldest-row query: %w", err)
		}
		if _, err := s.db.Exec(ctx, `DELETE FROM bodies WHERE ref = $1`, ref); err != nil {
			return deleted, fmt.Errorf("bodystore: size-cap delete: %w", err)
		}
		deleted++
		total -= size
	}
	return deleted, nil
}
