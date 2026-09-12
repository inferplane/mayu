// Package authapi implements the data-plane CLI login endpoints (ADR-028):
// GET /v1/auth/config (unauthenticated, secret-free discovery for
// `mayu login`), POST /v1/auth/key (OIDC-authenticated mint of a
// short-lived virtual key), and DELETE /v1/auth/key (virtual-key-authenticated
// self-revoke, for `mayu logout`). All three are opt-in — mounted only
// when oidc.cli_login is configured — and exist so a developer never copies a
// long-lived ik_... key by hand; CI/service-account provisioning keeps using
// `mayu keys create` / POST /admin/keys unchanged.
package authapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/pkg/ulid"
)

// ConfigView is the secret-free bootstrap payload `mayu login` reads to
// discover the CLI OAuth client (mirrors server.AuthConfigView for the
// console SPA, ADR-026). Issuer and ClientID are public identifiers of a PKCE
// public client, never a secret.
type ConfigView struct {
	CLI      bool   `json:"cli"`
	Issuer   string `json:"issuer,omitempty"`
	ClientID string `json:"client_id,omitempty"`
}

// NewConfigHandler serves GET /v1/auth/config from a closure so config
// reflects whatever oidc.cli_login was loaded with. Only GET is accepted.
func NewConfigHandler(view func() ConfigView) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"read-only"}`, http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(view())
	})
}

// mintEvent emits one CLI-auth audit record — same shape as adminapi's
// admin_key_created/revoked, distinct event names so an operator can tell a
// CLI-minted key apart from a console/API one at a glance.
func mintEvent(emit func(audit.Record), event, sub, team, keyID string) {
	if emit == nil {
		return
	}
	method := "oidc"
	emit(audit.Record{
		SchemaVersion: 1,
		Event:         event,
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: keyID, Team: team, User: &sub, AuthMethod: &method},
		Request:       audit.RequestRef{Ingress: "cli"},
	})
}

// mintRequest is the only client-controlled input to POST /v1/auth/key. TTL,
// owner, and metadata are ALWAYS server-decided (ADR-028) — a client-supplied
// TTL would make "short-lived" a false claim, and a client-supplied owner
// would let a teammate mint a key and attribute it to someone else (the same
// hole closed in adminapi.KeysHandler.create).
type mintRequest struct {
	Team string `json:"team,omitempty"`
}

// pickTeam resolves the team to mint for: an explicit request wins if the
// caller is entitled to it; otherwise a caller entitled to exactly one team
// gets it for free; anything else is an error naming the ambiguity so the CLI
// can print a useful "--team" hint. ok=false half of the possible ambiguity
// error cases carries a message meant to reach the CLI's stderr verbatim.
func pickTeam(id principal.AdminIdentity, requested string) (team string, err error) {
	if requested != "" {
		if !id.Entitled(requested) {
			return "", errNotEntitled
		}
		return requested, nil
	}
	if id.IsAdmin {
		return "", errTeamRequired
	}
	switch len(id.Teams) {
	case 1:
		return id.Teams[0], nil
	case 0:
		return "", errNoTeams
	default:
		return "", errAmbiguousTeams
	}
}

// MintHandler serves POST /v1/auth/key. It is mounted behind server.AdminAuth
// (the SAME OIDC-verify + groups→team middleware the admin plane uses, keyed
// to the CLI's own client_id — see cliVerifier in cmd/mayu/gateway.go)
// so principal.AdminFrom(ctx) is already populated on entry; a request that
// reached here without it is denied, never silently trusted.
func MintHandler(store keystore.Store, ttl time.Duration, mint limiter.LimiterStore, emit func(audit.Record)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		id, ok := principal.AdminFrom(r.Context())
		if !ok {
			http.Error(w, `{"error":"no identity"}`, http.StatusForbidden)
			return
		}
		var body mintRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid request body: `+jsonEscape(err.Error())+`"}`, http.StatusBadRequest)
			return
		}
		team, err := pickTeam(id, body.Team)
		if err != nil {
			status := http.StatusBadRequest
			if err == errNotEntitled {
				status = http.StatusForbidden
			}
			http.Error(w, `{"error":"`+jsonEscape(err.Error())+`"}`, status)
			return
		}
		// Per-subject mint throttle (ADR-028 follow-up r1): a valid ID token
		// alone must not be able to grow the keys table without bound. 10/min
		// with a burst of 10 comfortably covers a normal login+every-renewal
		// cadence while capping an accidental or malicious mint loop.
		if !mint.AllowRate("cli_mint:"+id.Subject, 1, 10, 10) {
			http.Error(w, `{"error":"too many key requests, slow down"}`, http.StatusTooManyRequests)
			return
		}
		expiresAt := time.Now().UTC().Add(ttl)
		plaintext, p, err := store.CreateWithOptions(r.Context(), team, []string{"*"}, keystore.KeyOptions{
			ExpiresAt: &expiresAt,
			Owner:     id.Subject,
			Metadata:  map[string]string{"source": "cli"},
			// Deliberately no BudgetUSDMicros/TPM/RPM: those key on the
			// rotating key_id with a fixed-length window (governance.go), so a
			// per-key limit would reset every time the CLI re-mints. CLI-key
			// spend is governed at the TEAM level only.
		})
		if err != nil {
			http.Error(w, `{"error":"create failed"}`, http.StatusInternalServerError)
			return
		}
		mintEvent(emit, "cli_key_created", id.Subject, p.Team, p.KeyID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"key":        plaintext,
			"key_id":     p.KeyID,
			"team":       p.Team,
			"expires_at": expiresAt.Format(time.RFC3339Nano),
		})
	})
}

// RevokeHandler serves DELETE /v1/auth/key: a virtual key revokes ITSELF, for
// `mayu logout`. Mounted behind KeyAuth (the data-plane Principal, not
// AdminIdentity) — no refresh token is cached client-side (ADR-028), so the
// key currently in hand is the only credential logout can authenticate with.
// Self-revoke needs no entitlement check beyond "this is a valid, live key".
func RevokeHandler(store keystore.Store, emit func(audit.Record)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		p, ok := principal.From(r.Context())
		if !ok {
			http.Error(w, `{"error":"no identity"}`, http.StatusUnauthorized)
			return
		}
		var err error
		if conditional, ok := store.(interface {
			RevokeSnapshot(context.Context, keystore.Principal) error
		}); ok {
			err = conditional.RevokeSnapshot(r.Context(), p)
		} else {
			err = store.Revoke(r.Context(), p.KeyID)
		}
		if errors.Is(err, keystore.ErrSnapshotChanged) {
			http.Error(w, `{"error":"key changed; retry authorization"}`, http.StatusConflict)
			return
		}
		if err != nil {
			http.Error(w, `{"error":"revoke failed"}`, http.StatusInternalServerError)
			return
		}
		mintEvent(emit, "cli_key_revoked", p.Owner, p.Team, p.KeyID)
		w.WriteHeader(http.StatusNoContent)
	})
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1]) // strip the surrounding quotes json.Marshal adds
}
