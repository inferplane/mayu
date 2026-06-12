// Package principal carries the authenticated Principal across the request
// context. Separate from internal/server to avoid an import cycle
// (server imports anthropicapi; anthropicapi needs the principal accessor).
package principal

import (
	"context"

	"github.com/inferplane/inferplane/internal/keystore"
)

type ctxKey int

const (
	key      ctxKey = 0
	adminKey ctxKey = 1 // separate key: admin identity never shadows the data-plane Principal
)

func With(ctx context.Context, p keystore.Principal) context.Context {
	return context.WithValue(ctx, key, p)
}

func From(ctx context.Context) (keystore.Principal, bool) {
	p, ok := ctx.Value(key).(keystore.Principal)
	return p, ok
}

// AdminIdentity is the admin-plane caller (§5.1 Identity→Principal, ADR-004).
// PII-minimal by design (P2 gate): only the opaque OIDC `sub` — never email,
// never raw IdP groups — enters the request context; groups are consumed by
// the middleware's mapping step and dropped. Break-glass static tokens inject
// the sentinel {Subject: "break-glass", IsAdmin: true}.
type AdminIdentity struct {
	Subject    string
	Teams      []string // teams this identity may issue/revoke keys for (nil for admins)
	IsAdmin    bool     // admin_groups member or break-glass: entitled to every team
	AuthMethod string   // "oidc" | "break_glass" — recorded in audit
}

// Entitled reports whether the identity may act on the given team.
// Fail-closed: a zero identity (or empty team) is entitled to nothing.
func (a AdminIdentity) Entitled(team string) bool {
	if a.IsAdmin {
		return true
	}
	if team == "" {
		return false
	}
	for _, t := range a.Teams {
		if t == team {
			return true
		}
	}
	return false
}

// WithAdmin attaches the admin-plane identity to the context.
func WithAdmin(ctx context.Context, a AdminIdentity) context.Context {
	return context.WithValue(ctx, adminKey, a)
}

// AdminFrom retrieves the admin-plane identity, if any.
func AdminFrom(ctx context.Context) (AdminIdentity, bool) {
	a, ok := ctx.Value(adminKey).(AdminIdentity)
	return a, ok
}
