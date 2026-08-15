// Package retry owns the timing policy shared by outbound HTTP clients.
package retry

import (
	"math/rand/v2"
	"time"
)

// jitterDelay returns delay perturbed by +/-20% drawn uniformly from
// [-delay/5, +delay/5). Falls back to the bare delay when the jitter
// window collapses to zero (delay < 5 ns is effectively 0). Safe for
// concurrent use because math/rand/v2 uses a per-goroutine source.
func jitterDelay(delay time.Duration) time.Duration {
	jitterWindow := int64(delay) * 2 / 5
	if jitterWindow <= 0 {
		return delay
	}
	return delay + time.Duration(rand.Int64N(jitterWindow)) - delay/5
}
