package adminapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/keystore"
)

func newTestStore(t *testing.T) *keystore.SQLiteStore {
	s, err := keystore.OpenSQLite(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateKeyReturnsPlaintextOnce(t *testing.T) {
	h := NewKeysHandler(newTestStore(t))
	req := httptest.NewRequest("POST", "/admin/keys", strings.NewReader(`{"team":"platform-eng","allowed_models":["claude-sonnet-4-6"]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		KeyID     string `json:"key_id"`
		Plaintext string `json:"plaintext"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.HasPrefix(out.Plaintext, "ik_") || out.KeyID == "" {
		t.Fatalf("expected plaintext+key_id: %+v", out)
	}
}

func TestListKeysOmitsSecrets(t *testing.T) {
	store := newTestStore(t)
	store.Create(context.Background(), "t", []string{"*"})
	h := NewKeysHandler(store)
	req := httptest.NewRequest("GET", "/admin/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "plaintext") {
		t.Fatalf("list must not expose plaintext: %s", rec.Body.String())
	}
}

func TestRevokeKey(t *testing.T) {
	store := newTestStore(t)
	_, p, _ := store.Create(context.Background(), "t", []string{"*"})
	h := NewKeysHandler(store)
	req := httptest.NewRequest("DELETE", "/admin/keys/"+p.KeyID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("revoke: %d", rec.Code)
	}
}
