package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// DevKeyAuth is the TEMPORARY single-key gate for M2. It compares the client's
// x-api-key or Authorization: Bearer against one configured key in constant
// time. Replaced by virtual-key auth + key store in M3. The upstream provider
// key is never exposed to the client (design doc §5.2 — established from M2).
func DevKeyAuth(devKey string, next http.Handler) http.Handler {
	want := []byte(devKey)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("x-api-key")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeAnthropicError emits an Anthropic-shaped error body. Anthropic clients
// (Claude Code) expect {"type":"error","error":{"type","message"}}.
// (Task 10 relocates this to errors.go.)
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}
