package bodystore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver (CGO_ENABLED=0), registered as "sqlite"
)

// TEXT/INTEGER/BLOB only, mirrors internal/analytics' SQLite schema shape.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS bodies (
  ref               TEXT PRIMARY KEY,
  record_id         TEXT NOT NULL DEFAULT '',
  team              TEXT NOT NULL DEFAULT '',
  created_ts        TEXT NOT NULL DEFAULT '',
  expires_ts        TEXT NOT NULL DEFAULT '',
  size              INTEGER NOT NULL DEFAULT 0,
  wrapped_key_nonce BLOB NOT NULL,
  wrapped_key_ct    BLOB NOT NULL,
  req_nonce         BLOB NOT NULL,
  req_ct            BLOB NOT NULL,
  resp_nonce        BLOB,
  resp_ct           BLOB
);
CREATE INDEX IF NOT EXISTS bodies_expires ON bodies(expires_ts);`

// SQLiteStore is the default single-instance backend.
type SQLiteStore struct {
	db *sql.DB
}

var _ Store = (*SQLiteStore)(nil)

func OpenSQLite(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(sqliteSchema); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) Put(ctx context.Context, row Row) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO bodies (ref, record_id, team, created_ts, expires_ts, size,
			wrapped_key_nonce, wrapped_key_ct, req_nonce, req_ct, resp_nonce, resp_ct)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ref) DO NOTHING`,
		row.Ref, row.RecordID, row.Team, row.CreatedTS, row.ExpiresTS, row.Size,
		row.WrappedKeyNonce, row.WrappedKeyCT, row.ReqNonce, row.ReqCT, row.RespNonce, row.RespCT)
	return err
}

func (s *SQLiteStore) Get(ctx context.Context, ref string) (Row, error) {
	var row Row
	err := s.db.QueryRowContext(ctx, `
		SELECT ref, record_id, team, created_ts, expires_ts, size,
			wrapped_key_nonce, wrapped_key_ct, req_nonce, req_ct, resp_nonce, resp_ct
		FROM bodies WHERE ref = ?`, ref).Scan(
		&row.Ref, &row.RecordID, &row.Team, &row.CreatedTS, &row.ExpiresTS, &row.Size,
		&row.WrappedKeyNonce, &row.WrappedKeyCT, &row.ReqNonce, &row.ReqCT, &row.RespNonce, &row.RespCT)
	if errors.Is(err, sql.ErrNoRows) {
		return Row{}, ErrGone
	}
	if err != nil {
		return Row{}, err
	}
	return row, nil
}

func (s *SQLiteStore) Delete(ctx context.Context, ref string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM bodies WHERE ref = ?`, ref)
	return err
}

// Purge deletes expired rows (TTL), then — if still over maxBytes — the
// oldest remaining rows until it isn't. See Store.Purge's doc.
func (s *SQLiteStore) Purge(ctx context.Context, now time.Time, maxBytes int64) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM bodies WHERE expires_ts <= ?`, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("bodystore: TTL purge: %w", err)
	}
	n, _ := res.RowsAffected()
	deleted := int(n)

	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM bodies`).Scan(&total); err != nil {
		return deleted, fmt.Errorf("bodystore: size query: %w", err)
	}
	for total > maxBytes {
		var ref string
		var size int64
		err := s.db.QueryRowContext(ctx, `SELECT ref, size FROM bodies ORDER BY created_ts ASC LIMIT 1`).Scan(&ref, &size)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return deleted, fmt.Errorf("bodystore: oldest-row query: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM bodies WHERE ref = ?`, ref); err != nil {
			return deleted, fmt.Errorf("bodystore: size-cap delete: %w", err)
		}
		deleted++
		total -= size
	}
	return deleted, nil
}
