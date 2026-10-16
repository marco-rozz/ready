package agent

import (
	"context"
	"sync"
)

type livenessKey struct{}

// WithLiveness returns a context whose liveness callback is safe to signal
// repeatedly. The first signal wins; nil callbacks remain harmless.
func WithLiveness(ctx context.Context, fn func()) context.Context {
	if fn == nil {
		return ctx
	}

	return context.WithValue(ctx, livenessKey{}, sync.OnceFunc(fn))
}

// SignalLiveness invokes the callback installed by WithLiveness, if any.
func SignalLiveness(ctx context.Context) {
	if ctx == nil {
		return
	}

	if fn, ok := ctx.Value(livenessKey{}).(func()); ok {
		fn()
	}
}
