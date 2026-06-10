package server

import (
	"net/http"
	"strings"

	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
)

// KeyAuth resolves the client's virtual API key (x-api-key or Authorization:
// Bearer) to a Principal via the key store and injects it into the request
// context. Replaces M2's DevKeyAuth. The upstream provider key is never the
// client's (§5.2). Resolution failure → 401 with an Anthropic-shaped error.
func KeyAuth(store keystore.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-api-key")
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if key == "" {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "missing API key")
			return
		}
		p, err := store.Resolve(r.Context(), key)
		if err != nil {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
			return
		}
		next.ServeHTTP(w, r.WithContext(principal.With(r.Context(), p)))
	})
}
