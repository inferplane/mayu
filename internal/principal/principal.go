// Package principal carries the authenticated Principal across the request
// context. Separate from internal/server to avoid an import cycle
// (server imports anthropicapi; anthropicapi needs the principal accessor).
package principal

import (
	"context"

	"github.com/inferplane/inferplane/internal/keystore"
)

type ctxKey int

const key ctxKey = 0

func With(ctx context.Context, p keystore.Principal) context.Context {
	return context.WithValue(ctx, key, p)
}

func From(ctx context.Context) (keystore.Principal, bool) {
	p, ok := ctx.Value(key).(keystore.Principal)
	return p, ok
}
