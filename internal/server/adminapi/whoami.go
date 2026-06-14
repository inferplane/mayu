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
		id, _ := principal.AdminFrom(r.Context()) // zero value if absent (behind AdminAuth, so present)
		teams := id.Teams
		if teams == nil {
			teams = []string{} // serialize [] not null
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whoamiResponse{
			Subject:    id.Subject,
			Teams:      teams,
			IsAdmin:    id.IsAdmin,
			AuthMethod: id.AuthMethod,
		})
	})
}
