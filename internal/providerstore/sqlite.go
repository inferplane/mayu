package providerstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// SQLiteStore is the shipping Store. Schema uses only portable types (TEXT) so
// the same DDL maps cleanly onto Postgres for the v0.2 HA path (keystore pattern).
type SQLiteStore struct{ db *sql.DB }

// schema — TEXT-only, Postgres-portable. The providers table has NO secret
// column: api_key_ref_env / api_key_ref_file hold the REFERENCE, never a value.
const schema = `
CREATE TABLE IF NOT EXISTS providers (
    name             TEXT PRIMARY KEY,
    type             TEXT NOT NULL,
    base_url         TEXT NOT NULL DEFAULT '',
    region           TEXT NOT NULL DEFAULT '',
    auth_mode        TEXT NOT NULL DEFAULT '',
    auth_profile     TEXT NOT NULL DEFAULT '',
    api_key_ref_env  TEXT NOT NULL DEFAULT '',
    api_key_ref_file TEXT NOT NULL DEFAULT ''
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
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("providerstore: schema: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) UpsertProvider(ctx context.Context, p ProviderRow) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO providers (name, type, base_url, region, auth_mode, auth_profile, api_key_ref_env, api_key_ref_file)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(name) DO UPDATE SET
    type=excluded.type, base_url=excluded.base_url, region=excluded.region,
    auth_mode=excluded.auth_mode, auth_profile=excluded.auth_profile,
    api_key_ref_env=excluded.api_key_ref_env, api_key_ref_file=excluded.api_key_ref_file`,
		p.Name, p.Type, p.BaseURL, p.Region, p.AuthMode, p.AuthProfile, p.APIKeyRefEnv, p.APIKeyRefFile)
	if err != nil {
		return fmt.Errorf("providerstore: upsert: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetProvider(ctx context.Context, name string) (ProviderRow, error) {
	var p ProviderRow
	err := s.db.QueryRowContext(ctx, `
SELECT name, type, base_url, region, auth_mode, auth_profile, api_key_ref_env, api_key_ref_file
FROM providers WHERE name = ?`, name).
		Scan(&p.Name, &p.Type, &p.BaseURL, &p.Region, &p.AuthMode, &p.AuthProfile, &p.APIKeyRefEnv, &p.APIKeyRefFile)
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
SELECT name, type, base_url, region, auth_mode, auth_profile, api_key_ref_env, api_key_ref_file
FROM providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderRow
	for rows.Next() {
		var p ProviderRow
		if err := rows.Scan(&p.Name, &p.Type, &p.BaseURL, &p.Region, &p.AuthMode, &p.AuthProfile, &p.APIKeyRefEnv, &p.APIKeyRefFile); err != nil {
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
