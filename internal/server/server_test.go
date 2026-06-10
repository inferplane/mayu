package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
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
	mux := DataMux(r, "dev-key")

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
	mux := AdminMux()
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/healthz = %d", rec.Code)
	}
	_ = http.StatusOK
}
