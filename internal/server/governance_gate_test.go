package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server/configapi"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

// TestDataMuxGovernanceGate (review/fable5 §08 B2/B3): while the require_sync
// gate is not ready, governed routes 503 (inside KeyAuth — an unauthenticated
// caller still sees 401), count_tokens and /v1/models stay unaffected, and
// flipping the gate to ready restores service.
func TestDataMuxGovernanceGate(t *testing.T) {
	provs := map[string]providers.Provider{"p": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "p", Model: "claude-sonnet-4-6"}}},
	}
	holder := newHolder(provs, models)
	r := router.New(holder)
	store := stubStore{key: "dev-key", p: keystore.Principal{KeyID: "ik_abc", Team: "t", AllowedModels: []string{"*"}}}
	ready := false
	gate := func() (bool, string) {
		if ready {
			return true, ""
		}
		return false, "no policy generation received from the control plane yet"
	}
	mux := DataMux(r, holder, store, nil, nil, nil, nil, nil, nil, nil, adminauth.MappingConfig{}, nil, 0, WithGovernanceGate(gate))

	body := `{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	do := func(method, path string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth {
			req.Header.Set("x-api-key", "dev-key")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("POST", "/v1/messages", false); rec.Code != 401 {
		t.Fatalf("unauthenticated must still be 401 (gate sits inside KeyAuth), got %d", rec.Code)
	}
	if rec := do("POST", "/v1/messages", true); rec.Code != 503 || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "governance not ready") {
		t.Fatalf("governed route while not ready = %d %q, want 503 + Retry-After", rec.Code, rec.Body.String())
	}
	if rec := do("POST", "/v1/messages/count_tokens", true); rec.Code != 200 {
		t.Fatalf("count_tokens must be exempt from the gate: %d", rec.Code)
	}
	if rec := do("POST", "/model/claude-sonnet-4-6/count-tokens", true); rec.Code != 200 {
		t.Fatalf("Bedrock count-tokens must be exempt from the gate: %d", rec.Code)
	}
	if rec := do("GET", "/v1/models", true); rec.Code != 200 {
		t.Fatalf("/v1/models must be exempt from the gate: %d", rec.Code)
	}
	ready = true
	if rec := do("POST", "/v1/messages", true); rec.Code != 200 {
		t.Fatalf("governed route once ready = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// TestAdminMuxReadyzReflectsGate: /readyz is 503 while the gate is not ready
// (so a scale-out during a control-plane outage fails health checks), 200
// otherwise, and 200 when no gate is wired at all (nil = today's posture).
func TestAdminMuxReadyzReflectsGate(t *testing.T) {
	notReady := func() (bool, string) { return false, "no policy generation received" }
	ready := func() (bool, string) { return true, "" }
	for _, tc := range []struct {
		name string
		gate func() (bool, string)
		want int
	}{{"not ready", notReady, 503}, {"ready", ready, 200}, {"no gate", nil, 200}} {
		t.Run(tc.name, func(t *testing.T) {
			mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} },
				nil, nil, nil, tc.gate, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
			if rec.Code != tc.want {
				t.Fatalf("/readyz = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
