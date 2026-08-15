// Package fetcher – outbound per-host rate limiter.
//
// OutboundLimiter caps how fast we hit a single upstream host.
// Specialized fetchers (wechat, future juejin/zhihu/csdn) use this to
// avoid tripping aggressive anti-bot regimes that ban an IP after a
// burst. Unlike middleware.RateLimit which throttles inbound HTTP, this
// shapes our own outbound RPS.
//
// The limiter is keyed by host so two specialized fetchers hitting
// different hosts don't share a budget. Each per-host bucket is a
// `golang.org/x/time/rate.Limiter` configured from a single
// (events, window) parameter pair — "N events per window" is a more
// intuitive operator knob than (rate, burst).
package fetcher

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// OutboundLimiter is safe for concurrent use.
type OutboundLimiter struct {
	events int
	window time.Duration

	mu       sync.Mutex
	limiters map[string]*rate.Limiter
}

// NewOutboundLimiter returns a limiter that allows at most `events`
// requests per `window` per host. e.g. NewOutboundLimiter(1, 30*time.Second)
// = "1 request every 30 seconds per host". Burst is set to events.
//
// Returns nil if events <= 0 or window <= 0 — callers can treat the
// nil case as "no limit", which mirrors how *HTTPClient handles nil
// transports in other places in this package.
func NewOutboundLimiter(events int, window time.Duration) *OutboundLimiter {
	if events <= 0 || window <= 0 {
		return nil
	}
	return &OutboundLimiter{
		events:   events,
		window:   window,
		limiters: make(map[string]*rate.Limiter),
	}
}

// Wait blocks until the limiter for the given host has a token, ctx is
// done, or the wait would exceed ctx's deadline. A nil receiver is a
// no-op so call sites can wire `f.limiter.Wait(ctx, host)`
// unconditionally without nil-guarding.
func (l *OutboundLimiter) Wait(ctx context.Context, host string) error {
	if l == nil {
		return nil
	}
	if host == "" {
		// Empty host is a sentinel for "couldn't extract host from URL".
		// Don't share a single bucket between every malformed URL — that
		// would silently couple unrelated requests. Skip limiting.
		return nil
	}
	limiter := l.bucketFor(host)
	if err := limiter.Wait(ctx); err != nil {
		return fmt.Errorf("outbound limiter wait host=%s: %w", host, err)
	}
	return nil
}

func (l *OutboundLimiter) bucketFor(host string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.limiters[host]; ok {
		return existing
	}
	// rate.Every(window/events) gives 1 token every (window/events)
	// duration, which over `window` produces exactly `events` tokens.
	tokenInterval := l.window / time.Duration(l.events)
	limiter := rate.NewLimiter(rate.Every(tokenInterval), l.events)
	l.limiters[host] = limiter
	return limiter
}
