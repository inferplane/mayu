// Package adminapi implements the admin-plane key management endpoints,
// guarded by AdminAuth (§5.5, ADR-004). Create returns the plaintext key
// ONCE. Every mutation — including denied attempts — is a governance event
// and emits an audit record; the handler enforces per-team entitlement
// itself (enforcing only in the middleware would make authZ cosmetic).
package adminapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/pkg/ulid"
)

type KeysHandler struct {
	store keystore.Store
	emit  func(audit.Record) // nil-safe; wired to the audit writer in server.go
}

// NewKeysHandler builds the handler. emit receives admin-action audit records
// (admin_key_created / admin_key_revoked / admin_denied); pass nil to skip.
func NewKeysHandler(store keystore.Store, emit func(audit.Record)) *KeysHandler {
	return &KeysHandler{store: store, emit: emit}
}

func (h *KeysHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Fail-closed: a request that reached us without the AdminAuth middleware
	// (no identity in context) is denied — never silently trusted.
	id, ok := principal.AdminFrom(r.Context())
	if !ok {
		http.Error(w, `{"error":"no admin identity"}`, http.StatusForbidden)
		return
	}
	switch {
	case r.Method == http.MethodPost:
		h.create(w, r, id)
	case r.Method == http.MethodGet:
		h.list(w, r)
	case r.Method == http.MethodDelete:
		h.revoke(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// adminEvent emits one admin-plane audit record. The User field carries the
// opaque subject (never email — PII stays out of the chain, ADR-003/004);
// keyID is empty for denials.
func (h *KeysHandler) adminEvent(event string, id principal.AdminIdentity, team, keyID string) {
	if h.emit == nil {
		return
	}
	sub, method := id.Subject, id.AuthMethod
	h.emit(audit.Record{
		SchemaVersion: 1,
		Event:         event,
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: keyID, Team: team, User: &sub, AuthMethod: &method},
		Request:       audit.RequestRef{Ingress: "admin"},
	})
}

// keyOptionsBody is the wire shape of the optional §8 D2 governance fields.
// ExpiresAt is RFC3339 text (empty = never); enforcement of budget/TPM/RPM in
// the request hot path is a separate follow-up — these fields are stored and
// surfaced only, for now.
type keyOptionsBody struct {
	BudgetUSDMicros int64             `json:"budget_usd_micros,omitempty"`
	TPM             int64             `json:"tpm,omitempty"`
	RPM             int64             `json:"rpm,omitempty"`
	ExpiresAt       string            `json:"expires_at,omitempty"`
	Owner           string            `json:"owner,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

func (b keyOptionsBody) toKeyOptions() (keystore.KeyOptions, error) {
	opts := keystore.KeyOptions{BudgetUSDMicros: b.BudgetUSDMicros, TPM: b.TPM, RPM: b.RPM, Owner: b.Owner, Metadata: b.Metadata}
	if b.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, b.ExpiresAt)
		if err != nil {
			return opts, err
		}
		opts.ExpiresAt = &t
	}
	return opts, nil
}

func keyView(p keystore.Principal) map[string]any {
	v := map[string]any{"key_id": p.KeyID, "team": p.Team, "allowed_models": p.AllowedModels}
	if p.BudgetUSDMicros != 0 {
		v["budget_usd_micros"] = p.BudgetUSDMicros
	}
	if p.TPM != 0 {
		v["tpm"] = p.TPM
	}
	if p.RPM != 0 {
		v["rpm"] = p.RPM
	}
	if p.ExpiresAt != nil {
		v["expires_at"] = p.ExpiresAt.Format(time.RFC3339)
	}
	if p.Owner != "" {
		v["owner"] = p.Owner
	}
	if len(p.Metadata) > 0 {
		v["metadata"] = p.Metadata
	}
	return v
}

func (h *KeysHandler) create(w http.ResponseWriter, r *http.Request, id principal.AdminIdentity) {
	var body struct {
		Team          string   `json:"team"`
		AllowedModels []string `json:"allowed_models"`
		keyOptionsBody
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Team == "" {
		http.Error(w, `{"error":"team required"}`, http.StatusBadRequest)
		return
	}
	if !id.Entitled(body.Team) {
		h.adminEvent("admin_denied", id, body.Team, "")
		http.Error(w, `{"error":"not entitled to team"}`, http.StatusForbidden)
		return
	}
	opts, err := body.toKeyOptions()
	if err != nil {
		http.Error(w, `{"error":"expires_at must be RFC3339"}`, http.StatusBadRequest)
		return
	}
	plaintext, p, err := h.store.CreateWithOptions(r.Context(), body.Team, body.AllowedModels, opts)
	if err != nil {
		http.Error(w, `{"error":"create failed"}`, http.StatusInternalServerError)
		return
	}
	h.adminEvent("admin_key_created", id, p.Team, p.KeyID)
	out := keyView(p)
	out["plaintext"] = plaintext
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (h *KeysHandler) list(w http.ResponseWriter, r *http.Request) {
	ps, err := h.store.List(r.Context())
	if err != nil {
		http.Error(w, `{"error":"list failed"}`, http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		out = append(out, keyView(p))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": out})
}

func (h *KeysHandler) revoke(w http.ResponseWriter, r *http.Request, id principal.AdminIdentity) {
	keyID := strings.TrimPrefix(r.URL.Path, "/admin/keys/")
	if keyID == "" || keyID == r.URL.Path {
		http.Error(w, `{"error":"key_id required in path"}`, http.StatusBadRequest)
		return
	}
	// Entitlement needs the key's team: look it up before revoking. The list
	// is small (an admin-plane operation). Non-entitled callers get an
	// explicit 403 (and the denial is audited) — key IDs are not secret
	// material, so the existence signal is acceptable and the audit trail
	// is worth more. A lookup ERROR fails closed (P4 gate): proceeding
	// without a team would skip the entitlement check entirely.
	team, found, err := h.teamOf(r, keyID)
	if err != nil {
		http.Error(w, `{"error":"team lookup failed"}`, http.StatusInternalServerError)
		return
	}
	if found && !id.Entitled(team) {
		h.adminEvent("admin_denied", id, team, keyID)
		http.Error(w, `{"error":"not entitled to team"}`, http.StatusForbidden)
		return
	}
	if err := h.store.Revoke(r.Context(), keyID); err != nil {
		http.Error(w, `{"error":"revoke failed"}`, http.StatusNotFound)
		return
	}
	h.adminEvent("admin_key_revoked", id, team, keyID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *KeysHandler) teamOf(r *http.Request, keyID string) (string, bool, error) {
	ps, err := h.store.List(r.Context())
	if err != nil {
		return "", false, err
	}
	for _, p := range ps {
		if p.KeyID == keyID {
			return p.Team, true, nil
		}
	}
	return "", false, nil
}
