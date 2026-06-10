// Package adminapi implements the admin-plane key management endpoints,
// guarded by AdminTokenAuth (§5.5). Create returns the plaintext key ONCE.
package adminapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/inferplane/inferplane/internal/keystore"
)

type KeysHandler struct{ store keystore.Store }

func NewKeysHandler(store keystore.Store) *KeysHandler { return &KeysHandler{store: store} }

func (h *KeysHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost:
		h.create(w, r)
	case r.Method == http.MethodGet:
		h.list(w, r)
	case r.Method == http.MethodDelete:
		h.revoke(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *KeysHandler) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Team          string   `json:"team"`
		AllowedModels []string `json:"allowed_models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Team == "" {
		http.Error(w, `{"error":"team required"}`, http.StatusBadRequest)
		return
	}
	plaintext, p, err := h.store.Create(r.Context(), body.Team, body.AllowedModels)
	if err != nil {
		http.Error(w, `{"error":"create failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"key_id": p.KeyID, "team": p.Team, "allowed_models": p.AllowedModels, "plaintext": plaintext})
}

func (h *KeysHandler) list(w http.ResponseWriter, r *http.Request) {
	ps, err := h.store.List(r.Context())
	if err != nil {
		http.Error(w, `{"error":"list failed"}`, http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		out = append(out, map[string]any{"key_id": p.KeyID, "team": p.Team, "allowed_models": p.AllowedModels})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": out})
}

func (h *KeysHandler) revoke(w http.ResponseWriter, r *http.Request) {
	keyID := strings.TrimPrefix(r.URL.Path, "/admin/keys/")
	if keyID == "" || keyID == r.URL.Path {
		http.Error(w, `{"error":"key_id required in path"}`, http.StatusBadRequest)
		return
	}
	if err := h.store.Revoke(r.Context(), keyID); err != nil {
		http.Error(w, `{"error":"revoke failed"}`, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
