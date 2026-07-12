package providerstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const seededKey = "seeded"

// SetModel replaces a model's ordered target chain. Replace-all in one
// transaction: every existing position for the model is deleted, then the new
// chain is inserted at positions 0..n-1, so the stored order is the slice order.
func (s *SQLiteStore) SetModel(ctx context.Context, name string, targets []Target) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := replaceModel(ctx, tx, name, targets); err != nil {
		return err
	}
	return tx.Commit()
}

// replaceModel does the delete-then-insert for one model on an open tx (shared
// by SetModel and Seed).
func replaceModel(ctx context.Context, tx *sql.Tx, name string, targets []Target) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM model_targets WHERE model = ?`, name); err != nil {
		return fmt.Errorf("providerstore: set model: %w", err)
	}
	for i, t := range targets {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO model_targets (model, position, provider, model_id, api) VALUES (?,?,?,?,?)`,
			name, i, t.Provider, t.Model, t.API); err != nil {
			return fmt.Errorf("providerstore: set model: %w", err)
		}
	}
	return nil
}

func (s *SQLiteStore) ListModels(ctx context.Context) (map[string][]Target, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT model, provider, model_id, api FROM model_targets ORDER BY model, position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Target{}
	for rows.Next() {
		var model string
		var t Target
		if err := rows.Scan(&model, &t.Provider, &t.Model, &t.API); err != nil {
			return nil, err
		}
		out[model] = append(out[model], t)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteModel(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM model_targets WHERE model = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
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
func (s *SQLiteStore) Seed(ctx context.Context, providers []ProviderRow, models map[string][]Target) (bool, error) {
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
	for name, targets := range models {
		if err := replaceModel(ctx, tx, name, targets); err != nil {
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
