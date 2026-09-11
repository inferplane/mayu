package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

// TestDataMuxBoundsRequestBody (C9): every KeyAuth-guarded body is bounded —
// an oversized generation request is refused with a 4xx, count_tokens stays
// 200 with the local estimate (the never-non-200 invariant), and ordinary
// bodies under the default limit are untouched.
func TestDataMuxBoundsRequestBody(t *testing.T) {
	provs := map[string]providers.Provider{"p": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "p", Model: "claude-sonnet-4-6"}}},
	}
	holder := newHolder(provs, models)
	r := router.New(holder)
	store := stubStore{key: "dev-key", p: keystore.Principal{KeyID: "ik_abc", Team: "t", AllowedModels: []string{"*"}}}
	mux := DataMux(r, holder, store, nil, nil, nil, nil, nil, nil, nil, adminauth.MappingConfig{}, nil, 0,
		WithMaxRequestBytes(64))

	big := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"` + strings.Repeat("x", 256) + `"}]}`

	// Oversized generation request → 4xx (413 via the Content-Length
	// short-circuit here; an unbounded stream would surface as the ingress's
	// own read/parse 400 — either way, never served).
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(big))
	req.Header.Set("x-api-key", "dev-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("oversized /v1/messages = %d, want a 4xx", rec.Code)
	}

	// Same oversized body on count_tokens → STILL 200 with a local estimate.
	req = httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(big))
	req.Header.Set("x-api-key", "dev-key")
	req.ContentLength = -1 // undeclared length: exercise the MaxBytesReader path, not the 413 short-circuit
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "input_tokens") {
		t.Fatalf("oversized count_tokens = %d %s, want 200 with an estimate", rec.Code, rec.Body.String())
	}
}

// TestDataMuxDefaultLimitPassesOrdinaryTraffic: no option ⇒ the 64 MiB
// default applies and a normal body is unaffected.
func TestDataMuxDefaultLimitPassesOrdinaryTraffic(t *testing.T) {
	provs := map[string]providers.Provider{"p": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "p", Model: "claude-sonnet-4-6"}}},
	}
	holder := newHolder(provs, models)
	r := router.New(holder)
	store := stubStore{key: "dev-key", p: keystore.Principal{KeyID: "ik_abc", Team: "t", AllowedModels: []string{"*"}}}
	mux := DataMux(r, holder, store, nil, nil, nil, nil, nil, nil, nil, adminauth.MappingConfig{}, nil, 0)

	body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("x-api-key", "dev-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("ordinary count_tokens under the default limit = %d, want 200", rec.Code)
	}
}
