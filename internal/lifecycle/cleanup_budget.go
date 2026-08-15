// Package lifecycle contains low-level ownership primitives shared by process
// components that cannot depend on the application composition root.
package lifecycle

import (
	"context"
	"sync"
	"time"
)

type cleanupBudgetContextKey struct{}

// CleanupBudget lazily establishes one detached cleanup deadline for every
// nested owner participating in a failed lifecycle transition.
type CleanupBudget struct {
	mu       sync.Mutex
	timeout  time.Duration
	ctx      context.Context
	cancel   context.CancelFunc
	canceled bool
}

// WithCleanupBudget attaches a dormant cleanup budget to parent. The deadline
// is not started until CleanupContext is first called.
func WithCleanupBudget(parent context.Context, timeout time.Duration) (context.Context, *CleanupBudget) {
	budget := &CleanupBudget{timeout: timeout}
	return context.WithValue(parent, cleanupBudgetContextKey{}, budget), budget
}

// CleanupContext returns the shared detached cleanup context attached to
// parent. When no budget is attached, it returns a new detached fallback
// timeout so standalone lifecycle owners remain bounded.
func CleanupContext(parent context.Context, fallback time.Duration) (context.Context, context.CancelFunc) {
	budget, _ := parent.Value(cleanupBudgetContextKey{}).(*CleanupBudget)
	if budget == nil {
		return context.WithTimeout(context.WithoutCancel(parent), fallback)
	}
	return budget.context(parent), func() {}
}

// Cancel releases the shared deadline timer. It is safe to call more than
// once and before the budget has been activated.
func (b *CleanupBudget) Cancel() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.canceled = true
	cancel := b.cancel
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (b *CleanupBudget) context(parent context.Context) context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx == nil {
		b.ctx, b.cancel = context.WithTimeout(context.WithoutCancel(parent), b.timeout)
		if b.canceled {
			b.cancel()
		}
	}
	return b.ctx
}
