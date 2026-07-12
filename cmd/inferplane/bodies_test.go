package main

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/bodystore"
)

func hexKey(seed byte) string {
	var k [32]byte
	for i := range k {
		k[i] = seed
	}
	return hex.EncodeToString(k[:])
}

// TestBodiesRewrapKey_RoundTrip seeds a real encrypted row via the exported
// Recorder API (production entrypoint, no bodystore internals reached from
// this package), runs the CLI, then black-box-verifies the rewrap through the
// same exported API: Fetch with the new key succeeds, Fetch with the old key
// now fails.
func TestBodiesRewrapKey_RoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bodies.db")
	store, err := bodystore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	var oldMaster, newMaster [32]byte
	for i := range oldMaster {
		oldMaster[i], newMaster[i] = 1, 2
	}

	rec := bodystore.NewRecorder(store, oldMaster, time.Hour, 1<<20)
	ref := rec.Capture("rec-1", "acme", []byte("request body"), []byte("response body"))
	if ref == "" {
		t.Fatal("Capture dropped the body")
	}
	rec.Close()
	store.Close()

	t.Setenv("BODIES_TEST_OLD_KEY", hexKey(1))
	t.Setenv("BODIES_TEST_NEW_KEY", hexKey(2))

	code := bodiesCmd([]string{"rewrap-key", "--store", dbPath, "--old-key-env", "BODIES_TEST_OLD_KEY", "--new-key-env", "BODIES_TEST_NEW_KEY"})
	if code != 0 {
		t.Fatalf("bodiesCmd rewrap-key exit = %d, want 0", code)
	}

	store2, err := bodystore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	newRec := bodystore.NewRecorder(store2, newMaster, time.Hour, 1<<20)
	body, err := newRec.Fetch(context.Background(), ref)
	if err != nil {
		t.Fatalf("Fetch with the NEW master key after rewrap: %v", err)
	}
	if string(body.Request) != "request body" || string(body.Response) != "response body" {
		t.Fatalf("rewrap changed the underlying data: %+v", body)
	}

	oldRec := bodystore.NewRecorder(store2, oldMaster, time.Hour, 1<<20)
	if _, err := oldRec.Fetch(context.Background(), ref); err == nil {
		t.Fatal("Fetch with the OLD master key must fail after rewrap")
	}
}

// TestBodiesRewrapKey_WrongOldKeyFailsClosed pins the plan-gate fail-open fix:
// a wrong --old-key-env must NOT report success. Exit code 1 (nothing matched
// the given old key), and the row is left byte-for-byte untouched.
func TestBodiesRewrapKey_WrongOldKeyFailsClosed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bodies.db")
	store, err := bodystore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	var realOld [32]byte
	for i := range realOld {
		realOld[i] = 1
	}
	rec := bodystore.NewRecorder(store, realOld, time.Hour, 1<<20)
	ref := rec.Capture("rec-1", "acme", []byte("request body"), []byte("response body"))
	if ref == "" {
		t.Fatal("Capture dropped the body")
	}
	rec.Close()
	store.Close()

	t.Setenv("BODIES_TEST_WRONG_OLD_KEY", hexKey(9)) // never wrapped anything
	t.Setenv("BODIES_TEST_NEW_KEY2", hexKey(2))

	code := bodiesCmd([]string{"rewrap-key", "--store", dbPath, "--old-key-env", "BODIES_TEST_WRONG_OLD_KEY", "--new-key-env", "BODIES_TEST_NEW_KEY2"})
	if code != 1 {
		t.Fatalf("bodiesCmd rewrap-key with a wrong old key, exit = %d, want 1", code)
	}

	store2, err := bodystore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	realOldRec := bodystore.NewRecorder(store2, realOld, time.Hour, 1<<20)
	if _, err := realOldRec.Fetch(context.Background(), ref); err != nil {
		t.Fatalf("row must be left untouched (correctly skipped): %v", err)
	}
}

func TestBodiesCmd_UnknownVerbUsage(t *testing.T) {
	if code := bodiesCmd([]string{"bogus"}); code != 2 {
		t.Fatalf("unknown verb exit = %d, want 2", code)
	}
	if code := bodiesCmd(nil); code != 2 {
		t.Fatalf("missing verb exit = %d, want 2", code)
	}
}

func TestBodiesRewrapKey_MissingFlagsUsage(t *testing.T) {
	if code := bodiesCmd([]string{"rewrap-key"}); code != 2 {
		t.Fatalf("missing all flags exit = %d, want 2", code)
	}
}
