package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server/configapi"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

func TestDataMuxRoutesAndAuths(t *testing.T) {
	provs := map[string]providers.Provider{"p": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "p", Model: "claude-sonnet-4-6"}}},
	}
	r := router.New(newHolder(provs, models))
	store := stubStore{key: "dev-key", p: keystore.Principal{KeyID: "ik_abc", Team: "platform-eng", AllowedModels: []string{"*"}}}
	mux := DataMux(r, store, nil, nil, nil, nil)

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

func TestDataMuxModelsContentNegotiation(t *testing.T) {
	provs := map[string]providers.Provider{"p": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "p", Model: "claude-sonnet-4-6"}}},
	}
	r := router.New(newHolder(provs, models))
	store := stubStore{key: "dev-key", p: keystore.Principal{KeyID: "ik_abc", Team: "platform-eng", AllowedModels: []string{"*"}}}
	mux := DataMux(r, store, nil, nil, nil, nil)

	// OpenAI client (no anthropic-version header) → OpenAI {"object":"list"} shape.
	reqO := httptest.NewRequest("GET", "/v1/models", nil)
	reqO.Header.Set("x-api-key", "dev-key")
	recO := httptest.NewRecorder()
	mux.ServeHTTP(recO, reqO)
	if recO.Code != 200 || !strings.Contains(recO.Body.String(), `"object":"list"`) {
		t.Fatalf("openai /v1/models = %d body %s", recO.Code, recO.Body.String())
	}

	// Anthropic client (anthropic-version header) → Anthropic {"data":[...],"has_more"} shape.
	reqA := httptest.NewRequest("GET", "/v1/models", nil)
	reqA.Header.Set("x-api-key", "dev-key")
	reqA.Header.Set("anthropic-version", "2023-06-01")
	recA := httptest.NewRecorder()
	mux.ServeHTTP(recA, reqA)
	if recA.Code != 200 || !strings.Contains(recA.Body.String(), `"has_more"`) {
		t.Fatalf("anthropic /v1/models = %d body %s", recA.Code, recA.Body.String())
	}
}

func TestDataMuxChatCompletionsRoutes(t *testing.T) {
	provs := map[string]providers.Provider{"p": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "p", Model: "claude-sonnet-4-6"}}},
	}
	r := router.New(newHolder(provs, models))
	store := stubStore{key: "dev-key", p: keystore.Principal{KeyID: "ik_abc", Team: "platform-eng", AllowedModels: []string{"*"}}}
	mux := DataMux(r, store, nil, nil, nil, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "dev-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"chat.completion"`) {
		t.Fatalf("/v1/chat/completions = %d body %s", rec.Code, rec.Body.String())
	}
}

func TestAdminMuxHealthz(t *testing.T) {
	store := stubStore{}
	mux := AdminMux(store, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil)
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
	mux := AdminMux(store, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil)

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

func TestAdminMuxMetricsUnauthed(t *testing.T) {
	m := metrics.New()
	m.ObserveRequest("anthropic", "claude-sonnet-4-6", "anthropic-direct", "t", 200, 1.0, 0)
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, m, nil, nil)
	req := httptest.NewRequest("GET", "/metrics", nil) // NO auth
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/metrics should be unauthenticated 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "inferplane_requests_total") {
		t.Fatalf("/metrics missing exposition: %s", rec.Body.String())
	}
}

func TestAdminMuxServesUI(t *testing.T) {
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil)
	req := httptest.NewRequest("GET", "/admin/ui/", nil) // NO auth — data-free static page (ADR-001)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/admin/ui/ = %d, want 200 without auth", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("/admin/ui/ Content-Type = %q", ct)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("/admin/ui/ CSP = %q", csp)
	}
}

func TestAdminMuxUIRedirect(t *testing.T) {
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil)
	req := httptest.NewRequest("GET", "/admin/ui", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 301 && rec.Code != 308 {
		t.Fatalf("/admin/ui = %d, want permanent redirect to /admin/ui/", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/ui/" {
		t.Fatalf("redirect Location = %q, want /admin/ui/", loc)
	}
}

// TestAdminMuxUIDoesNotBypassKeysAuth pins the ADR-001 invariant: wiring the
// unauthenticated UI must not loosen auth on the keys API.
func TestAdminMuxUIDoesNotBypassKeysAuth(t *testing.T) {
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/admin/keys"},
		{"POST", "/admin/keys"},
		{"DELETE", "/admin/keys/ik-123"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("%s %s without token = %d, want 401 (UI wiring must not bypass auth)", tc.method, tc.path, rec.Code)
		}
	}
}

// --- OIDC wiring into AdminMux (plan 2026-06-12 task 7) ---

// TestAdminMuxOIDCWiring: with a verifier configured, both credential kinds
// reach /admin/keys, and an OIDC team-member is IMMEDIATELY subject to the
// task-6 entitlement + audit (cross-team create => 403 AND audited).
func TestAdminMuxOIDCWiring(t *testing.T) {
	store, err := keystore.OpenSQLite(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	dir := t.TempDir()
	auditFile := filepath.Join(dir, "audit.jsonl")
	fsink, err := audit.NewFileSink(auditFile, true)
	if err != nil {
		t.Fatal(err)
	}
	aud, err := audit.NewWriter("test-instance", filepath.Join(dir, "wal"), []audit.Sink{fsink})
	if err != nil {
		t.Fatal(err)
	}

	v := &fakeVerifier{claims: adminauth.Claims{Subject: "u-alpha", Groups: []string{"team-alpha"}}}
	mapping := adminauth.MappingConfig{GroupMappings: []adminauth.GroupMapping{{Group: "team-alpha", Teams: []string{"alpha"}}}}
	mux := AdminMux(store, []string{"admin-tok"}, v, mapping, func() configapi.View { return configapi.View{} }, nil, aud, nil, nil, nil)

	do := func(bearer, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// static break-glass still works
	if rec := do("admin-tok", "POST", "/admin/keys", `{"team":"any","allowed_models":["*"]}`); rec.Code != 200 {
		t.Fatalf("break-glass create = %d", rec.Code)
	}
	// OIDC member: own team OK
	if rec := do(jwtShaped, "POST", "/admin/keys", `{"team":"alpha","allowed_models":["*"]}`); rec.Code != 200 {
		t.Fatalf("oidc own-team create = %d: %s", rec.Code, rec.Body.String())
	}
	// OIDC member: cross-team 403
	if rec := do(jwtShaped, "POST", "/admin/keys", `{"team":"beta","allowed_models":["*"]}`); rec.Code != 403 {
		t.Fatalf("oidc cross-team create = %d, want 403", rec.Code)
	}

	aud.Close()
	raw, err := os.ReadFile(auditFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"event":"admin_denied"`, `"user":"u-alpha"`, `"auth_method":"oidc"`, `"auth_method":"break_glass"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("audit missing %s:\n%s", want, raw)
		}
	}
	res, err := audit.Verify(strings.NewReader(string(raw)))
	if err != nil || !res.OK {
		t.Fatalf("chain: %+v %v", res, err)
	}
}

// TestAdminMuxNilOIDCUnchanged: without a verifier the mux behaves exactly as
// the task-6 static-only state; UI/metrics/health stay unauthenticated.
func TestAdminMuxNilOIDCUnchanged(t *testing.T) {
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil)
	for path, want := range map[string]int{
		"/healthz": 200, "/readyz": 200, "/admin/ui/": 200,
	} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s = %d, want %d", path, rec.Code, want)
		}
	}
	req := httptest.NewRequest("GET", "/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer "+jwtShaped) // shaped bearer, nil verifier → static path → 401
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("shaped bearer with nil verifier = %d, want 401", rec.Code)
	}
}

// TestAdminMuxConfigEndpoint (plan C2): GET /admin/config is behind AdminAuth,
// read-only, and exposes the topology without secret values.
func TestAdminMuxConfigEndpoint(t *testing.T) {
	view := configapi.ViewFrom(map[string]config.ProviderConfig{
		"anthropic-direct": {Type: "anthropic", BaseURL: "https://api.anthropic.com", APIKeyRef: &config.SecretRef{Env: "ANTHROPIC_API_KEY"}, APIKey: "sk-LEAK"},
	}, map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "anthropic-direct", Model: "claude-sonnet-4-6"}}},
	})
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return view }, nil, nil, nil, nil, nil)

	// no token → 401
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/config", nil))
	if rec.Code != 401 {
		t.Fatalf("GET /admin/config without token = %d, want 401", rec.Code)
	}

	// with token → 200, topology present, secret absent
	req := httptest.NewRequest("GET", "/admin/config", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /admin/config = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "https://api.anthropic.com") || !strings.Contains(body, "ANTHROPIC_API_KEY") {
		t.Fatalf("config view missing topology: %s", body)
	}
	if strings.Contains(body, "sk-LEAK") {
		t.Fatalf("config endpoint leaked secret value: %s", body)
	}
}

// newHolder builds a live.Holder for router tests (no registry needed).
func newHolder(provs map[string]providers.Provider, models map[string]config.ModelConfig) *live.Holder {
	ids := make(map[string]string, len(provs))
	for n := range provs {
		ids[n] = n
	}
	h := &live.Holder{}
	h.Swap(live.NewState(provs, models, nil, ids))
	return h
}

// TestAdminMuxAuditVerifyBehindAuth: /admin/audit/verify requires a token and,
// with one, returns the per-sink result for the configured file sinks.
func TestAdminMuxAuditVerifyBehindAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{},
		func() configapi.View { return configapi.View{} }, []string{path}, nil, nil, nil, nil)

	// no token → 401
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/audit/verify", nil))
	if rec.Code != 401 {
		t.Fatalf("audit/verify without token = %d, want 401", rec.Code)
	}
	// with token → 200 + sinks array
	req := httptest.NewRequest("GET", "/admin/audit/verify", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"sinks"`) {
		t.Fatalf("audit/verify with token = %d: %s", rec.Code, rec.Body.String())
	}
}
