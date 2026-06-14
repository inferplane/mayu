package adminapi

import (
	"encoding/json"
	"net/http"

	"github.com/inferplane/inferplane/internal/principal"
)

// whoamiResponse is the secret-free, PII-free identity projection (ADR-010). It
// is a DEDICATED DTO — NOT principal.AdminIdentity marshaled directly — so a
// future identity field (email, raw claims, groups) cannot leak through whoami:
// it would have to be added here on purpose. Teams is always a non-nil slice so
// it serializes as [] rather than null.
type whoamiResponse struct {
	Subject    string   `json:"subject"`
	Teams      []string `json:"teams"`
	IsAdmin    bool     `json:"is_admin"`
	AuthMethod string   `json:"auth_method"`
}

// newWhoamiResponse is the single allowlist boundary from the rich AdminIdentity
// to the PII-free wire DTO (P4 gate): it copies ONLY the four safe fields and
// always initializes Teams to a non-nil slice (serializes [] not null). A future
// AdminIdentity field is ignored here unless added on purpose.
func newWhoamiResponse(id principal.AdminIdentity) whoamiResponse {
	teams := id.Teams
	if teams == nil {
		teams = []string{}
	}
	return whoamiResponse{
		Subject:    id.Subject,
		Teams:      teams,
		IsAdmin:    id.IsAdmin,
		AuthMethod: id.AuthMethod,
	}
}

// WhoamiHandler serves GET /admin/whoami: the caller's resolved admin identity
// (opaque subject, entitled teams, admin flag, auth method). It is mounted
// behind the same AdminAuth as /admin/keys, so the identity is already in the
// request context. Read-only and not audited (no state change; the identity is
// already recorded at middleware entry / on denials). Writes 405.
func WhoamiHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"read-only"}`, http.StatusMethodNotAllowed)
			return
		}
		// Fail closed (P4 gate): although this is mounted behind AdminAuth, a
		// missing identity must 401, never serialize a zero-value identity at 200.
		id, ok := principal.AdminFrom(r.Context())
		if !ok {
			http.Error(w, `{"error":"unauthenticated"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(newWhoamiResponse(id))
	})
}
