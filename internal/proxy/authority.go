package proxy

import (
	"context"
	"time"

	"github.com/inferplane/inferplane/internal/policy"
)

// AuthorityClient journals every change locally; methods never call a control
// plane. Only Syncer owns the network, keeping inference off the central path.
type AuthorityClient interface {
	Request(context.Context) (policy.AuthorityRequest, error)
	Apply(context.Context, policy.AuthorityResponse, time.Duration) error
	Wake() <-chan struct{}
}
