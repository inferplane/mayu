package keystore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
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
