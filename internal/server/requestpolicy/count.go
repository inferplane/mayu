package requestpolicy

import "context"

type localCountKey struct{}

// WithLocalCount marks a declared oversized count request. The body must still
// pass through MaxBytesReader and KeyAuth, but even a valid short body may not
// reach a provider. This small context seam avoids a server/ingress import cycle.
func WithLocalCount(ctx context.Context) context.Context {
	return context.WithValue(ctx, localCountKey{}, true)
}

func LocalCount(ctx context.Context) bool {
	local, _ := ctx.Value(localCountKey{}).(bool)
	return local
}
