package representation

import (
	"context"
	"sync/atomic"
)

type cacheabilityContextKey struct{}

type cacheabilityState struct {
	nonCacheable atomic.Bool
}

// WithCacheabilityState installs request-scoped response cacheability state.
// Repeated calls preserve the existing state so every layer observes the same
// fail-soft decision.
func WithCacheabilityState(ctx context.Context) context.Context {
	if _, ok := ctx.Value(cacheabilityContextKey{}).(*cacheabilityState); ok {
		return ctx
	}
	return context.WithValue(ctx, cacheabilityContextKey{}, &cacheabilityState{})
}

// MarkResponseNonCacheable records that a fail-soft or partial dependency was
// used while assembling the current response. It is a no-op outside an
// eligible conditional request.
func MarkResponseNonCacheable(ctx context.Context) {
	state, ok := ctx.Value(cacheabilityContextKey{}).(*cacheabilityState)
	if ok && state != nil {
		state.nonCacheable.Store(true)
	}
}

// ResponseMarkedNonCacheable reports the request-scoped fail-soft decision.
func ResponseMarkedNonCacheable(ctx context.Context) bool {
	state, ok := ctx.Value(cacheabilityContextKey{}).(*cacheabilityState)
	return ok && state != nil && state.nonCacheable.Load()
}
