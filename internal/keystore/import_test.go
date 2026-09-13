package keystore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPostgresImportRawSQLiteIdentityAndTombstones(t *testing.T) {
	s := openPostgresTest(t, postgresTestDSN(t))
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "source.db")
	source, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	team := completeTeam()
	if err := source.UpsertTeam(ctx, team); err != nil {
		t.Fatal(err)
	}
	live, old, err := source.CreateWithOptions(ctx, "team-a", []string{"a"}, completeKeyOptions())
	if err != nil {
		t.Fatal(err)
	}
	dead, tombstone, err := source.Create(ctx, "team-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Revoke(ctx, tombstone.KeyID); err != nil {
		t.Fatal(err)
	}
	// Preserve a raw legacy ID too; a migration through Create/EnsureKey would
	// regenerate it, discard the tombstone or change timestamps.
	if _, err := source.db.ExecContext(ctx, `UPDATE keys SET key_id='legacy-key-id', created_at='2001-02-03T04:05:06Z' WHERE key_id=?`, old.KeyID); err != nil {
		t.Fatal(err)
	}
	expected, err := source.Resolve(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	wantTeam, _, err := source.GetTeam(ctx, team.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.ImportSQLite(ctx, path)
	if err != nil || result.Keys != 2 || result.Teams != 1 {
		t.Fatalf("import counts: %+v %v", result, err)
	}
	got, err := s.Resolve(ctx, live)
	if err != nil || got.KeyID != "legacy-key-id" || !reflect.DeepEqual(got.KeyOptions, expected.KeyOptions) ||
		!reflect.DeepEqual(got.TeamSnapshot, &wantTeam) {
		t.Fatalf("raw import lost identity or fields: %v", err)
	}
	if _, err := s.Resolve(ctx, dead); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("import revived tombstone")
	}
	var hash, created string
	if err := s.db.QueryRow(ctx, `SELECT key_hash, created_at FROM keys WHERE key_id='legacy-key-id'`).Scan(&hash, &created); err != nil {
		t.Fatal("read imported row")
	}
	if hash != hashKey(live) || created != "2001-02-03T04:05:06Z" {
		t.Fatal("import regenerated source identity")
	}
	// Return newly inserted counts; an identical retry makes no writes.
	result, err = s.ImportSQLite(ctx, path)
	if err != nil || result != (ImportResult{}) {
		t.Fatalf("idempotent retry: %+v %v", result, err)
	}
	if err := s.Seed(ctx, []TeamRecord{team}, []SeedKey{{
		Plaintext: live, Team: team.Name, AllowedModels: []string{"a"}, Options: completeKeyOptions(),
	}}); err != nil {
		t.Fatalf("seed must attach to matching imported rows without changing legacy IDs: %v", err)
	}
	if err := s.Revoke(ctx, got.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportSQLite(ctx, path); !errors.Is(err, ErrSnapshotChanged) {
		t.Fatal("stale import did not reject destination tombstone conflict")
	}
	if _, err := s.Resolve(ctx, live); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("retry revived destination tombstone")
	}
}

func TestPostgresImportConflictRollsBackAllRows(t *testing.T) {
	s := openPostgresTest(t, postgresTestDSN(t))
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "source.db")
	source, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.UpsertTeam(ctx, TeamRecord{Name: "new-team"}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.EnsureKey(ctx, "fixture-import-conflict", "source", nil, KeyOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureKey(ctx, "fixture-import-conflict", "destination", nil, KeyOptions{}); err != nil {
		t.Fatal(err)
	}
	result, err := s.ImportSQLite(ctx, path)
	if !errors.Is(err, ErrSnapshotChanged) || result != (ImportResult{}) {
		t.Fatalf("conflicting import succeeded: %+v %v", result, err)
	}
	if _, ok, err := s.GetTeam(ctx, "new-team"); err != nil || ok {
		t.Fatal("failed import retained partial team")
	}
	p, err := s.Resolve(ctx, "fixture-import-conflict")
	if err != nil || p.Team != "destination" {
		t.Fatal("failed import modified destination")
	}
}

func TestPostgresImportLegacySQLiteDoesNotMigrateSource(t *testing.T) {
	s := openPostgresTest(t, postgresTestDSN(t))
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	source, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.Exec(`CREATE TABLE keys (
key_id TEXT PRIMARY KEY, key_hash TEXT NOT NULL UNIQUE, team TEXT NOT NULL,
allowed_models TEXT NOT NULL, created_at TEXT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(`INSERT INTO keys VALUES ('legacy', ?, 'old-team', '*', '2000-01-01T00:00:00Z', 1)`, hashKey("fixture-legacy")); err != nil {
		t.Fatal(err)
	}
	result, err := s.ImportSQLite(ctx, path)
	if err != nil || result.Keys != 1 || result.Teams != 0 {
		t.Fatalf("legacy import failed: %+v %v", result, err)
	}
	var columns int
	if err := source.QueryRow(`SELECT count(*) FROM pragma_table_info('keys')`).Scan(&columns); err != nil || columns != 6 {
		t.Fatal("import mutated source schema")
	}
	if _, err := s.Resolve(ctx, "fixture-legacy"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("legacy tombstone lost")
	}
}
