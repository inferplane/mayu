package keystore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Every integration test owns a random schema, including its cleanup. The
// opt-in DSN is never printed and default package tests need no database.
func postgresTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("KEYSTORE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("KEYSTORE_TEST_POSTGRES_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("cannot connect to disposable Postgres")
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	name := "keystore_test_" + hex.EncodeToString(random[:])
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		_ = conn.Close(ctx)
		t.Fatal("cannot create isolated schema")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(ctx, "DROP SCHEMA "+ident+" CASCADE"); err != nil {
			t.Error("cannot remove isolated schema")
		}
		_ = conn.Close(ctx)
	})
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid test DSN")
		}
		q := u.Query()
		q.Set("search_path", name)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " search_path=" + name
}

func openPostgresTest(t *testing.T, dsn string) *PostgresStore {
	t.Helper()
	s, err := OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func execPostgresTest(t *testing.T, s *PostgresStore, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, query, args...); err != nil {
		t.Fatal("fixture SQL failed")
	}
}

func completeTeam() TeamRecord {
	return TeamRecord{
		Name: "team-a", AllowedModels: []string{"model-a", "model-b"},
		RPM: 4_000_000_001, TPM: 4_000_000_002, TokensPerDay: 4_000_000_003,
		QuotaOnExceeded: "block", BudgetUSDMicros: 9_223_372_036_854_775_807,
		BudgetUSDMicrosPerDay: 4_000_000_005, BudgetOnExceeded: "warn",
		GuardrailID: "guardrail-a", GuardrailVersion: "9", AllowedRegions: []string{"eu", "us"},
	}
}

func completeKeyOptions() KeyOptions {
	expires := time.Date(2099, 4, 5, 6, 7, 8, 901, time.UTC)
	return KeyOptions{
		BudgetUSDMicros: 9_007_199_254_740_993, BudgetUSDMicrosPerDay: 8_000_000_002,
		TPM: 8_000_000_003, RPM: 8_000_000_004, ExpiresAt: &expires,
		Owner: "opaque-owner", Metadata: map[string]string{"purpose": "test", "tag": "value"},
	}
}

func TestPostgresIndependentPoolsPreserveAllFields(t *testing.T) {
	dsn := postgresTestDSN(t)
	a, b := openPostgresTest(t, dsn), openPostgresTest(t, dsn)
	ctx := context.Background()
	team, opts := completeTeam(), completeKeyOptions()
	if err := a.UpsertTeam(ctx, team); err != nil {
		t.Fatal(err)
	}
	plain, created, err := a.CreateWithOptions(ctx, team.Name, []string{"model-b"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Resolve(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyID != created.KeyID || got.Team != "team-a" ||
		!reflect.DeepEqual(got.AllowedModels, []string{"model-b"}) ||
		!reflect.DeepEqual(got.KeyOptions, opts) {
		t.Fatal("key fields failed to round trip")
	}
	storedTeam, ok, err := b.GetTeam(ctx, "team-a")
	if err != nil || !ok {
		t.Fatalf("team missing: %v", err)
	}
	team.CreatedAt, team.UpdatedAt = storedTeam.CreatedAt, storedTeam.UpdatedAt
	if team.CreatedAt == "" || team.UpdatedAt == "" || !reflect.DeepEqual(storedTeam, team) {
		t.Fatal("team fields failed to round trip")
	}
	if !got.TeamSnapshotLoaded || !reflect.DeepEqual(got.TeamSnapshot, &storedTeam) ||
		got.SharedRevision == "" || got.SharedRevision != created.SharedRevision {
		t.Fatal("atomic snapshot missing or inconsistent")
	}
	again, err := a.Resolve(ctx, plain)
	if err != nil || again.SharedRevision != got.SharedRevision {
		t.Fatal("revision changed without a row change")
	}
	list, err := b.List(ctx)
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0], got) {
		t.Fatal("List must carry the same authorization snapshot")
	}
	teams, err := b.ListTeams(ctx)
	if err != nil || len(teams) != 1 || !reflect.DeepEqual(teams[0], storedTeam) {
		t.Fatal("ListTeams mismatch")
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"SharedRevision", "TeamSnapshot", "TeamSnapshotLoaded", got.SharedRevision, "guardrail-a"} {
		if strings.Contains(string(wire), hidden) {
			t.Fatal("internal authorization snapshot leaked into JSON")
		}
	}
	if err := b.RevokeSnapshot(ctx, list[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Resolve(ctx, plain); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("cross-pool revoke did not take effect: %v", err)
	}
	if err := a.Ready(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresConcurrentSchemaInitialization(t *testing.T) {
	dsn := postgresTestDSN(t)
	const count = 6
	start := make(chan struct{})
	errs := make(chan error, count)
	for range count {
		go func() {
			<-start
			s, err := OpenPostgres(context.Background(), dsn)
			if err == nil {
				err = s.Close()
			}
			errs <- err
		}()
	}
	close(start)
	for range count {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	s := openPostgresTest(t, dsn)
	if _, _, err := s.CreateWithOptions(context.Background(), "team", nil, completeKeyOptions()); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresEnsureKeyParityPreservesRevocation(t *testing.T) {
	s := openPostgresTest(t, postgresTestDSN(t))
	ctx := context.Background()
	const plain = "fixture-key-ensure"
	p, err := s.EnsureKey(ctx, plain, "before", []string{"a"}, KeyOptions{RPM: 1})
	if err != nil {
		t.Fatal(err)
	}
	var created string
	if err := s.db.QueryRow(ctx, `SELECT created_at FROM keys WHERE key_id=$1`, p.KeyID).Scan(&created); err != nil {
		t.Fatal("read created_at")
	}
	if err := s.Revoke(ctx, p.KeyID); err != nil {
		t.Fatal(err)
	}
	updated, err := s.EnsureKey(ctx, plain, "after", []string{"b"}, completeKeyOptions())
	if err != nil || updated.KeyID != p.KeyID || updated.Team != "after" || updated.RPM != 8_000_000_004 {
		t.Fatalf("EnsureKey upsert failed: %v", err)
	}
	var after string
	var revoked int
	if err := s.db.QueryRow(ctx, `SELECT created_at, revoked FROM keys WHERE key_id=$1`, p.KeyID).Scan(&after, &revoked); err != nil {
		t.Fatal("read tombstone")
	}
	if after != created || revoked != 1 {
		t.Fatal("upsert changed identity creation time or revived tombstone")
	}
	if _, err := s.Resolve(ctx, plain); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("revoked key resolved: %v", err)
	}
	if err := s.Revoke(ctx, p.KeyID); err != nil {
		t.Fatal("unconditional revoke must remain idempotent")
	}
	if err := s.Revoke(ctx, "absent"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("missing revoke must return ErrKeyNotFound")
	}
}

func TestPostgresReadPrincipalUsesCallerTransactionAndTime(t *testing.T) {
	s := openPostgresTest(t, postgresTestDSN(t))
	ctx := context.Background()
	future := time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC)
	plain, created, err := s.CreateWithOptions(ctx, "missing-team", nil, KeyOptions{ExpiresAt: &future})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Resolve(ctx, plain)
	if err != nil || !resolved.TeamSnapshotLoaded || resolved.TeamSnapshot != nil {
		t.Fatal("absent team is a loaded nil snapshot")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal("begin caller transaction")
	}
	defer tx.Rollback(ctx)
	p, err := ReadPostgresPrincipal(ctx, tx, created.KeyID, future.Add(-time.Nanosecond))
	if err != nil || p.SharedRevision != resolved.SharedRevision {
		t.Fatal("caller-time read disagrees with Resolve")
	}
	if _, err := ReadPostgresPrincipal(ctx, tx, created.KeyID, future); !errors.Is(err, ErrKeyExpired) {
		t.Fatalf("expiry boundary accepted: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE keys SET owner='transaction-owner' WHERE key_id=$1`, p.KeyID); err != nil {
		t.Fatal("update inside caller tx")
	}
	changed, err := ReadPostgresPrincipal(ctx, tx, p.KeyID, future.Add(-time.Second))
	if err != nil || changed.Owner != "transaction-owner" || changed.SharedRevision == p.SharedRevision {
		t.Fatal("helper did not consume caller transaction")
	}
	if _, err := tx.Exec(ctx, `UPDATE keys SET revoked=1 WHERE key_id=$1`, p.KeyID); err != nil {
		t.Fatal("revoke in caller tx")
	}
	if _, err := ReadPostgresPrincipal(ctx, tx, p.KeyID, future.Add(-time.Second)); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("helper accepted a revoked identity")
	}
	if _, err := ReadPostgresPrincipal(ctx, tx, "absent", future); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("helper accepted missing key")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal("helper ended its caller's transaction")
	}
	if _, err := s.Resolve(ctx, plain); err != nil {
		t.Fatal("caller rollback was not preserved")
	}
}

func TestPostgresExpiredListAndCompareAndSet(t *testing.T) {
	dsn := postgresTestDSN(t)
	a, b := openPostgresTest(t, dsn), openPostgresTest(t, dsn)
	ctx := context.Background()
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	plain, p, err := a.CreateWithOptions(ctx, "team", nil, KeyOptions{ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Resolve(ctx, plain); !errors.Is(err, ErrKeyExpired) {
		t.Fatal("expired key resolved")
	}
	list, err := a.List(ctx)
	if err != nil || len(list) != 1 || list[0].SharedRevision == "" {
		t.Fatal("expired keys must remain listable with revision")
	}
	execPostgresTest(t, b, `UPDATE keys SET team='other' WHERE key_id=$1`, p.KeyID)
	if err := a.RevokeSnapshot(ctx, list[0]); !errors.Is(err, ErrSnapshotChanged) {
		t.Fatal("team transfer bypassed conditional revoke")
	}
	list, err = b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.UpsertTeam(ctx, TeamRecord{Name: "other", RPM: 1}); err != nil {
		t.Fatal(err)
	}
	if err := b.RevokeSnapshot(ctx, list[0]); !errors.Is(err, ErrSnapshotChanged) {
		t.Fatal("new team policy bypassed conditional revoke")
	}
	list, err = a.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.RevokeSnapshot(ctx, Principal{KeyID: p.KeyID}); !errors.Is(err, ErrSnapshotChanged) {
		t.Fatal("unversioned revoke allowed")
	}
	if err := b.RevokeSnapshot(ctx, list[0]); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresEveryAuthorizationFieldChangesRevision(t *testing.T) {
	s := openPostgresTest(t, postgresTestDSN(t))
	ctx := context.Background()
	if err := s.UpsertTeam(ctx, completeTeam()); err != nil {
		t.Fatal(err)
	}
	plain, p, err := s.CreateWithOptions(ctx, "team-a", []string{"a"}, completeKeyOptions())
	if err != nil {
		t.Fatal(err)
	}
	changes := []string{
		`UPDATE keys SET allowed_models='b'`, `UPDATE keys SET budget_usd_micros=7`,
		`UPDATE keys SET budget_usd_micros_per_day=8`, `UPDATE keys SET tpm=9`,
		`UPDATE keys SET rpm=10`, `UPDATE keys SET expires_at='2098-01-01T00:00:00Z'`,
		`UPDATE keys SET owner='changed'`, `UPDATE keys SET metadata='{"new":"value"}'`,
		`UPDATE teams SET allowed_models='b'`, `UPDATE teams SET rpm=11`,
		`UPDATE teams SET tpm=12`, `UPDATE teams SET tokens_per_day=13`,
		`UPDATE teams SET quota_on_exceeded='warn'`, `UPDATE teams SET budget_usd_micros=14`,
		`UPDATE teams SET budget_on_exceeded='block'`, `UPDATE teams SET budget_usd_micros_per_day=15`,
		`UPDATE teams SET guardrail_id='other'`, `UPDATE teams SET guardrail_version='10'`,
		`UPDATE teams SET allowed_regions='asia'`, `DELETE FROM teams`, `UPDATE keys SET team='new-team'`,
	}
	for _, query := range changes {
		execPostgresTest(t, s, query)
		next, err := s.Resolve(ctx, plain)
		if err != nil || next.SharedRevision == p.SharedRevision {
			t.Fatalf("revision did not change for %s: %v", query, err)
		}
		if err := s.RevokeSnapshot(ctx, p); !errors.Is(err, ErrSnapshotChanged) {
			t.Fatalf("stale snapshot accepted after %s", query)
		}
		p = next
	}
}

func TestPostgresSeedImmutableAtomicAndConcurrent(t *testing.T) {
	dsn := postgresTestDSN(t)
	a, b := openPostgresTest(t, dsn), openPostgresTest(t, dsn)
	ctx := context.Background()
	team := completeTeam()
	key := SeedKey{Plaintext: "fixture-seed", Team: team.Name, AllowedModels: []string{"a"}, Options: completeKeyOptions()}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, s := range []*PostgresStore{a, b} {
		wg.Add(1)
		go func(s *PostgresStore) {
			defer wg.Done()
			errs <- s.Seed(ctx, []TeamRecord{team}, []SeedKey{key})
		}(s)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := a.Resolve(ctx, key.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Revoke(ctx, p.KeyID); err != nil {
		t.Fatal(err)
	}
	if err := a.Seed(ctx, []TeamRecord{team}, []SeedKey{key}); err != nil {
		t.Fatal("identical seed over tombstone must be idempotent")
	}
	if _, err := b.Resolve(ctx, key.Plaintext); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("seed revived tombstone")
	}
	changed := key
	changed.Options.RPM++
	if err := b.Seed(ctx, []TeamRecord{{Name: "must-rollback"}}, []SeedKey{changed}); !errors.Is(err, ErrSnapshotChanged) {
		t.Fatalf("conflicting seed accepted: %v", err)
	}
	if _, ok, err := a.GetTeam(ctx, "must-rollback"); err != nil || ok {
		t.Fatal("seed was not atomic")
	}
	team.RPM++
	if err := a.UpsertTeam(ctx, team); err != nil {
		t.Fatal(err)
	}
	if err := b.Seed(ctx, []TeamRecord{completeTeam()}, nil); err != nil {
		t.Fatal("original seed must remain a no-op after admin team edit")
	}
	current, ok, err := a.GetTeam(ctx, team.Name)
	if err != nil || !ok || current.RPM != team.RPM {
		t.Fatal("original seed overwrote admin team edit")
	}
	if _, err := a.EnsureKey(ctx, key.Plaintext, "admin-transfer", []string{"different"}, KeyOptions{RPM: 19}); err != nil {
		t.Fatal(err)
	}
	if err := b.Seed(ctx, []TeamRecord{completeTeam()}, []SeedKey{key}); err != nil {
		t.Fatal("original seed must remain a no-op after key edits and revocation")
	}
	var keyTeam string
	var keyRPM int64
	var revoked int
	if err := a.db.QueryRow(ctx, `SELECT team,rpm,revoked FROM keys WHERE key_id=$1`, p.KeyID).Scan(&keyTeam, &keyRPM, &revoked); err != nil {
		t.Fatal("read edited tombstone")
	}
	if keyTeam != "admin-transfer" || keyRPM != 19 || revoked != 1 {
		t.Fatal("seed changed edited tombstone")
	}
	if err := b.Seed(ctx, nil, []SeedKey{{Plaintext: key.Plaintext, Team: "admin-transfer",
		AllowedModels: []string{"different"}, Options: KeyOptions{RPM: 19}}}); !errors.Is(err, ErrSnapshotChanged) {
		t.Fatal("declaration matching admin edits bypassed original fingerprint")
	}
	if err := a.DeleteTeam(ctx, team.Name); err != nil {
		t.Fatal(err)
	}
	execPostgresTest(t, a, `DELETE FROM keys WHERE key_id=$1`, p.KeyID)
	// Reopening proves seed authority was persisted, rather than kept in the
	// original process or tied to the lifetime of the key/team row.
	reopened := openPostgresTest(t, dsn)
	if err := reopened.Seed(ctx, []TeamRecord{completeTeam()}, []SeedKey{key}); err != nil {
		t.Fatal("original seed must survive admin deletion and restart")
	}
	if _, ok, err := b.GetTeam(ctx, team.Name); err != nil || ok {
		t.Fatal("seed recreated deleted team")
	}
	if _, err := b.Resolve(ctx, key.Plaintext); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("seed recreated deleted key")
	}
	if err := reopened.Seed(ctx, []TeamRecord{team}, nil); !errors.Is(err, ErrSnapshotChanged) {
		t.Fatal("new declaration bypassed original fingerprint after deletion")
	}
}

func TestPostgresFirstSeedAttachesOnlyToMatchingSemanticRows(t *testing.T) {
	s := openPostgresTest(t, postgresTestDSN(t))
	ctx := context.Background()
	team := completeTeam()
	if err := s.UpsertTeam(ctx, team); err != nil {
		t.Fatal(err)
	}
	key := SeedKey{Plaintext: "fixture-first-seed", Team: team.Name, AllowedModels: []string{"b", "a"}, Options: completeKeyOptions()}
	p, err := s.EnsureKey(ctx, key.Plaintext, key.Team, []string{"a", "b"}, key.Options)
	if err != nil {
		t.Fatal(err)
	}
	// Semantically equivalent encodings must not prevent first attachment.
	execPostgresTest(t, s, `UPDATE keys SET metadata='{ "tag": "value", "purpose": "test" }',
expires_at='2099-04-05T07:07:08.000000901+01:00'`)
	if err := s.Revoke(ctx, p.KeyID); err != nil {
		t.Fatal(err)
	}
	reordered := team
	reordered.AllowedModels = []string{"model-b", "model-a"}
	reordered.AllowedRegions = []string{"us", "eu"}
	if err := s.Seed(ctx, []TeamRecord{reordered}, []SeedKey{key}); err != nil {
		t.Fatalf("matching preexisting data should acquire original fingerprint: %v", err)
	}
	if _, err := s.Resolve(ctx, key.Plaintext); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("first seed revived preexisting tombstone")
	}
	// A rejected first batch must not leave a marker for its earlier rows.
	if _, err := s.EnsureKey(ctx, "fixture-first-conflict", "admin-team", nil, KeyOptions{}); err != nil {
		t.Fatal(err)
	}
	conflict := SeedKey{Plaintext: "fixture-first-conflict", Team: "declared-team"}
	if err := s.Seed(ctx, []TeamRecord{{Name: "batch-team", RPM: 1}}, []SeedKey{conflict}); !errors.Is(err, ErrSnapshotChanged) {
		t.Fatal("first seed attached to conflicting admin data")
	}
	if _, ok, err := s.GetTeam(ctx, "batch-team"); err != nil || ok {
		t.Fatal("first seed conflict left partial rows")
	}
	if err := s.Seed(ctx, []TeamRecord{{Name: "batch-team", RPM: 2}}, nil); err != nil {
		t.Fatal("failed first seed retained a fingerprint")
	}
}

func TestPostgresBoundedAndRedactedFailures(t *testing.T) {
	const secret = "private-test-value"
	if _, err := OpenPostgres(context.Background(), "postgres://"+secret+":%zz@invalid"); !errors.Is(err, ErrStoreUnavailable) || strings.Contains(fmt.Sprint(err), secret) {
		t.Fatal("parse error did not redact DSN")
	}
	s := openPostgresTest(t, postgresTestDSN(t))
	ctx := context.Background()
	plain, p, err := s.Create(ctx, "team", nil)
	if err != nil {
		t.Fatal(err)
	}
	execPostgresTest(t, s, `UPDATE keys SET expires_at=$1`, secret)
	if _, err := s.Resolve(ctx, plain); !errors.Is(err, ErrStoreUnavailable) || strings.Contains(fmt.Sprint(err), secret) {
		t.Fatal("corrupt expiry not redacted/fail-closed")
	}
	execPostgresTest(t, s, `UPDATE keys SET expires_at=''`)
	execPostgresTest(t, s, `CREATE FUNCTION reject_key_write() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'private-test-value %', NEW.key_hash; END $$;
CREATE TRIGGER reject_key_write BEFORE UPDATE ON keys FOR EACH ROW EXECUTE FUNCTION reject_key_write()`)
	err = s.Revoke(ctx, p.KeyID)
	if !errors.Is(err, ErrStoreUnavailable) || strings.Contains(fmt.Sprintf("%+v", err), secret) || strings.Contains(fmt.Sprint(err), hashKey(plain)) {
		t.Fatal("database exception details escaped")
	}
	execPostgresTest(t, s, `DROP TRIGGER reject_key_write ON keys`)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal("begin lock")
	}
	defer tx.Rollback(ctx)
	// This connection deliberately holds a lock longer than normal operations.
	// Keep the fixture alive so it measures the reader's bound, not eviction of
	// the blocking transaction by the store's idle-transaction guard.
	if _, err := tx.Exec(ctx, `SET LOCAL idle_in_transaction_session_timeout=0`); err != nil {
		t.Fatal("configure lock fixture")
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE keys IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal("lock keys")
	}
	start := time.Now()
	if _, err := s.Resolve(ctx, plain); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatal("blocked resolve did not fail closed")
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("unbounded resolve: %v", elapsed)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal("unlock keys")
	}
	short, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Ready(short); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatal("Ready ignored cancellation")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, plain); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatal("closed store authorized request")
	}
}
