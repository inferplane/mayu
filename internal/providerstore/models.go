package providerstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const seededKey = "seeded"

// SetModel replaces a model's aliases and ordered target chain. Replace-all in
// one transaction: every existing target position AND every existing alias for
// the model are deleted, then the new chain/aliases are inserted, so the stored
// state is exactly route's.
func (s *SQLiteStore) SetModel(ctx context.Context, name string, route ModelRoute) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := replaceModel(ctx, tx, name, route); err != nil {
		return err
	}
	return tx.Commit()
}

// replaceModel does the delete-then-insert for one model's targets and aliases
// on an open tx (shared by SetModel and Seed).
func replaceModel(ctx context.Context, tx *sql.Tx, name string, route ModelRoute) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM model_targets WHERE model = ?`, name); err != nil {
		return fmt.Errorf("providerstore: set model: %w", err)
	}
	for i, t := range route.Targets {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO model_targets (model, position, provider, model_id, api) VALUES (?,?,?,?,?)`,
			name, i, t.Provider, t.Model, t.API); err != nil {
			return fmt.Errorf("providerstore: set model: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM model_aliases WHERE model = ?`, name); err != nil {
		return fmt.Errorf("providerstore: set model: %w", err)
	}
	for _, alias := range route.Aliases {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO model_aliases (model, alias) VALUES (?,?)`, name, alias); err != nil {
			return fmt.Errorf("providerstore: set model: %w", err)
		}
	}
	return nil
}

func (s *SQLiteStore) ListModels(ctx context.Context) (map[string]ModelRoute, error) {
	out := map[string]ModelRoute{}

	rows, err := s.db.QueryContext(ctx,
		`SELECT model, provider, model_id, api FROM model_targets ORDER BY model, position`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var model string
		var t Target
		if err := rows.Scan(&model, &t.Provider, &t.Model, &t.API); err != nil {
			rows.Close()
			return nil, err
		}
		r := out[model]
		r.Targets = append(r.Targets, t)
		out[model] = r
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	aliasRows, err := s.db.QueryContext(ctx, `SELECT model, alias FROM model_aliases ORDER BY model, alias`)
	if err != nil {
		return nil, err
	}
	defer aliasRows.Close()
	for aliasRows.Next() {
		var model, alias string
		if err := aliasRows.Scan(&model, &alias); err != nil {
			return nil, err
		}
		r := out[model]
		r.Aliases = append(r.Aliases, alias)
		out[model] = r
	}
	return out, aliasRows.Err()
}

func (s *SQLiteStore) DeleteModel(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	res, err := tx.ExecContext(ctx, `DELETE FROM model_targets WHERE model = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM model_aliases WHERE model = ?`, name); err != nil {
		return err
	}
	return tx.Commit()
}

// Seeded reports whether the durable seed marker is set.
func (s *SQLiteStore) Seeded(ctx context.Context) (bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, seededKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "1", nil
}

// Seed imports the file topology and sets the durable marker in ONE transaction,
// but only if not already seeded. Returns true if it seeded. The marker (not a
// row count) gates this, so a deliberately-emptied store is never re-seeded.
func (s *SQLiteStore) Seed(ctx context.Context, providers []ProviderRow, models map[string]ModelRoute) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	// Re-check the marker inside the txn (the single connection already
	// serializes, but this keeps check-and-seed atomic).
	var v string
	switch err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, seededKey).Scan(&v); {
	case err == nil:
		return false, nil // already seeded — no-op
	case errors.Is(err, sql.ErrNoRows):
		// not seeded — proceed
	default:
		return false, err
	}

	for _, p := range providers {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO providers (name, type, base_url, region, auth_mode, auth_profile, api_key_ref_env, api_key_ref_file, auth_header, guardrail_id, guardrail_version)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			p.Name, p.Type, p.BaseURL, p.Region, p.AuthMode, p.AuthProfile, p.APIKeyRefEnv, p.APIKeyRefFile, p.AuthHeader, p.GuardrailID, p.GuardrailVersion); err != nil {
			return false, fmt.Errorf("providerstore: seed provider %q: %w", p.Name, err)
		}
	}
	for name, route := range models {
		if err := replaceModel(ctx, tx, name, route); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, '1')`, seededKey); err != nil {
		return false, fmt.Errorf("providerstore: mark seeded: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
