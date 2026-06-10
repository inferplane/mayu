package server

import (
	"encoding/json"
	"net/http"
)

// writeAnthropicError emits {"type":"error","error":{"type","message"}} —
// the shape Anthropic clients (Claude Code) expect.
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}
