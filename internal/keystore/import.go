package keystore

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ImportSQLite reads raw rows, including revoked hashes, without opening the
// SQLiteStore (which would migrate/write the source). Operators must drain old
// writers before cutover. A read transaction fixes the source snapshot, while
// all destination rows are inserted/verified and committed atomically.
func (s *PostgresStore) ImportSQLite(ctx context.Context, path string) (ImportResult, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	empty := ImportResult{}
	info, err := os.Stat(path)
	if err != nil {
		return empty, postgresError("stat import source", err)
	}
	if !info.Mode().IsRegular() {
		return empty, ErrStoreUnavailable
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return empty, postgresError("resolve import path", err)
	}
	sourceURL := url.URL{Scheme: "file", Path: abs}
	params := url.Values{"mode": {"ro"}, "_pragma": {"query_only(1)", "busy_timeout(1000)"}}
	sourceURL.RawQuery = params.Encode()
	db, err := sql.Open("sqlite", sourceURL.String())
	if err != nil {
		return empty, postgresError("open import source", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	source, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return empty, postgresError("begin import source", err)
	}
	defer source.Rollback()
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return empty, postgresError("begin import", err)
	}
	defer rollbackPostgres(tx)
	if err := lockPostgresIdentity(ctx, tx); err != nil {
		return empty, err
	}
	teams, err := importSQLiteTeams(ctx, source, tx)
	if err != nil {
		return empty, err
	}
	keys, err := importSQLiteKeys(ctx, source, tx)
	if err != nil {
		return empty, err
	}
	if err := source.Commit(); err != nil {
		return empty, postgresError("finish import source", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return empty, postgresError("commit import", err)
	}
	return ImportResult{Keys: keys, Teams: teams}, nil
}

// Table/column names are private literals, never a supplied identifier. Legacy
// sources receive read-side defaults for the same columns SQLiteStore migrates.
func importSQLiteQuery(ctx context.Context, tx *sql.Tx, table string, columns []postgresColumn) (string, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return "", postgresError("read import schema", err)
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
			return "", postgresError("scan import schema", err)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return "", postgresError("read import columns", err)
	}
	if len(present) == 0 && table == "teams" {
		return "", nil // keys-only databases predate the team table
	}
	expressions := make([]string, len(columns))
	for i, col := range columns {
		switch {
		case present[col.name]:
			expressions[i] = col.name
		case col.required:
			return "", ErrStoreUnavailable
		case col.numeric:
			expressions[i] = "0"
		default:
			expressions[i] = "''"
		}
	}
	return `SELECT ` + strings.Join(expressions, ",") + ` FROM ` + table, nil
}

func importSQLiteKeys(ctx context.Context, source *sql.Tx, tx pgx.Tx) (int, error) {
	query, err := importSQLiteQuery(ctx, source, "keys", postgresKeyColumns)
	if err != nil {
		return 0, err
	}
	rows, err := source.QueryContext(ctx, query)
	if err != nil {
		return 0, postgresError("read import keys", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var k postgresKey
		if err := rows.Scan(k.destinations()...); err != nil {
			return 0, postgresError("scan import key", err)
		}
		inserted, err := insertPostgresKey(ctx, tx, k, false)
		if err != nil {
			return 0, err
		}
		if inserted {
			count++
		}
	}
	return count, postgresError("finish import keys", rows.Err())
}

func importSQLiteTeams(ctx context.Context, source *sql.Tx, tx pgx.Tx) (int, error) {
	query, err := importSQLiteQuery(ctx, source, "teams", postgresTeamColumns)
	if err != nil || query == "" {
		return 0, err
	}
	rows, err := source.QueryContext(ctx, query)
	if err != nil {
		return 0, postgresError("read import teams", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var team postgresTeam
		if err := rows.Scan(team.destinations()...); err != nil {
			return 0, postgresError("scan import team", err)
		}
		inserted, err := insertPostgresTeam(ctx, tx, team, false)
		if err != nil {
			return 0, err
		}
		if inserted {
			count++
		}
	}
	return count, postgresError("finish import teams", rows.Err())
}
