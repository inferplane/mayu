package keystore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPostgresMigrationWidensLegacyLimitsAndPreservesRows(t *testing.T) {
	dsn := postgresTestDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect legacy fixture")
	}
	defer conn.Close(ctx)
	// SQLite's portable DDL gives the old INTEGER layout. The Postgres
	// migration must upgrade every limit without discarding either table.
	if _, err := conn.Exec(ctx, schema); err != nil {
		t.Fatal("create legacy INTEGER schema")
	}
	if _, err := conn.Exec(ctx, `INSERT INTO keys(key_id,key_hash,team,allowed_models,created_at,revoked)
VALUES('legacy-live',$1,'legacy-team','*','2000-01-01T00:00:00Z',0),
('legacy-revoked',$2,'legacy-team','*','2000-01-01T00:00:00Z',1)`,
		hashKey("fixture-upgrade-live"), hashKey("fixture-upgrade-revoked")); err != nil {
		t.Fatal("create legacy identities")
	}
	s := openPostgresTest(t, dsn)
	if _, err := s.Resolve(ctx, "fixture-upgrade-live"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, "fixture-upgrade-revoked"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("migration revived tombstone")
	}
	var bigintCount, revokedIntegerCount int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name IN ('keys','teams')
AND column_name IN ('rpm','tpm','tokens_per_day','budget_usd_micros','budget_usd_micros_per_day')
AND data_type='bigint'`).Scan(&bigintCount); err != nil || bigintCount != 9 {
		t.Fatal("not all nine limits use BIGINT")
	}
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='keys' AND column_name='revoked'
AND data_type='integer'`).Scan(&revokedIntegerCount); err != nil || revokedIntegerCount != 1 {
		t.Fatal("revoked must retain INTEGER representation")
	}
	if _, err := s.EnsureKey(ctx, "fixture-upgrade-live", "team-a", nil, completeKeyOptions()); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTeam(ctx, completeTeam()); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresMigrationAddsMissingColumnsAtomically(t *testing.T) {
	dsn := postgresTestDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect migration fixture")
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE TABLE keys (
key_id TEXT PRIMARY KEY, key_hash TEXT NOT NULL UNIQUE, team TEXT NOT NULL,
allowed_models TEXT NOT NULL, created_at TEXT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0);
CREATE TABLE teams (name TEXT PRIMARY KEY, allowed_models TEXT NOT NULL DEFAULT '',
rpm TEXT NOT NULL DEFAULT 'invalid', tpm INTEGER NOT NULL DEFAULT 0,
tokens_per_day INTEGER NOT NULL DEFAULT 0, quota_on_exceeded TEXT NOT NULL DEFAULT '',
budget_usd_micros INTEGER NOT NULL DEFAULT 0, budget_on_exceeded TEXT NOT NULL DEFAULT '',
created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal("create intentionally incompatible legacy schema")
	}
	if _, err := OpenPostgres(ctx, dsn); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatal("invalid schema migration should fail closed")
	}
	var additions int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='keys' AND column_name='owner'`).Scan(&additions); err != nil || additions != 0 {
		t.Fatal("failed migration committed partial ALTER statements")
	}
	if _, err := conn.Exec(ctx, `ALTER TABLE teams ALTER COLUMN rpm DROP DEFAULT;
ALTER TABLE teams ALTER COLUMN rpm TYPE INTEGER USING 0;
ALTER TABLE teams ALTER COLUMN rpm SET DEFAULT 0`); err != nil {
		t.Fatal("repair fixture")
	}
	s := openPostgresTest(t, dsn)
	if _, _, err := s.CreateWithOptions(ctx, "team-a", nil, completeKeyOptions()); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTeam(ctx, completeTeam()); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresResolveNeverTearsKeyAndTeamSnapshot(t *testing.T) {
	dsn := postgresTestDSN(t)
	a, b := openPostgresTest(t, dsn), openPostgresTest(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.UpsertTeam(ctx, TeamRecord{Name: "team", GuardrailID: "state-0"}); err != nil {
		t.Fatal(err)
	}
	plain, p, err := a.CreateWithOptions(ctx, "team", nil, KeyOptions{Owner: "state-0"})
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	go func() {
		for i := 1; i <= 30; i++ {
			tx, err := b.db.Begin(ctx)
			if err != nil {
				errs <- errors.New("begin snapshot writer")
				return
			}
			state := fmt.Sprintf("state-%d", i)
			if _, err = tx.Exec(ctx, `UPDATE keys SET owner=$1 WHERE key_id=$2`, state, p.KeyID); err == nil {
				_, err = tx.Exec(ctx, `UPDATE teams SET guardrail_id=$1 WHERE name='team'`, state)
			}
			if err != nil {
				_ = tx.Rollback(ctx)
				errs <- errors.New("update snapshot writer")
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs <- errors.New("commit snapshot writer")
				return
			}
		}
		errs <- nil
	}()
	for range 60 {
		got, err := a.Resolve(ctx, plain)
		if err != nil || got.TeamSnapshot == nil || got.Owner != got.TeamSnapshot.GuardrailID {
			cancel()
			<-errs
			t.Fatal("observed a torn key/team snapshot")
		}
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
}

func TestPostgresConditionalRevokeRechecksAfterWaitingForWriter(t *testing.T) {
	dsn := postgresTestDSN(t)
	a, b := openPostgresTest(t, dsn), openPostgresTest(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plain, p, err := a.Create(ctx, "authorized-team", nil)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := b.db.Begin(ctx)
	if err != nil {
		t.Fatal("begin transfer")
	}
	defer writer.Rollback(ctx)
	if _, err := writer.Exec(ctx, `UPDATE keys SET team='transferred-team' WHERE key_id=$1`, p.KeyID); err != nil {
		t.Fatal("stage transfer")
	}
	result := make(chan error, 1)
	go func() { result <- a.RevokeSnapshot(ctx, p) }()
	for {
		var waiting bool
		err := b.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
WHERE relation='keys'::regclass AND mode='ShareRowExclusiveLock' AND NOT granted)`).Scan(&waiting)
		if err != nil {
			t.Fatal("observe conditional revoke lock")
		}
		if waiting {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("conditional revoke did not wait for transfer: %v", err)
		case <-ctx.Done():
			t.Fatal("conditional revoke never acquired identity lock")
		case <-time.After(time.Millisecond):
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal("commit transfer")
	}
	if err := <-result; !errors.Is(err, ErrSnapshotChanged) {
		t.Fatalf("stale snapshot revoked transferred identity: %v", err)
	}
	got, err := b.Resolve(ctx, plain)
	if err != nil || got.Team != "transferred-team" {
		t.Fatal("conditional revoke mutated transferred key")
	}
}

func TestPostgresTeamLifecycleAndMissingKeys(t *testing.T) {
	s := openPostgresTest(t, postgresTestDSN(t))
	ctx := context.Background()
	if _, err := s.Resolve(ctx, "fixture-unknown"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("unknown key should return ErrKeyNotFound")
	}
	if err := s.UpsertTeam(ctx, TeamRecord{Name: "team", RPM: 1}); err != nil {
		t.Fatal(err)
	}
	before, _, err := s.GetTeam(ctx, "team")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTeam(ctx, TeamRecord{Name: "team", RPM: 2, CreatedAt: "must-ignore", UpdatedAt: "must-ignore"}); err != nil {
		t.Fatal(err)
	}
	after, _, err := s.GetTeam(ctx, "team")
	beforeTime, beforeErr := time.Parse(time.RFC3339Nano, before.UpdatedAt)
	afterTime, afterErr := time.Parse(time.RFC3339Nano, after.UpdatedAt)
	if err != nil || beforeErr != nil || afterErr != nil ||
		before.CreatedAt != after.CreatedAt || after.RPM != 2 || !afterTime.After(beforeTime) {
		t.Fatal("team upsert failed to preserve creation/advance modification")
	}
	plain, p, err := s.Create(ctx, "team", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTeam(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.GetTeam(ctx, "team"); err != nil || ok {
		t.Fatal("deleted team still exists")
	}
	if err := s.DeleteTeam(ctx, "team"); !errors.Is(err, ErrTeamNotFound) {
		t.Fatal("missing team deletion should return ErrTeamNotFound")
	}
	got, err := s.Resolve(ctx, plain)
	if err != nil || got.TeamSnapshot != nil || !got.TeamSnapshotLoaded || got.SharedRevision == p.SharedRevision {
		t.Fatal("team deletion not reflected in auth snapshot")
	}
}

func TestPostgresEveryOperationHonorsCancellation(t *testing.T) {
	s := openPostgresTest(t, postgresTestDSN(t))
	ctx := context.Background()
	plain, p, err := s.Create(ctx, "team", nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source.db")
	source, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal("begin helper transaction")
	}
	defer tx.Rollback(ctx)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	tests := map[string]func() error{
		"Ready":             func() error { return s.Ready(cancelled) },
		"Create":            func() error { _, _, err := s.Create(cancelled, "team", nil); return err },
		"CreateWithOptions": func() error { _, _, err := s.CreateWithOptions(cancelled, "team", nil, KeyOptions{}); return err },
		"Resolve":           func() error { _, err := s.Resolve(cancelled, plain); return err },
		"EnsureKey":         func() error { _, err := s.EnsureKey(cancelled, plain, "other", nil, KeyOptions{}); return err },
		"List":              func() error { _, err := s.List(cancelled); return err },
		"ListTeams":         func() error { _, err := s.ListTeams(cancelled); return err },
		"GetTeam":           func() error { _, _, err := s.GetTeam(cancelled, "team"); return err },
		"UpsertTeam":        func() error { return s.UpsertTeam(cancelled, TeamRecord{Name: "team"}) },
		"DeleteTeam":        func() error { return s.DeleteTeam(cancelled, "team") },
		"Revoke":            func() error { return s.Revoke(cancelled, p.KeyID) },
		"RevokeSnapshot":    func() error { return s.RevokeSnapshot(cancelled, p) },
		"Seed":              func() error { return s.Seed(cancelled, nil, nil) },
		"ImportSQLite":      func() error { _, err := s.ImportSQLite(cancelled, path); return err },
		"ReadPrincipal":     func() error { _, err := ReadPostgresPrincipal(cancelled, tx, p.KeyID, time.Now()); return err },
	}
	for name, operation := range tests {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			if err := operation(); !errors.Is(err, ErrStoreUnavailable) {
				t.Fatalf("cancelled operation must fail closed: %v", err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("cancelled operation was not promptly bounded")
			}
		})
	}
}
