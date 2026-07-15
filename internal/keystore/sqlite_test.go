package keystore

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateResolveRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	plaintext, p, err := s.Create(ctx, "platform-eng", []string{"claude-sonnet-4-6"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plaintext, "ik_") {
		t.Fatalf("key should be prefixed ik_: %q", plaintext)
	}
	if p.Team != "platform-eng" || len(p.AllowedModels) != 1 {
		t.Fatalf("principal: %+v", p)
	}
	got, err := s.Resolve(ctx, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyID != p.KeyID || got.Team != "platform-eng" {
		t.Fatalf("resolve mismatch: %+v vs %+v", got, p)
	}
}

func TestResolveUnknownKeyErrors(t *testing.T) {
	s := openTest(t)
	if _, err := s.Resolve(context.Background(), "ik_does_not_exist"); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestRevokeInvalidatesKey(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	plaintext, p, _ := s.Create(ctx, "t", []string{"*"})
	if err := s.Revoke(ctx, p.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, plaintext); err == nil {
		t.Fatal("revoked key must not resolve")
	}
}

func TestPlaintextNeverStored(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	plaintext, _, _ := s.Create(ctx, "t", []string{"*"})
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if strings.Contains(p.KeyID, plaintext) {
			t.Fatal("plaintext leaked into key_id")
		}
	}
}

func TestAllowsModel(t *testing.T) {
	wild := Principal{AllowedModels: []string{"*"}}
	if !wild.Allows("anything") {
		t.Fatal("* must allow all")
	}
	limited := Principal{AllowedModels: []string{"a", "b"}}
	if !limited.Allows("a") || limited.Allows("c") {
		t.Fatal("explicit allow-list wrong")
	}
}

func TestCreateWithOptions_roundTripAndExpiry(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	future := time.Now().UTC().Add(time.Hour)
	plaintext, p, err := s.CreateWithOptions(ctx, "platform-eng", []string{"*"}, KeyOptions{
		BudgetUSDMicros: 5_000_000,
		TPM:             1000,
		RPM:             60,
		ExpiresAt:       &future,
		Owner:           "alice",
		Metadata:        map[string]string{"purpose": "ci"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Resolve(ctx, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if got.BudgetUSDMicros != 5_000_000 || got.TPM != 1000 || got.RPM != 60 || got.Owner != "alice" {
		t.Fatalf("options not round-tripped: %+v", got)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(future) {
		t.Fatalf("expires_at not round-tripped: %+v want %v", got.ExpiresAt, future)
	}
	if got.Metadata["purpose"] != "ci" {
		t.Fatalf("metadata not round-tripped: %+v", got.Metadata)
	}
	if len(p.AllowedModels) != 1 {
		t.Fatalf("Create's own return value wrong: %+v", p)
	}
}

func TestCreateWithOptions_expiredKeyDoesNotResolve(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Hour)
	plaintext, _, err := s.CreateWithOptions(ctx, "t", []string{"*"}, KeyOptions{ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, plaintext); err == nil {
		t.Fatal("expired key must not resolve")
	}
}

func TestCreateWithOptions_zeroOptionsMeansUnlimited(t *testing.T) {
	s := openTest(t)
	plaintext, _, _ := s.CreateWithOptions(context.Background(), "t", []string{"*"}, KeyOptions{})
	got, err := s.Resolve(context.Background(), plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if got.BudgetUSDMicros != 0 || got.ExpiresAt != nil {
		t.Fatalf("zero options should mean unlimited/never: %+v", got)
	}
}

// TestMigration_addsColumnsToPreExistingSchema proves a keystore.db created
// before this feature (5-column keys table) still opens and accepts the new
// columns — ALTER TABLE ADD COLUMN, not CREATE TABLE IF NOT EXISTS, since the
// table already exists in deployed databases.
func TestMigration_addsColumnsToPreExistingSchema(t *testing.T) {
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
		t.Fatalf("OpenSQLite on pre-existing old-schema db: %v", err)
	}
	defer s.Close()
	plaintext, _, err := s.CreateWithOptions(context.Background(), "t", []string{"*"}, KeyOptions{Owner: "bob"})
	if err != nil {
		t.Fatalf("create after migration: %v", err)
	}
	got, err := s.Resolve(context.Background(), plaintext)
	if err != nil || got.Owner != "bob" {
		t.Fatalf("resolve after migration: %v %+v", err, got)
	}
}

// TestResolve_corruptExpiryFailsClosed pins the security-relevant fix: a
// corrupt/unparseable expires_at must NOT resolve as "never expires"
// (fail-open on an auth control) — it must fail closed.
func TestResolve_corruptExpiryFailsClosed(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	plaintext, p, err := s.CreateWithOptions(ctx, "t", []string{"*"}, KeyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE keys SET expires_at = ? WHERE key_id = ?`, "not-a-timestamp", p.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, plaintext); err == nil {
		t.Fatal("corrupt expires_at must fail closed, not resolve as never-expiring")
	}
}

// TestMigration_concurrentOpensDoNotRace simulates two separate processes
// (two independent *sql.DB handles, as two inferplane pods mid rolling-
// restart would have) opening the SAME pre-existing-schema keystore file at
// the same time. Without an atomic (BEGIN EXCLUSIVE) migration, one side can
// read the old column list, then lose a race to ALTER TABLE ADD COLUMN after
// the other side already added it, and OpenSQLite would fail with "duplicate
// column name".
func TestMigration_concurrentOpensDoNotRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
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

	const n = 3 // realistic worst case: 2-3 pods briefly overlapping during a rolling restart
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			s, err := OpenSQLite(path)
			if err != nil {
				errs <- err
				return
			}
			defer s.Close()
			errs <- nil
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent OpenSQLite failed (migration race): %v", err)
		}
	}
}

// ADR-023: declarative virtual keys. EnsureKey upserts a caller-supplied
// plaintext (unlike Create/CreateWithOptions, which generate a random one) so
// config-declared keys survive a wiped-and-recreated store across restarts.

func TestEnsureKey_createsAndResolves(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const plaintext = "sk-declarative-key-0123456789"

	p1, err := s.EnsureKey(ctx, plaintext, "platform-eng", []string{"*"}, KeyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Resolve(ctx, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if got.Team != "platform-eng" || got.KeyID != p1.KeyID {
		t.Fatalf("resolve mismatch: %+v vs %+v", got, p1)
	}

	// Ensuring again with the same plaintext must yield the same keyID (stable
	// across restarts — the whole point of ADR-023).
	p2, err := s.EnsureKey(ctx, plaintext, "platform-eng", []string{"*"}, KeyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p2.KeyID != p1.KeyID {
		t.Fatalf("keyID not stable across EnsureKey calls: %q vs %q", p1.KeyID, p2.KeyID)
	}
}

func TestEnsureKey_updatesFieldsPreservesCreatedAt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const plaintext = "sk-declarative-key-0123456789"

	if _, err := s.EnsureKey(ctx, plaintext, "team-a", []string{"model-a"}, KeyOptions{RPM: 10}); err != nil {
		t.Fatal(err)
	}
	var createdAt string
	if err := s.db.QueryRowContext(ctx, `SELECT created_at FROM keys WHERE key_hash = ?`, hashKey(plaintext)).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Millisecond)
	if _, err := s.EnsureKey(ctx, plaintext, "team-b", []string{"model-b"}, KeyOptions{RPM: 20}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Resolve(ctx, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if got.Team != "team-b" || got.RPM != 20 || len(got.AllowedModels) != 1 || got.AllowedModels[0] != "model-b" {
		t.Fatalf("fields not updated on re-Ensure: %+v", got)
	}
	var createdAt2 string
	if err := s.db.QueryRowContext(ctx, `SELECT created_at FROM keys WHERE key_hash = ?`, hashKey(plaintext)).Scan(&createdAt2); err != nil {
		t.Fatal(err)
	}
	if createdAt2 != createdAt {
		t.Fatalf("created_at changed on re-Ensure: %q -> %q", createdAt, createdAt2)
	}
}

func TestEnsureKey_doesNotResurrectRevoked(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const plaintext = "sk-declarative-key-0123456789"

	p, err := s.EnsureKey(ctx, plaintext, "team-a", []string{"*"}, KeyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, p.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureKey(ctx, plaintext, "team-a", []string{"*"}, KeyOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, plaintext); err == nil {
		t.Fatal("a revoked key must not resolve again after a re-Ensure of the same plaintext (revocation wins; the row still exists)")
	}
}

func TestEnsureKey_plaintextNeverStored(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const plaintext = "sk-declarative-key-0123456789"

	if _, err := s.EnsureKey(ctx, plaintext, "t", []string{"*"}, KeyOptions{}); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if strings.Contains(p.KeyID, plaintext) {
			t.Fatal("plaintext leaked into key_id")
		}
	}
}

var _ KeyEnsurer = (*SQLiteStore)(nil)
