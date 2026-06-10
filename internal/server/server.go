package server

import (
	"net/http"

	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server/anthropicapi"
)

// DataMux builds the data-plane (:8080) handler: Anthropic ingress endpoints
// behind virtual-key auth (M3). All endpoints resolve a Principal via the key
// store before reaching the router.
func DataMux(r *router.Router, store keystore.Store) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/messages", anthropicapi.NewMessagesHandler(r))
	mux.Handle("POST /v1/messages/count_tokens", anthropicapi.NewCountTokensHandler(r))
	mux.Handle("GET /v1/models", anthropicapi.NewModelsHandler(r))
	return KeyAuth(store, mux)
}

// AdminMux builds the admin-plane (:9090) handler: health + (M3) /metrics,
// admin API. /healthz and /readyz are unauthenticated (design doc §5.5 splits
// metrics/health auth from admin auth).
func AdminMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	return mux
}
