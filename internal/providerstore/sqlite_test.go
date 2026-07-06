package providerstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "providers.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestProviderRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	row := ProviderRow{
		Name:         "anthropic-prod",
		Type:         "anthropic",
		BaseURL:      "https://api.anthropic.com",
		APIKeyRefEnv: "ANTHROPIC_KEY",
	}
	if err := s.UpsertProvider(ctx, row); err != nil {
		t.Fatalf("UpsertProvider: %v", err)
	}
	got, err := s.GetProvider(ctx, "anthropic-prod")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got != row {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, row)
	}
}

func TestProviderRoundTripAuthHeader(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	row := ProviderRow{
		Name: "openrouter", Type: "anthropic", BaseURL: "https://openrouter.ai/api",
		APIKeyRefEnv: "OPENROUTER_KEY", AuthHeader: "bearer",
	}
	if err := s.UpsertProvider(ctx, row); err != nil {
		t.Fatalf("UpsertProvider: %v", err)
	}
	got, err := s.GetProvider(ctx, "openrouter")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got.AuthHeader != "bearer" {
		t.Fatalf("auth_header not round-tripped: got %+v", got)
	}
	list, err := s.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].AuthHeader != "bearer" {
		t.Fatalf("ListProviders lost auth_header: %+v", list)
	}
}

// TestMigrationAddsAuthHeaderColumn opens a store whose providers table
// predates the auth_header column (created with the OLD schema — no
// CREATE TABLE IF NOT EXISTS no-ops on an existing table's missing column) and
// confirms OpenSQLite adds it rather than failing, and that the store is
// immediately usable with the new column.
func TestMigrationAddsAuthHeaderColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	const oldSchema = `
CREATE TABLE providers (
    name             TEXT PRIMARY KEY,
    type             TEXT NOT NULL,
    base_url         TEXT NOT NULL DEFAULT '',
    region           TEXT NOT NULL DEFAULT '',
    auth_mode        TEXT NOT NULL DEFAULT '',
    auth_profile     TEXT NOT NULL DEFAULT '',
    api_key_ref_env  TEXT NOT NULL DEFAULT '',
    api_key_ref_file TEXT NOT NULL DEFAULT ''
);
CREATE TABLE model_targets (model TEXT, position INTEGER, provider TEXT, model_id TEXT, api TEXT, PRIMARY KEY (model, position));
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`
	if _, err := old.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO providers (name, type) VALUES ('pre-existing', 'anthropic')`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite on pre-migration schema: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	got, err := s.GetProvider(ctx, "pre-existing")
	if err != nil {
		t.Fatalf("GetProvider on migrated row: %v", err)
	}
	if got.AuthHeader != "" {
		t.Fatalf("migrated row should default auth_header to empty, got %q", got.AuthHeader)
	}

	if err := s.UpsertProvider(ctx, ProviderRow{Name: "new", Type: "anthropic", AuthHeader: "bearer"}); err != nil {
		t.Fatalf("UpsertProvider after migration: %v", err)
	}
	got2, err := s.GetProvider(ctx, "new")
	if err != nil || got2.AuthHeader != "bearer" {
		t.Fatalf("post-migration write: got=%+v err=%v", got2, err)
	}
}

func TestProviderUpsertReplaces(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.UpsertProvider(ctx, ProviderRow{Name: "p", Type: "anthropic", BaseURL: "old"})
	if err := s.UpsertProvider(ctx, ProviderRow{Name: "p", Type: "anthropic", BaseURL: "new"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetProvider(ctx, "p")
	if got.BaseURL != "new" {
		t.Fatalf("upsert did not replace: base_url=%q", got.BaseURL)
	}
	list, _ := s.ListProviders(ctx)
	if len(list) != 1 {
		t.Fatalf("upsert created a duplicate row: %d providers", len(list))
	}
}

func TestProviderListSorted(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.UpsertProvider(ctx, ProviderRow{Name: "zeta", Type: "anthropic"})
	_ = s.UpsertProvider(ctx, ProviderRow{Name: "alpha", Type: "bedrock", Region: "us-west-2", AuthMode: "default"})
	list, err := s.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "zeta" {
		t.Fatalf("ListProviders not name-sorted: %+v", list)
	}
	if list[0].Region != "us-west-2" || list[0].AuthMode != "default" {
		t.Fatalf("bedrock fields not persisted: %+v", list[0])
	}
}

func TestProviderDeleteMissing(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.DeleteProvider(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteProvider(missing) = %v, want ErrNotFound", err)
	}
	_ = s.UpsertProvider(ctx, ProviderRow{Name: "p", Type: "anthropic"})
	if err := s.DeleteProvider(ctx, "p"); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	if _, err := s.GetProvider(ctx, "p"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProvider after delete = %v, want ErrNotFound", err)
	}
}

// TestNoSecretColumn is the structural secret-safety guarantee (ADR-008): the
// providers table has NO column capable of holding a secret VALUE — only refs.
func TestNoSecretColumn(t *testing.T) {
	s := openTestStore(t)
	rows, err := s.db.Query(`PRAGMA table_info(providers)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "api_key", "apikey", "secret", "key", "token", "password":
			t.Fatalf("providers table has a secret-capable column %q", name)
		}
	}
}
