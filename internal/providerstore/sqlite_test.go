package providerstore

import (
	"context"
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
