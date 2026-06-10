package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// AdminTokenAuth guards the admin plane. Tokens are compared by SHA-256 +
// constant-time (so length/content don't leak via timing), and multiple tokens
// are accepted for rotation (§5.5). An empty token set denies everything
// (defense-in-depth, like KeyAuth). Separate credential from data-plane keys.
func AdminTokenAuth(tokens []string, next http.Handler) http.Handler {
	hashes := make([][32]byte, len(tokens))
	for i, t := range tokens {
		hashes[i] = sha256.Sum256([]byte(t))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" || len(hashes) == 0 {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "admin token required")
			return
		}
		gh := sha256.Sum256([]byte(got))
		ok := false
		for _, h := range hashes {
			if subtle.ConstantTimeCompare(gh[:], h[:]) == 1 {
				ok = true
			}
		}
		if !ok {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid admin token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
