package server

import (
	"net/http"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server/adminapi"
	"github.com/inferplane/inferplane/internal/server/anthropicapi"
)

// DataMux builds the data-plane (:8080) handler: Anthropic ingress endpoints
// behind virtual-key auth (M3). All endpoints resolve a Principal via the key
// store before reaching the router. aud is the audit writer (may be nil) used
// for the two-phase request_started/request_completed records on /v1/messages.
func DataMux(r *router.Router, store keystore.Store, aud *audit.Writer) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/messages", anthropicapi.NewMessagesHandlerWithAudit(r, aud))
	mux.Handle("POST /v1/messages/count_tokens", anthropicapi.NewCountTokensHandler(r))
	mux.Handle("GET /v1/models", anthropicapi.NewModelsHandler(r))
	return KeyAuth(store, mux)
}

// AdminMux builds the admin-plane (:9090) handler: health + /admin/keys CRUD.
// /healthz and /readyz are unauthenticated; /admin/keys is guarded by
// AdminTokenAuth (design doc §5.5 splits metrics/health auth from admin auth).
func AdminMux(store keystore.Store, adminTokens []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	keys := adminapi.NewKeysHandler(store)
	mux.Handle("/admin/keys", AdminTokenAuth(adminTokens, keys))
	mux.Handle("/admin/keys/", AdminTokenAuth(adminTokens, keys))
	return mux
}
