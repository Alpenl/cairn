package retry

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Policy owns the retry timing shared by outbound clients. Callers retain
// retryability classification; Policy decides when a retry should run.
type Policy struct {
	baseDelay time.Duration
	maxDelay  time.Duration
}

// NewPolicy constructs a retry timing policy.
func NewPolicy(baseDelay, maxDelay time.Duration) Policy {
	return Policy{baseDelay: baseDelay, maxDelay: maxDelay}
}

// Delay returns the jittered delay before the next attempt. attempt is
// zero-based: attempt 0 waits baseDelay, attempt 1 waits twice baseDelay.
func (p Policy) Delay(attempt int, retryAfter string) time.Duration {
	if delay, ok := retryAfterDelay(retryAfter); ok {
		if p.maxDelay > 0 && delay > p.maxDelay {
			delay = p.maxDelay
		}
		return jitterDelay(delay)
	}

	delay := p.baseDelay
	if delay < 0 {
		delay = 0
	}
	if p.maxDelay > 0 && delay > p.maxDelay {
		delay = p.maxDelay
	}
	for i := 0; i < attempt; i++ {
		if p.maxDelay > 0 && delay >= p.maxDelay {
			delay = p.maxDelay
			break
		}
		delay *= 2
		if p.maxDelay > 0 && delay > p.maxDelay {
			delay = p.maxDelay
			break
		}
	}
	return jitterDelay(delay)
}

// Wait blocks until delay elapses or ctx is canceled.
func (Policy) Wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func retryAfterDelay(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	seconds, err := strconv.Atoi(value)
	if err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := time.Until(retryAt)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}
