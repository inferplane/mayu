package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/inferplane/inferplane/internal/keystore"
)

func TestSharedKeyCLIImportPreservesCredentialsAndTombstones(t *testing.T) {
	dsn := isolatedPostgresDSN(t)
	t.Setenv("KEY_CLI_TEST_DSN", dsn)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"key_store":{"type":"postgres","dsn_ref":{"env":"KEY_CLI_TEST_DSN"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "old.sqlite")
	old, err := keystore.OpenSQLite(source)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := old.UpsertTeam(ctx, keystore.TeamRecord{Name: "team"}); err != nil {
		t.Fatal(err)
	}
	active, original, err := old.CreateWithOptions(ctx, "team", []string{"m"}, keystore.KeyOptions{RPM: 7, Owner: "opaque-user"})
	if err != nil {
		t.Fatal(err)
	}
	revoked, p, err := old.Create(ctx, "team", []string{"m"})
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Revoke(ctx, p.KeyID); err != nil {
		t.Fatal(err)
	}
	old.Close()
	if err := keysCmd([]string{"import", "--config", cfg, "--sqlite", source}); err != nil {
		t.Fatal(err)
	}
	store, err := openKeysCLI("", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.Resolve(ctx, active)
	if err != nil || got.KeyID != original.KeyID || got.RPM != 7 || got.Owner != "opaque-user" {
		t.Fatalf("import lost identity or options: %v", err)
	}
	if _, err := store.Resolve(ctx, revoked); err == nil {
		t.Fatal("import reactivated a revoked credential")
	}
	if err := keysCmd([]string{"revoke", "--config", cfg, "--id", original.KeyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(ctx, active); err == nil {
		t.Fatal("CLI revocation was not shared")
	}
	if _, err := openKeysCLI(source, cfg); err == nil {
		t.Fatal("ambiguous backend selection accepted")
	}
}
