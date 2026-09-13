package keystore

import "context"

const postgresSchema = `
CREATE TABLE IF NOT EXISTS keys (
 key_id TEXT PRIMARY KEY, key_hash TEXT NOT NULL UNIQUE, team TEXT NOT NULL,
 allowed_models TEXT NOT NULL, created_at TEXT NOT NULL,
 revoked INTEGER NOT NULL DEFAULT 0,
 budget_usd_micros BIGINT NOT NULL DEFAULT 0,
 tpm BIGINT NOT NULL DEFAULT 0, rpm BIGINT NOT NULL DEFAULT 0,
 expires_at TEXT NOT NULL DEFAULT '', owner TEXT NOT NULL DEFAULT '',
 metadata TEXT NOT NULL DEFAULT '', budget_usd_micros_per_day BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_keys_hash ON keys(key_hash) WHERE revoked=0;
CREATE TABLE IF NOT EXISTS teams (
 name TEXT PRIMARY KEY, allowed_models TEXT NOT NULL DEFAULT '',
 rpm BIGINT NOT NULL DEFAULT 0, tpm BIGINT NOT NULL DEFAULT 0,
 tokens_per_day BIGINT NOT NULL DEFAULT 0, quota_on_exceeded TEXT NOT NULL DEFAULT '',
 budget_usd_micros BIGINT NOT NULL DEFAULT 0, budget_on_exceeded TEXT NOT NULL DEFAULT '',
 budget_usd_micros_per_day BIGINT NOT NULL DEFAULT 0,
 guardrail_id TEXT NOT NULL DEFAULT '', guardrail_version TEXT NOT NULL DEFAULT '',
 allowed_regions TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS keystore_seed_fingerprints (
 kind TEXT NOT NULL, identity TEXT NOT NULL, fingerprint TEXT NOT NULL,
 PRIMARY KEY (kind, identity)
);`

type postgresColumn struct {
	name     string
	numeric  bool
	required bool
}

// The order also defines the raw import scan, matching postgresKey/postgresTeam.
// Required columns existed in the oldest supported SQLite schema; only the
// subsequently added fields may be defaulted during a read-only legacy import.
var postgresKeyColumns = []postgresColumn{
	{name: "key_id", required: true}, {name: "key_hash", required: true},
	{name: "team", required: true}, {name: "allowed_models", required: true},
	{name: "created_at", required: true}, {name: "revoked", numeric: true, required: true},
	{name: "budget_usd_micros", numeric: true}, {name: "tpm", numeric: true}, {name: "rpm", numeric: true},
	{name: "expires_at"}, {name: "owner"}, {name: "metadata"}, {name: "budget_usd_micros_per_day", numeric: true},
}

var postgresTeamColumns = []postgresColumn{
	{name: "name", required: true}, {name: "allowed_models", required: true},
	{name: "rpm", numeric: true, required: true}, {name: "tpm", numeric: true, required: true},
	{name: "tokens_per_day", numeric: true, required: true}, {name: "quota_on_exceeded", required: true},
	{name: "budget_usd_micros", numeric: true, required: true}, {name: "budget_on_exceeded", required: true},
	{name: "budget_usd_micros_per_day", numeric: true}, {name: "guardrail_id"},
	{name: "guardrail_version"}, {name: "allowed_regions"},
	{name: "created_at", required: true}, {name: "updated_at", required: true},
}

func (s *PostgresStore) initialize(ctx context.Context) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return postgresError("begin schema", err)
	}
	defer rollbackPostgres(tx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, postgresSchemaLock); err != nil {
		return postgresError("lock schema", err)
	}
	if _, err := tx.Exec(ctx, postgresSchema); err != nil {
		return postgresError("create schema", err)
	}
	for _, table := range []struct {
		name    string
		columns []postgresColumn
	}{{"keys", postgresKeyColumns}, {"teams", postgresTeamColumns}} {
		for _, col := range table.columns {
			if !col.required {
				kind := "TEXT NOT NULL DEFAULT ''"
				if col.numeric {
					kind = "BIGINT NOT NULL DEFAULT 0"
				}
				if _, err := tx.Exec(ctx, `ALTER TABLE `+table.name+` ADD COLUMN IF NOT EXISTS `+col.name+` `+kind); err != nil {
					return postgresError("add schema column", err)
				}
			}
			if col.numeric && col.name != "revoked" {
				var kind string
				if err := tx.QueryRow(ctx, `SELECT data_type FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name=$1 AND column_name=$2`, table.name, col.name).Scan(&kind); err != nil {
					return postgresError("check schema type", err)
				}
				switch kind {
				case "bigint":
				case "integer", "smallint":
					// Identifiers come only from the literal column lists above.
					if _, err := tx.Exec(ctx, `ALTER TABLE `+table.name+` ALTER COLUMN `+col.name+` TYPE BIGINT`); err != nil {
						return postgresError("widen schema limit", err)
					}
				default:
					return ErrStoreUnavailable
				}
			}
		}
	}
	return postgresError("commit schema", tx.Commit(ctx))
}
