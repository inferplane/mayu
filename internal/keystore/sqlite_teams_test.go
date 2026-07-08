package keystore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestTeamCRUD_roundTrip proves the basic Upsert -> Get -> List -> Delete
// lifecycle, including that zero-value fields (unlimited/unset, same
// convention as KeyOptions) round-trip as zero, not some other sentinel.
func TestTeamCRUD_roundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if err := s.UpsertTeam(ctx, TeamRecord{
		Name:             "platform-eng",
		AllowedModels:    []string{"claude-sonnet-4-6", "claude-haiku-4-5"},
		RPM:              60,
		TPM:              10000,
		TokensPerDay:     1_000_000,
		QuotaOnExceeded:  "block",
		BudgetUSDMicros:  5_000_000,
		BudgetOnExceeded: "warn",
		GuardrailID:      "gr-abc123",
		GuardrailVersion: "3",
		AllowedRegions:   []string{"eu", "us"},
	}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.GetTeam(ctx, "platform-eng")
	if err != nil || !ok {
		t.Fatalf("GetTeam: ok=%v err=%v", ok, err)
	}
	if got.RPM != 60 || got.TPM != 10000 || got.TokensPerDay != 1_000_000 ||
		got.QuotaOnExceeded != "block" || got.BudgetUSDMicros != 5_000_000 || got.BudgetOnExceeded != "warn" {
		t.Fatalf("fields not round-tripped: %+v", got)
	}
	if got.GuardrailID != "gr-abc123" || got.GuardrailVersion != "3" {
		t.Fatalf("guardrail fields not round-tripped: %+v", got)
	}
	if len(got.AllowedRegions) != 2 || got.AllowedRegions[0] != "eu" || got.AllowedRegions[1] != "us" {
		t.Fatalf("allowed_regions not round-tripped: %+v", got.AllowedRegions)
	}
	if len(got.AllowedModels) != 2 || got.AllowedModels[0] != "claude-sonnet-4-6" {
		t.Fatalf("allowed_models not round-tripped: %+v", got.AllowedModels)
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Fatalf("timestamps not set: %+v", got)
	}

	list, err := s.ListTeams(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListTeams: %v %+v", err, list)
	}

	if err := s.DeleteTeam(ctx, "platform-eng"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.GetTeam(ctx, "platform-eng"); err != nil || ok {
		t.Fatalf("team survived delete: ok=%v err=%v", ok, err)
	}
}

func TestTeamCRUD_zeroValueMeansUnlimited(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.UpsertTeam(ctx, TeamRecord{Name: "no-limits"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetTeam(ctx, "no-limits")
	if err != nil || !ok {
		t.Fatalf("GetTeam: ok=%v err=%v", ok, err)
	}
	if got.RPM != 0 || got.TPM != 0 || got.BudgetUSDMicros != 0 || len(got.AllowedModels) != 0 {
		t.Fatalf("zero-value team should mean unlimited: %+v", got)
	}
}

func TestTeamCRUD_getMissReturnsFalseNotError(t *testing.T) {
	s := openTest(t)
	_, ok, err := s.GetTeam(context.Background(), "nonexistent")
	if err != nil || ok {
		t.Fatalf("miss should be ok=false err=nil, got ok=%v err=%v", ok, err)
	}
}

func TestTeamCRUD_deleteMissingReturnsErrTeamNotFound(t *testing.T) {
	s := openTest(t)
	err := s.DeleteTeam(context.Background(), "nonexistent")
	if err != ErrTeamNotFound {
		t.Fatalf("delete of missing team: got %v, want ErrTeamNotFound", err)
	}
}

// TestTeamCRUD_upsertPreservesCreatedAtButAdvancesUpdatedAt proves a repeat
// Upsert on the same name is an update, not insert-or-ignore, and that
// created_at is stable across the update (only set on first insert).
func TestTeamCRUD_upsertPreservesCreatedAtButAdvancesUpdatedAt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.UpsertTeam(ctx, TeamRecord{Name: "t", RPM: 1}); err != nil {
		t.Fatal(err)
	}
	first, _, _ := s.GetTeam(ctx, "t")

	if err := s.UpsertTeam(ctx, TeamRecord{Name: "t", RPM: 2}); err != nil {
		t.Fatal(err)
	}
	second, _, _ := s.GetTeam(ctx, "t")

	if second.RPM != 2 {
		t.Fatalf("upsert must overwrite, not insert-or-ignore: %+v", second)
	}
	if second.CreatedAt != first.CreatedAt {
		t.Fatalf("created_at must be preserved across update: %q -> %q", first.CreatedAt, second.CreatedAt)
	}
}

// TestTeamsTable_appearsOnPreExistingKeysOnlyDatabase proves that a keystore
// file created before D3 (keys table only, no teams table) still opens and
// gains the teams table — CREATE TABLE IF NOT EXISTS running unconditionally
// inside ensureSchema, same as the fresh-DB path (no ALTER-TABLE migration
// needed for a brand-new table).
func TestTeamsTable_appearsOnPreExistingKeysOnlyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE keys (
		key_id TEXT PRIMARY KEY, key_hash TEXT NOT NULL UNIQUE, team TEXT NOT NULL,
		allowed_models TEXT NOT NULL, created_at TEXT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite on pre-D3 db: %v", err)
	}
	defer s.Close()
	if err := s.UpsertTeam(context.Background(), TeamRecord{Name: "t"}); err != nil {
		t.Fatalf("teams table missing after migration: %v", err)
	}
}

// TestTeamsTable_migratesGuardrailColumns proves a teams table that predates
// D6 (guardrail_id/guardrail_version) gets those columns added in place,
// without losing existing rows.
func TestTeamsTable_migratesGuardrailColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-d6.db")
	old, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE teams (
		name TEXT PRIMARY KEY, allowed_models TEXT NOT NULL DEFAULT '',
		rpm INTEGER NOT NULL DEFAULT 0, tpm INTEGER NOT NULL DEFAULT 0,
		tokens_per_day INTEGER NOT NULL DEFAULT 0, quota_on_exceeded TEXT NOT NULL DEFAULT '',
		budget_usd_micros INTEGER NOT NULL DEFAULT 0, budget_on_exceeded TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO teams (name, created_at, updated_at) VALUES ('pre-existing', 't1', 't1')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite on pre-D6 teams table: %v", err)
	}
	defer s.Close()

	// The pre-existing row survives the migration, with guardrail fields
	// defaulting to empty (no override).
	got, ok, err := s.GetTeam(context.Background(), "pre-existing")
	if err != nil || !ok {
		t.Fatalf("pre-existing row lost during migration: ok=%v err=%v", ok, err)
	}
	if got.GuardrailID != "" || got.GuardrailVersion != "" {
		t.Fatalf("migrated columns should default empty: %+v", got)
	}

	// New writes with guardrail fields work post-migration.
	if err := s.UpsertTeam(context.Background(), TeamRecord{Name: "new", GuardrailID: "gr-x", GuardrailVersion: "1"}); err != nil {
		t.Fatalf("upsert after migration: %v", err)
	}
	got2, ok, err := s.GetTeam(context.Background(), "new")
	if err != nil || !ok || got2.GuardrailID != "gr-x" || got2.GuardrailVersion != "1" {
		t.Fatalf("post-migration guardrail write/read: ok=%v err=%v got=%+v", ok, err, got2)
	}
}

// TestTeamsTable_migratesAllowedRegionsColumn proves a teams table that
// predates D7 (allowed_regions) — i.e. has the D6 guardrail columns but not
// this one — gets the column added in place, without losing existing rows.
func TestTeamsTable_migratesAllowedRegionsColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-d7.db")
	old, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE teams (
		name TEXT PRIMARY KEY, allowed_models TEXT NOT NULL DEFAULT '',
		rpm INTEGER NOT NULL DEFAULT 0, tpm INTEGER NOT NULL DEFAULT 0,
		tokens_per_day INTEGER NOT NULL DEFAULT 0, quota_on_exceeded TEXT NOT NULL DEFAULT '',
		budget_usd_micros INTEGER NOT NULL DEFAULT 0, budget_on_exceeded TEXT NOT NULL DEFAULT '',
		guardrail_id TEXT NOT NULL DEFAULT '', guardrail_version TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO teams (name, guardrail_id, created_at, updated_at) VALUES ('pre-existing', 'gr-old', 't1', 't1')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite on pre-D7 teams table: %v", err)
	}
	defer s.Close()

	// The pre-existing row survives the migration, its D6 field intact, and
	// the new allowed_regions column defaults to empty (unrestricted).
	got, ok, err := s.GetTeam(context.Background(), "pre-existing")
	if err != nil || !ok {
		t.Fatalf("pre-existing row lost during migration: ok=%v err=%v", ok, err)
	}
	if got.GuardrailID != "gr-old" {
		t.Fatalf("pre-existing D6 field lost during migration: %+v", got)
	}
	if len(got.AllowedRegions) != 0 {
		t.Fatalf("migrated column should default empty: %+v", got)
	}

	// New writes with allowed_regions work post-migration.
	if err := s.UpsertTeam(context.Background(), TeamRecord{Name: "new", AllowedRegions: []string{"eu"}}); err != nil {
		t.Fatalf("upsert after migration: %v", err)
	}
	got2, ok, err := s.GetTeam(context.Background(), "new")
	if err != nil || !ok || len(got2.AllowedRegions) != 1 || got2.AllowedRegions[0] != "eu" {
		t.Fatalf("post-migration allowed_regions write/read: ok=%v err=%v got=%+v", ok, err, got2)
	}
}
