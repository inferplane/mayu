package requestpolicy

import (
	"context"
	"net/http"

	"github.com/inferplane/inferplane/internal/router"
)

type affinityKey struct{}
type affinityAttempt struct {
	router *router.Router
	token  *router.AffinityToken
	target router.ChainTarget
}

// SessionHint is a performance hint, never an authenticated identity. Bound it
// before hashing; provider adapters do not forward this gateway-local header.
func SessionHint(req *http.Request) string {
	hint := req.Header.Get("X-Inferplane-Session-ID")
	if len(hint) > 1024 {
		return ""
	}
	return hint
}

// WithAffinityAttempt attaches an unexported, request-local success callback.
// The token/target are not part of audit, metrics, headers or body capture.
func WithAffinityAttempt(req *http.Request, r *router.Router, token *router.AffinityToken, target router.ChainTarget) *http.Request {
	if token == nil {
		return req
	}
	return req.WithContext(context.WithValue(req.Context(), affinityKey{}, affinityAttempt{r, token, target}))
}

func RecordSuccess(req *http.Request) {
	if req.Context().Err() != nil {
		return
	}
	if a, ok := req.Context().Value(affinityKey{}).(affinityAttempt); ok {
		a.router.RecordAffinitySuccess(a.token, a.target)
	}
}
