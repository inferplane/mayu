package keystore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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
	if err := migrateGovernanceColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("keystore: migrate: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// migrateGovernanceColumns adds the §8 D2 governance columns to a `keys` table
// that may already exist from before this feature (CREATE TABLE IF NOT EXISTS
// is a no-op on an existing table, so new columns need ALTER TABLE). SQLite
// has no idempotent ADD COLUMN, so a "duplicate column" error is expected and
// ignored on every open after the first; any other error is real.
func migrateGovernanceColumns(db *sql.DB) error {
	for _, stmt := range []string{
		`ALTER TABLE keys ADD COLUMN budget_usd_micros INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE keys ADD COLUMN tpm INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE keys ADD COLUMN rpm INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE keys ADD COLUMN expires_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE keys ADD COLUMN owner TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE keys ADD COLUMN metadata TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) Create(ctx context.Context, team string, allowedModels []string) (string, Principal, error) {
	return s.CreateWithOptions(ctx, team, allowedModels, KeyOptions{})
}

func (s *SQLiteStore) CreateWithOptions(ctx context.Context, team string, allowedModels []string, opts KeyOptions) (string, Principal, error) {
	plaintext, hashHex, keyID, err := generateKey()
	if err != nil {
		return "", Principal{}, err
	}
	metaJSON, err := encodeMetadata(opts.Metadata)
	if err != nil {
		return "", Principal{}, fmt.Errorf("keystore: metadata: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO keys (key_id, key_hash, team, allowed_models, created_at,
		 budget_usd_micros, tpm, rpm, expires_at, owner, metadata) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		keyID, hashHex, team, joinModels(allowedModels), nowRFC3339(),
		opts.BudgetUSDMicros, opts.TPM, opts.RPM, encodeExpiry(opts.ExpiresAt), opts.Owner, metaJSON)
	if err != nil {
		return "", Principal{}, fmt.Errorf("keystore: insert: %w", err)
	}
	p := Principal{KeyID: keyID, Team: team, AllowedModels: allowedModels, KeyOptions: opts}
	return plaintext, p, nil
}

var ErrKeyNotFound = errors.New("keystore: key not found")

const keyColumns = `key_id, team, allowed_models, budget_usd_micros, tpm, rpm, expires_at, owner, metadata`

// scanPrincipal reads one keyColumns-shaped row. Expiry is checked by the
// caller (Resolve treats an expired key as not-found; List shows it as-is so
// operators can see and revoke it).
func scanPrincipal(row interface{ Scan(...any) error }) (Principal, error) {
	var p Principal
	var models, expiresAt, metaJSON string
	if err := row.Scan(&p.KeyID, &p.Team, &models, &p.BudgetUSDMicros, &p.TPM, &p.RPM, &expiresAt, &p.Owner, &metaJSON); err != nil {
		return Principal{}, err
	}
	p.AllowedModels = splitModels(models)
	p.ExpiresAt = decodeExpiry(expiresAt)
	p.Metadata = decodeMetadata(metaJSON)
	return p, nil
}

func (s *SQLiteStore) Resolve(ctx context.Context, plaintext string) (Principal, error) {
	h := hashKey(plaintext)
	row := s.db.QueryRowContext(ctx,
		`SELECT `+keyColumns+` FROM keys WHERE key_hash = ? AND revoked = 0`, h)
	p, err := scanPrincipal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrKeyNotFound
	}
	if err != nil {
		return Principal{}, err
	}
	if p.ExpiresAt != nil && p.ExpiresAt.Before(time.Now().UTC()) {
		return Principal{}, ErrKeyNotFound
	}
	return p, nil
}

func encodeExpiry(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func decodeExpiry(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return &t
}

func encodeMetadata(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	b, err := json.Marshal(m)
	return string(b), err
}

func decodeMetadata(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	_ = json.Unmarshal([]byte(s), &m) // malformed → nil, never a hard error on read
	return m
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
		`SELECT `+keyColumns+` FROM keys WHERE revoked = 0 ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Principal
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

var _ Store = (*SQLiteStore)(nil)
