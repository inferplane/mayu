package keystore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// SQLiteStore is the M3 default Store. Schema uses only portable SQL types so
// the same DDL maps cleanly onto Postgres in v0.2 (co-agent guidance).
type SQLiteStore struct{ db *sql.DB }

// schema — TEXT/INTEGER only, no SQLite-specific types, for Postgres portability.
const schema = `
CREATE TABLE IF NOT EXISTS keys (
    key_id        TEXT PRIMARY KEY,
    key_hash      TEXT NOT NULL UNIQUE,
    team          TEXT NOT NULL,
    allowed_models TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    revoked       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_keys_hash ON keys(key_hash) WHERE revoked = 0;
`

func OpenSQLite(path string) (*SQLiteStore, error) {
	// busy_timeout + WAL so the keys CLI and a running serve (two processes on
	// the same file) back off and retry instead of hard-erroring on contention.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("keystore: schema: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Create(ctx context.Context, team string, allowedModels []string) (string, Principal, error) {
	plaintext, hashHex, keyID, err := generateKey()
	if err != nil {
		return "", Principal{}, err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO keys (key_id, key_hash, team, allowed_models, created_at) VALUES (?,?,?,?,?)`,
		keyID, hashHex, team, joinModels(allowedModels), nowRFC3339())
	if err != nil {
		return "", Principal{}, fmt.Errorf("keystore: insert: %w", err)
	}
	return plaintext, Principal{KeyID: keyID, Team: team, AllowedModels: allowedModels}, nil
}

var ErrKeyNotFound = errors.New("keystore: key not found")

func (s *SQLiteStore) Resolve(ctx context.Context, plaintext string) (Principal, error) {
	h := hashKey(plaintext)
	var p Principal
	var models string
	err := s.db.QueryRowContext(ctx,
		`SELECT key_id, team, allowed_models FROM keys WHERE key_hash = ? AND revoked = 0`, h).
		Scan(&p.KeyID, &p.Team, &models)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrKeyNotFound
	}
	if err != nil {
		return Principal{}, err
	}
	p.AllowedModels = splitModels(models)
	return p, nil
}

func (s *SQLiteStore) Revoke(ctx context.Context, keyID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE keys SET revoked = 1 WHERE key_id = ?`, keyID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrKeyNotFound
	}
	return nil
}

func (s *SQLiteStore) List(ctx context.Context) ([]Principal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key_id, team, allowed_models FROM keys WHERE revoked = 0 ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Principal
	for rows.Next() {
		var p Principal
		var models string
		if err := rows.Scan(&p.KeyID, &p.Team, &models); err != nil {
			return nil, err
		}
		p.AllowedModels = splitModels(models)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

var _ Store = (*SQLiteStore)(nil)
