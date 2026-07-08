package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/analytics"
	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/bodystore"
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
	mux := DataMux(r, store, nil, nil, nil, nil, nil)

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
	mux := DataMux(r, store, nil, nil, nil, nil, nil)

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
	mux := DataMux(r, store, nil, nil, nil, nil, nil)

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
	mux := AdminMux(store, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
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
	mux := AdminMux(store, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

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
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, m, nil, nil, nil, nil, nil, nil, nil)
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
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
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
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
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
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
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
	mux := AdminMux(store, []string{"admin-tok"}, v, mapping, func() configapi.View { return configapi.View{} }, nil, aud, nil, nil, nil, nil, nil, nil, nil, nil)

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
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
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
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return view }, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

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
		func() configapi.View { return configapi.View{} }, []string{path}, nil, nil, nil, nil, nil, nil, nil, nil, nil)

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

// TestAdminMuxWhoami (ADR-010): /admin/whoami is behind AdminAuth (401 unauth)
// and returns the identity for a valid token.
func TestAdminMuxWhoami(t *testing.T) {
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{}, func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	// no token → 401 (no identity leak)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/whoami", nil))
	if rec.Code != 401 {
		t.Fatalf("whoami without token = %d, want 401", rec.Code)
	}

	// break-glass admin token → 200, is_admin true
	req := httptest.NewRequest("GET", "/admin/whoami", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("whoami with token = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"is_admin":true`) {
		t.Fatalf("break-glass whoami should be admin: %s", rec.Body.String())
	}
}

func TestAdminMux_capabilitiesEndpoint(t *testing.T) {
	caps := func() configapi.Capabilities {
		return configapi.Capabilities{AnalyticsIndex: "off", ProviderStore: true}
	}
	h := AdminMux(nil, []string{"tok"}, nil, adminauth.MappingConfig{}, nil, nil, nil, nil, nil, nil, caps, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/capabilities", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"provider_store":true`) {
		t.Fatalf("body missing provider_store:true — %s", rec.Body.String())
	}
}

func TestAdminMux_analyticsFullAdminOnly(t *testing.T) {
	fakeQ := analyticsStubQ{}
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{},
		func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil, nil, fakeQ, nil, nil, nil)

	// full admin (static token) → 200
	req := httptest.NewRequest("GET", "/admin/analytics/summary", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("full-admin analytics = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// no token → 401 (unauthenticated)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("GET", "/admin/analytics/summary", nil))
	if rec2.Code != 401 {
		t.Fatalf("no-token analytics = %d, want 401", rec2.Code)
	}
}

// TestAdminMux_logsFullAdminOnly (D4, ADR-018): /admin/logs mounts only when
// analyticsQ is non-nil (same optional-dependency shape as the other
// analytics endpoints) and is full-admin gated.
func TestAdminMux_logsFullAdminOnly(t *testing.T) {
	fakeQ := analyticsStubQ{}
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{},
		func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil, nil, fakeQ, nil, nil, nil)

	req := httptest.NewRequest("GET", "/admin/logs", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("full-admin /admin/logs = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("GET", "/admin/logs", nil))
	if rec2.Code != 401 {
		t.Fatalf("no-token /admin/logs = %d, want 401", rec2.Code)
	}
}

// TestAdminMux_logsOmittedWhenNilAnalyticsQ proves a nil analyticsQ omits
// /admin/logs entirely, matching the other analytics-dependent mounts.
func TestAdminMux_logsOmittedWhenNilAnalyticsQ(t *testing.T) {
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{},
		func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest("GET", "/admin/logs", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("nil analyticsQ: /admin/logs = %d, want 404 (unmounted)", rec.Code)
	}
}

type analyticsStubQ struct{}

func (analyticsStubQ) Summary(analytics.SummaryQuery) (analytics.Summary, error) {
	return analytics.Summary{Totals: analytics.Totals{Requests: 1}}, nil
}
func (analyticsStubQ) TimeSeries(analytics.TimeSeriesQuery) ([]analytics.DayPoint, error) {
	return []analytics.DayPoint{}, nil
}
func (analyticsStubQ) Health() (analytics.Health, error) {
	return analytics.Health{Mode: "A", IsLeader: true}, nil
}
func (analyticsStubQ) Recent(limit int, before string) ([]analytics.Event, error) {
	return []analytics.Event{}, nil
}

// --- /admin/teams + /admin/users wiring (D3, ADR-016) ---

// TestAdminMux_TeamsWriteRequiresFullAdmin proves PUT/DELETE /admin/teams is
// gated by requireAdmin (the SAME full-admin tier as the probe/analytics
// endpoints): a team-mapped OIDC identity can read but not write, so it can
// never raise its own team's budget via the console.
func TestAdminMux_TeamsWriteRequiresFullAdmin(t *testing.T) {
	store, err := keystore.OpenSQLite(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	v := &fakeVerifier{claims: adminauth.Claims{Subject: "u-alpha", Groups: []string{"team-alpha"}}}
	mapping := adminauth.MappingConfig{GroupMappings: []adminauth.GroupMapping{{Group: "team-alpha", Teams: []string{"alpha"}}}}
	configTeams := func() []string { return nil }
	mux := AdminMux(store, []string{"admin-tok"}, v, mapping, func() configapi.View { return configapi.View{} },
		nil, nil, nil, nil, nil, nil, nil, store, configTeams, nil)

	do := func(bearer, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// team-mapped OIDC identity: read allowed
	if rec := do(jwtShaped, "GET", "/admin/teams", ""); rec.Code != 200 {
		t.Fatalf("team-mapped GET /admin/teams = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// team-mapped OIDC identity: write denied, even for its own team
	if rec := do(jwtShaped, "PUT", "/admin/teams/alpha", `{"rpm":1}`); rec.Code != 403 {
		t.Fatalf("team-mapped PUT /admin/teams/alpha = %d, want 403", rec.Code)
	}
	// full admin (static break-glass token): write allowed
	if rec := do("admin-tok", "PUT", "/admin/teams/alpha", `{"rpm":1}`); rec.Code != 200 {
		t.Fatalf("full-admin PUT /admin/teams/alpha = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := do("admin-tok", "DELETE", "/admin/teams/alpha", ""); rec.Code != 204 {
		t.Fatalf("full-admin DELETE /admin/teams/alpha = %d, want 204", rec.Code)
	}
	// no token → 401 (unauthenticated)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/teams", nil))
	if rec.Code != 401 {
		t.Fatalf("no-token /admin/teams = %d, want 401", rec.Code)
	}
}

// TestAdminMux_UsersEndpointAnyAdminIdentity proves /admin/users (derived
// read-only, no writes exist) is reachable by any AdminAuth identity, not
// just full admins — it is a projection of data the team-mapped identity can
// already see via /admin/keys.
func TestAdminMux_UsersEndpointAnyAdminIdentity(t *testing.T) {
	store, err := keystore.OpenSQLite(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.CreateWithOptions(context.Background(), "alpha", []string{"*"}, keystore.KeyOptions{Owner: "alice"})

	v := &fakeVerifier{claims: adminauth.Claims{Subject: "u-alpha", Groups: []string{"team-alpha"}}}
	mapping := adminauth.MappingConfig{GroupMappings: []adminauth.GroupMapping{{Group: "team-alpha", Teams: []string{"alpha"}}}}
	mux := AdminMux(store, []string{"admin-tok"}, v, mapping, func() configapi.View { return configapi.View{} },
		nil, nil, nil, nil, nil, nil, nil, store, func() []string { return nil }, nil)

	req := httptest.NewRequest("GET", "/admin/users", nil)
	req.Header.Set("Authorization", "Bearer "+jwtShaped)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"alice"`) {
		t.Fatalf("team-mapped GET /admin/users = %d %s, want 200 with alice", rec.Code, rec.Body.String())
	}
}

// TestAdminMux_TeamsOmittedWhenNilTeamStore proves a nil teamStore (the
// optional-dependency shape shared with analyticsQ) omits both mounts
// entirely rather than panicking or 500ing.
func TestAdminMux_TeamsOmittedWhenNilTeamStore(t *testing.T) {
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{},
		func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest("GET", "/admin/teams", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("nil teamStore: /admin/teams = %d, want 404 (unmounted)", rec.Code)
	}
}

// --- /admin/bodies/{ref} wiring (D4, ADR-018) ---

func testBodiesRecorder(t *testing.T) *bodystore.Recorder {
	t.Helper()
	store, err := bodystore.OpenSQLite(filepath.Join(t.TempDir(), "bodies.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var key [32]byte
	rec := bodystore.NewRecorder(store, key, time.Hour, 1<<20)
	t.Cleanup(rec.Close)
	return rec
}

// TestAdminMux_BodiesRequiresFullAdmin proves /admin/bodies/{ref} is full-
// admin only (unlike /admin/teams, whose GET is any AdminAuth identity) — a
// body can carry cross-team-sensitive content.
func TestAdminMux_BodiesRequiresFullAdmin(t *testing.T) {
	bodies := testBodiesRecorder(t)
	ref := bodies.Capture("rec-1", "acme", []byte("req"), []byte("resp"))
	bodies.Close()

	v := &fakeVerifier{claims: adminauth.Claims{Subject: "u-alpha", Groups: []string{"team-alpha"}}}
	mapping := adminauth.MappingConfig{GroupMappings: []adminauth.GroupMapping{{Group: "team-alpha", Teams: []string{"alpha"}}}}
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, v, mapping, func() configapi.View { return configapi.View{} },
		nil, nil, nil, nil, nil, nil, nil, nil, nil, bodies)

	// team-mapped OIDC identity: denied even for reads.
	req := httptest.NewRequest("GET", "/admin/bodies/"+ref, nil)
	req.Header.Set("Authorization", "Bearer "+jwtShaped)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("team-mapped GET /admin/bodies/%s = %d, want 403", ref, rec.Code)
	}
	// full admin (static break-glass token): allowed.
	req2 := httptest.NewRequest("GET", "/admin/bodies/"+ref, nil)
	req2.Header.Set("Authorization", "Bearer admin-tok")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("full-admin GET /admin/bodies/%s = %d, want 200: %s", ref, rec2.Code, rec2.Body.String())
	}
	// no token → 401.
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest("GET", "/admin/bodies/"+ref, nil))
	if rec3.Code != 401 {
		t.Fatalf("no-token /admin/bodies/%s = %d, want 401", ref, rec3.Code)
	}
}

// TestAdminMux_BodiesOmittedWhenNilBodiesRec proves a nil bodiesRec (log_bodies
// off) omits the mount entirely rather than panicking or 500ing.
func TestAdminMux_BodiesOmittedWhenNilBodiesRec(t *testing.T) {
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{},
		func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest("GET", "/admin/bodies/anything", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("nil bodiesRec: /admin/bodies/anything = %d, want 404 (unmounted)", rec.Code)
	}
}
