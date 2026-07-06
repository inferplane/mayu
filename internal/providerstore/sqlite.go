package providerstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	sqlite "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// SQLiteStore is the shipping Store. Schema uses only portable types (TEXT) so
// the same DDL maps cleanly onto Postgres for the v0.2 HA path (keystore pattern).
type SQLiteStore struct{ db *sql.DB }

// schema — TEXT-only, Postgres-portable. The providers table has NO secret
// column: api_key_ref_env / api_key_ref_file hold the REFERENCE, never a value.
// auth_header is included directly here (not just via the migration below) so
// a FRESH database gets the canonical shape in one DDL instead of always
// taking the ALTER TABLE path — same reasoning as the keystore's governance
// columns. ensureSchema still runs the migration too, so it's a no-op on a
// fresh DB but required for one created before this column existed.
const schema = `
CREATE TABLE IF NOT EXISTS providers (
    name             TEXT PRIMARY KEY,
    type             TEXT NOT NULL,
    base_url         TEXT NOT NULL DEFAULT '',
    region           TEXT NOT NULL DEFAULT '',
    auth_mode        TEXT NOT NULL DEFAULT '',
    auth_profile     TEXT NOT NULL DEFAULT '',
    api_key_ref_env  TEXT NOT NULL DEFAULT '',
    api_key_ref_file TEXT NOT NULL DEFAULT '',
    auth_header      TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS model_targets (
    model    TEXT NOT NULL,
    position INTEGER NOT NULL,
    provider TEXT NOT NULL,
    model_id TEXT NOT NULL,
    api      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (model, position)
);
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// OpenSQLite opens (creating if needed) the provider store at path. busy_timeout
// + WAL so a keys CLI and a running serve back off instead of hard-erroring on
// contention; single connection keeps writes serialized at the driver too.
func OpenSQLite(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := ensureSchemaWithRetry(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("providerstore: schema: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// ensureSchemaWithRetry retries ensureSchema on SQLITE_BUSY (the driver's
// typed error code, not a string match) — several processes racing a
// cold-start migration (e.g. a rolling restart) can make BEGIN EXCLUSIVE
// observe an immediate busy even with busy_timeout set on the DSN. Mirrors
// the keystore's migration (internal/keystore/sqlite.go).
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

// ensureSchema creates the tables (if absent) and adds any column that
// predates the running binary — both inside one BEGIN EXCLUSIVE/COMMIT, which
// serializes the whole create-or-check-then-write sequence across PROCESSES
// (SQLite's EXCLUSIVE lock is file-level), not just goroutines.
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
	rows, err := conn.QueryContext(ctx, `PRAGMA table_info(providers)`)
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
		{"auth_header", `ALTER TABLE providers ADD COLUMN auth_header TEXT NOT NULL DEFAULT ''`},
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

func (s *SQLiteStore) UpsertProvider(ctx context.Context, p ProviderRow) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO providers (name, type, base_url, region, auth_mode, auth_profile, api_key_ref_env, api_key_ref_file, auth_header)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(name) DO UPDATE SET
    type=excluded.type, base_url=excluded.base_url, region=excluded.region,
    auth_mode=excluded.auth_mode, auth_profile=excluded.auth_profile,
    api_key_ref_env=excluded.api_key_ref_env, api_key_ref_file=excluded.api_key_ref_file,
    auth_header=excluded.auth_header`,
		p.Name, p.Type, p.BaseURL, p.Region, p.AuthMode, p.AuthProfile, p.APIKeyRefEnv, p.APIKeyRefFile, p.AuthHeader)
	if err != nil {
		return fmt.Errorf("providerstore: upsert: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetProvider(ctx context.Context, name string) (ProviderRow, error) {
	var p ProviderRow
	err := s.db.QueryRowContext(ctx, `
SELECT name, type, base_url, region, auth_mode, auth_profile, api_key_ref_env, api_key_ref_file, auth_header
FROM providers WHERE name = ?`, name).
		Scan(&p.Name, &p.Type, &p.BaseURL, &p.Region, &p.AuthMode, &p.AuthProfile, &p.APIKeyRefEnv, &p.APIKeyRefFile, &p.AuthHeader)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderRow{}, ErrNotFound
	}
	if err != nil {
		return ProviderRow{}, err
	}
	return p, nil
}

func (s *SQLiteStore) ListProviders(ctx context.Context) ([]ProviderRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT name, type, base_url, region, auth_mode, auth_profile, api_key_ref_env, api_key_ref_file, auth_header
FROM providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderRow
	for rows.Next() {
		var p ProviderRow
		if err := rows.Scan(&p.Name, &p.Type, &p.BaseURL, &p.Region, &p.AuthMode, &p.AuthProfile, &p.APIKeyRefEnv, &p.APIKeyRefFile, &p.AuthHeader); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteProvider(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM providers WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

var _ Store = (*SQLiteStore)(nil)
