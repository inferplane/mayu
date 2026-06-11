package server

import (
	"net/http"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server/adminapi"
	"github.com/inferplane/inferplane/internal/server/anthropicapi"
	"github.com/inferplane/inferplane/internal/server/openaiapi"
)

// DataMux builds the data-plane (:8080) handler: Anthropic ingress endpoints
// behind virtual-key auth (M3). All endpoints resolve a Principal via the key
// store before reaching the router. aud is the audit writer (may be nil) used
// for the two-phase request_started/request_completed records on /v1/messages.
// gov is the governance pipeline (rate/quota/budget + cost); when non-nil the
// /v1/messages handler enforces it, when nil governance is bypassed.
func DataMux(r *router.Router, store keystore.Store, aud *audit.Writer, gov *governance.Governor) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/messages", anthropicapi.NewMessagesHandlerFull(r, aud, gov))
	mux.Handle("POST /v1/messages/count_tokens", anthropicapi.NewCountTokensHandler(r))
	mux.Handle("POST /v1/chat/completions", openaiapi.NewChatHandlerFull(r, aud, gov))
	// Both the Anthropic (Claude Code) and OpenAI (OpenCode) clients hit the
	// same GET /v1/models path but expect different response shapes, so we
	// content-negotiate: Anthropic clients send an `anthropic-version` header,
	// OpenAI clients do not. (Documented heuristic, M5 §3.2.)
	mux.Handle("GET /v1/models", negotiateModels(
		anthropicapi.NewModelsHandler(r), openaiapi.NewModelsHandler(r)))
	return KeyAuth(store, mux)
}

// negotiateModels routes GET /v1/models to the Anthropic-shaped handler when the
// request carries an `anthropic-version` header (sent by Claude Code and other
// Anthropic SDKs), and to the OpenAI-shaped handler otherwise (OpenCode / OpenAI
// clients). The two ingress protocols share the path but expect different JSON.
func negotiateModels(anthropicH, openaiH http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("anthropic-version") != "" {
			anthropicH.ServeHTTP(w, req)
			return
		}
		openaiH.ServeHTTP(w, req)
	})
}

// AdminMux builds the admin-plane (:9090) handler: health + /metrics + /admin/keys
// CRUD. /healthz, /readyz, and /metrics are unauthenticated; /admin/keys is guarded
// by AdminTokenAuth (design doc §5.5 splits metrics/health auth from admin auth).
// When m is nil the /metrics endpoint is omitted.
func AdminMux(store keystore.Store, adminTokens []string, m *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	if m != nil {
		mux.Handle("GET /metrics", metricsHandler(m)) // unauthenticated (§5.5)
	}
	keys := adminapi.NewKeysHandler(store)
	mux.Handle("/admin/keys", AdminTokenAuth(adminTokens, keys))
	mux.Handle("/admin/keys/", AdminTokenAuth(adminTokens, keys))
	return mux
}
