// Shared bearer-auth middleware for Server and UsageServer (D3, inferplaned
// console SSO plan): one implementation instead of two byte-identical
// duplicated auth methods.
package controlplane

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/inferplane/inferplane/internal/adminauth"
)

// OIDCVerifier is the narrow seam to internal/adminauth.Verifier so tests can
// fake the OIDC path without a network. A separate interface from
// internal/server's OIDCVerifier — controlplane is a peer package, not a
// dependent of server, and must not import it.
type OIDCVerifier interface {
	Verify(ctx context.Context, raw string) (adminauth.Claims, error)
}

// authOptions is the OIDC config Server/UsageServer accept via Option.
type authOptions struct {
	verifier OIDCVerifier
	mapping  adminauth.MappingConfig
}

// Option configures optional cross-cutting behavior on Server/UsageServer.
type Option func(*authOptions)

// WithOIDC enables console-SSO bearer auth alongside the static token.
// Routing is the same TOTAL rule as internal/server's AdminAuth: verifier
// non-nil AND the bearer is JWT-shaped (adminauth.IsOIDCBearerShape) ⇒ OIDC
// path; everything else ⇒ static path. That total rule is what keeps mayu's
// usage-pusher token (never JWT-shaped) from ever reaching the verifier, and
// keeps a JWT-shaped bearer from ever being compared against the static
// token (auth-bypass/timing-oracle guard — same rationale as ADR-004).
func WithOIDC(verifier OIDCVerifier, mapping adminauth.MappingConfig) Option {
	return func(o *authOptions) {
		o.verifier = verifier
		o.mapping = mapping
	}
}

func newAuthOptions(opts []Option) authOptions {
	var o authOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// actorCtxKey carries the authenticated actor string set by authn (see
// ActorFromContext) — the only per-request identity the control plane keeps.
// D4/D5 still holds: this is attribution for the mutation log, not a new
// authorization axis, and every actor value still has whole-console access.
type actorCtxKey struct{}

// ActorFromContext returns the identity that authenticated the current
// request, as set by authn: an OIDC subject (prefixed "oidc:"), the fixed
// string "static-token" when authenticated via the shared bearer (which
// carries no per-human identity — D4/D5), or "unauthenticated" when no auth
// is configured at all (token == "" and no verifier). Used only for the
// mutation log (policies.go) — never for authorization. "" only when the
// value was never set (a request that didn't go through authn, e.g. a
// direct programmatic call in a test).
func ActorFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(actorCtxKey{}).(string); ok {
		return v
	}
	return ""
}

func withActor(r *http.Request, actor string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), actorCtxKey{}, actor))
}

// authnWrite guards the policy MUTATION routes (PUT/DELETE /v1alpha1/policies).
// It deliberately does NOT accept the heartbeat token: that token is deployed
// to every data plane (control_plane.token_ref), so if it also carried write
// authority any node operator could rewrite fleet policy — the same reasoning
// that gave the credential broker its own token (ADR-040 decision 1;
// review/fable5 §08 B1). Accepted writers, in order:
//
//   - a verified console OIDC identity (verifier configured, JWT-shaped bearer,
//     groups resolve) — the SSO console keeps working unchanged;
//   - the DEDICATED write token (INFERPLANED_POLICY_WRITE_TOKEN), constant-time
//     compared;
//   - nothing else. With no write token configured, every static bearer —
//     including the heartbeat token — gets 403 with a message naming the fix.
//
// The one carve-out mirrors authn: token == "" AND no verifier AND no write
// token is the loopback-only dev posture, left fully open on purpose.
func authnWrite(token, writeToken string, opts authOptions, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token == "" && opts.verifier == nil && writeToken == "" {
			next(w, withActor(r, "unauthenticated"))
			return
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if bearer == "" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if opts.verifier != nil && adminauth.IsOIDCBearerShape(bearer) {
			claims, err := opts.verifier.Verify(r.Context(), bearer)
			if err != nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if _, _, ok := adminauth.Resolve(claims.Groups, opts.mapping); !ok {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			next(w, withActor(r, "oidc:"+claims.Subject))
			return
		}
		if writeToken != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(writeToken)) == 1 {
			next(w, withActor(r, "policy-write-token"))
			return
		}
		// A caller presenting the HEARTBEAT token is authenticated (it is a
		// real credential) but not authorized to write — 403, and say why:
		// the operator who hits this is holding that token and expecting it
		// to work as it used to. Comparing against it here leaks nothing the
		// sync endpoint doesn't already: it accepts that exact value.
		if token != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(token)) == 1 {
			writeJSONError(w, http.StatusForbidden, "the heartbeat token carries no policy-write authority (it is deployed to every data plane): use INFERPLANED_POLICY_WRITE_TOKEN or a console OIDC identity")
			return
		}
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}
}

// authn is the shared middleware. There is no request-context principal and
// no per-team scoping on the control plane (D4/D5): a resolved OIDC identity
// and the static token both grant the SAME whole-console access the one
// shared token already grants today — SSO is a strict improvement in
// per-human identity and short-lived tokens, not a new authorization axis.
// It does, however, tag the request with WHO authenticated it (ActorFromContext)
// so a policy mutation can at least be attributed to someone (policies.go).
//
// token == "" AND no verifier configured ⇒ unauthenticated, byte-identical
// to today's loopback-only posture. Otherwise a request needs EITHER a
// verified OIDC token whose groups resolve to something, OR the exact static
// token — an SSO-only deploy (empty token, verifier configured) is a
// legitimate posture, not a break-glass gap.
func authn(token string, opts authOptions, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token == "" && opts.verifier == nil {
			next(w, withActor(r, "unauthenticated"))
			return
		}

		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if bearer == "" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		if opts.verifier != nil && adminauth.IsOIDCBearerShape(bearer) {
			claims, err := opts.verifier.Verify(r.Context(), bearer)
			if err != nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if _, _, ok := adminauth.Resolve(claims.Groups, opts.mapping); !ok {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			next(w, withActor(r, "oidc:"+claims.Subject))
			return
		}

		if token == "" {
			// SSO-only deploy: a non-shaped bearer can never authenticate.
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if subtle.ConstantTimeCompare([]byte(bearer), []byte(token)) != 1 {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, withActor(r, "static-token"))
	}
}
