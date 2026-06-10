package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

func TestDataMuxRoutesAndAuths(t *testing.T) {
	provs := map[string]providers.Provider{"p": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "p", Model: "claude-sonnet-4-6"}}},
	}
	r := router.New(provs, models)
	store := stubStore{key: "dev-key", p: keystore.Principal{KeyID: "ik_abc", Team: "platform-eng", AllowedModels: []string{"*"}}}
	mux := DataMux(r, store, nil, nil)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("unauth /v1/models = %d, want 401", rec.Code)
	}

	req2 := httptest.NewRequest("GET", "/v1/models", nil)
	req2.Header.Set("x-api-key", "dev-key")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("auth /v1/models = %d, want 200", rec2.Code)
	}
}

func TestAdminMuxHealthz(t *testing.T) {
	store := stubStore{}
	mux := AdminMux(store, []string{"admin-tok"})
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/healthz = %d", rec.Code)
	}
	_ = http.StatusOK
}

func TestAdminMuxKeysRequiresToken(t *testing.T) {
	store, err := keystore.OpenSQLite(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	mux := AdminMux(store, []string{"admin-tok"})

	// no admin token → 401
	req := httptest.NewRequest("GET", "/admin/keys", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("/admin/keys without token = %d, want 401", rec.Code)
	}

	// valid admin token → 200
	req2 := httptest.NewRequest("POST", "/admin/keys", strings.NewReader(`{"team":"t","allowed_models":["*"]}`))
	req2.Header.Set("Authorization", "Bearer admin-tok")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("/admin/keys with token = %d, want 200: %s", rec2.Code, rec2.Body.String())
	}
}
