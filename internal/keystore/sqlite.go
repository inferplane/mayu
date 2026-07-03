package keystore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	sqlite "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// SQLiteStore is the M3 default Store. Schema uses only portable SQL types so
// the same DDL maps cleanly onto Postgres in v0.2 (co-agent guidance).
type SQLiteStore struct{ db *sql.DB }

// schema — TEXT/INTEGER only, no SQLite-specific types, for Postgres portability.
// Includes the §8 D2 governance columns directly (not just via the ALTER TABLE
// entries in migrateGovernanceColumns's column list) so a FRESH database gets
// the canonical shape in one DDL instead of always taking the migration path,
// and so the two can't silently diverge if a future column is added to one
// but not the other. migrateGovernanceColumns still runs (idempotent no-op on
// a fresh DB) to upgrade pre-existing databases that predate this feature.
const schema = `
CREATE TABLE IF NOT EXISTS keys (
    key_id             TEXT PRIMARY KEY,
    key_hash           TEXT NOT NULL UNIQUE,
    team               TEXT NOT NULL,
    allowed_models     TEXT NOT NULL,
    created_at         TEXT NOT NULL,
    revoked            INTEGER NOT NULL DEFAULT 0,
    budget_usd_micros  INTEGER NOT NULL DEFAULT 0,
    tpm                INTEGER NOT NULL DEFAULT 0,
    rpm                INTEGER NOT NULL DEFAULT 0,
    expires_at         TEXT NOT NULL DEFAULT '',
    owner              TEXT NOT NULL DEFAULT '',
    metadata           TEXT NOT NULL DEFAULT ''
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
	if err := ensureSchemaWithRetry(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("keystore: schema: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// ensureSchemaWithRetry retries ensureSchema on SQLITE_BUSY, identified by the
// driver's typed error code (5, the stable SQLite C API result code — not a
// string-matching heuristic on error text, which round-2 review correctly
// flagged as fragile for the ALTER-TABLE path this replaced). busy_timeout
// already makes SQLite itself wait+retry internally; this covers the residual
// gap where BEGIN EXCLUSIVE can still observe an immediate SQLITE_BUSY under
// several processes racing a cold-start migration at once.
func ensureSchemaWithRetry(db *sql.DB) error {
	const attempts = 5
	var err error
	for i := 0; i < attempts; i++ {
		if err = ensureSchema(db); err == nil {
			return nil
		}
		var sqliteErr *sqlite.Error
		if !errors.As(err, &sqliteErr) || sqliteErr.Code() != sqlite3.SQLITE_BUSY {
			return err // a real error, not lock contention — don't retry
		}
		time.Sleep(time.Duration(20*(i+1)) * time.Millisecond)
	}
	return err
}

// ensureSchema creates the `keys` table (if absent) and adds the §8 D2
// governance columns (if the table predates them) — both inside ONE
// BEGIN EXCLUSIVE / COMMIT on one pinned connection (db.Conn). SQLite's
// EXCLUSIVE lock is a file-level lock, so this serializes the whole
// create-or-check-then-write sequence across PROCESSES too — e.g. two
// inferplane pods briefly overlapping during a rolling restart (or a scale-up
// from 0) against a shared keystore file. Without this, two processes can
// both see "table/column missing" before either writes, and the loser's
// CREATE/ALTER fails outright (reproduced by
// TestMigration_concurrentOpensDoNotRace at realistic 2-3-pod concurrency).
// busy_timeout (set in the DSN) makes a blocked BEGIN EXCLUSIVE wait and
// retry rather than error immediately.
func ensureSchema(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
		return err
	}
	rollback := func() { conn.ExecContext(ctx, `ROLLBACK`) }

	if _, err := conn.ExecContext(ctx, schema); err != nil {
		rollback()
		return err
	}

	existing := map[string]bool{}
	rows, err := conn.QueryContext(ctx, `PRAGMA table_info(keys)`)
	if err != nil {
		rollback()
		return err
	}
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			rollback()
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		rollback()
		return err
	}
	rows.Close()

	columns := []struct{ name, ddl string }{
		{"budget_usd_micros", `ALTER TABLE keys ADD COLUMN budget_usd_micros INTEGER NOT NULL DEFAULT 0`},
		{"tpm", `ALTER TABLE keys ADD COLUMN tpm INTEGER NOT NULL DEFAULT 0`},
		{"rpm", `ALTER TABLE keys ADD COLUMN rpm INTEGER NOT NULL DEFAULT 0`},
		{"expires_at", `ALTER TABLE keys ADD COLUMN expires_at TEXT NOT NULL DEFAULT ''`},
		{"owner", `ALTER TABLE keys ADD COLUMN owner TEXT NOT NULL DEFAULT ''`},
		{"metadata", `ALTER TABLE keys ADD COLUMN metadata TEXT NOT NULL DEFAULT ''`},
	}
	for _, c := range columns {
		if existing[c.name] {
			continue
		}
		if _, err := conn.ExecContext(ctx, c.ddl); err != nil {
			rollback()
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		rollback()
		return err
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
// operators can see and revoke it). A corrupt expires_at is a hard error here
// (fail-closed) — expiry is an auth control, unlike metadata, which is
// advisory and tolerates malformed data.
func scanPrincipal(row interface{ Scan(...any) error }) (Principal, error) {
	var p Principal
	var models, expiresAt, metaJSON string
	if err := row.Scan(&p.KeyID, &p.Team, &models, &p.BudgetUSDMicros, &p.TPM, &p.RPM, &expiresAt, &p.Owner, &metaJSON); err != nil {
		return Principal{}, err
	}
	p.AllowedModels = splitModels(models)
	exp, err := decodeExpiry(expiresAt)
	if err != nil {
		return Principal{}, err
	}
	p.ExpiresAt = exp
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

func decodeExpiry(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, fmt.Errorf("keystore: invalid expires_at %q: %w", s, err)
	}
	return &t, nil
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
