package server

import (
	"encoding/json"
	"errors"
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
			if errors.Is(err, keystore.ErrStoreUnavailable) {
				storeUnavailable(w, r)
				return
			}
			// 401 either way (never reveal which keys exist) — but a distinct
			// message for "expired" lets a CLI-minted key holder know to re-run
			// `mayu login` rather than suspect a typo. Claude Code's
			// apiKeyHelper also re-invokes on any 401, so this message doesn't
			// change client behavior, only what a human sees.
			if errors.Is(err, keystore.ErrKeyExpired) {
				writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "API key expired")
				return
			}
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
			return
		}
		next.ServeHTTP(w, r.WithContext(principal.With(r.Context(), p)))
	})
}

func storeUnavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens" {
		_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": 0})
		return
	}
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/model/") && strings.HasSuffix(r.URL.Path, "/count-tokens") {
		_ = json.NewEncoder(w).Encode(map[string]int{"inputTokens": 0})
		return
	}
	w.Header().Set("Retry-After", "1")
	if strings.HasPrefix(r.URL.Path, "/model/") {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"__type": "ServiceUnavailableException", "message": "identity store unavailable"})
		return
	}
	if r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/responses" || r.URL.Path == "/v1/usage" {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "server_error", "code": "store_unavailable", "message": "identity store unavailable"}})
		return
	}
	writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "identity store unavailable")
}
