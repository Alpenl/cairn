package retry_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"webtag/internal/retry"
)

func TestPolicyDelayUsesCappedExponentialBackoff(t *testing.T) {
	t.Parallel()

	policy := retry.NewPolicy(100*time.Millisecond, 500*time.Millisecond)
	assertDelayAround(t, policy.Delay(0, ""), 100*time.Millisecond)
	assertDelayAround(t, policy.Delay(2, ""), 400*time.Millisecond)
	assertDelayAround(t, policy.Delay(10, ""), 500*time.Millisecond)
}

func TestPolicyDelayAppliesBoundedJitter(t *testing.T) {
	t.Parallel()

	policy := retry.NewPolicy(time.Second, 30*time.Second)
	first := policy.Delay(0, "")
	assertDelayAround(t, first, time.Second)
	for i := 0; i < 100; i++ {
		delay := policy.Delay(0, "")
		assertDelayAround(t, delay, time.Second)
		if delay != first {
			return
		}
	}
	t.Fatal("Delay() returned the same jittered value 101 times")
}

func TestPolicyDelayHonorsRetryAfterSeconds(t *testing.T) {
	t.Parallel()

	policy := retry.NewPolicy(100*time.Millisecond, 30*time.Second)
	assertDelayAround(t, policy.Delay(10, "7"), 7*time.Second)
}

func TestPolicyDelayCapsRetryAfter(t *testing.T) {
	t.Parallel()

	policy := retry.NewPolicy(100*time.Millisecond, 30*time.Second)
	assertDelayAround(t, policy.Delay(0, "300"), 30*time.Second)
}

func TestPolicyDelayHonorsRetryAfterHTTPDate(t *testing.T) {
	t.Parallel()

	retryAt := time.Now().UTC().Add(10 * time.Second).Truncate(time.Second)
	policy := retry.NewPolicy(100*time.Millisecond, 30*time.Second)
	center := time.Until(retryAt)
	assertDelayAround(t, policy.Delay(10, retryAt.Format(http.TimeFormat)), center)
}

func TestPolicyWaitStopsWhenContextIsCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	policy := retry.NewPolicy(time.Second, 30*time.Second)
	started := time.Now()
	err := policy.Wait(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("Wait() took %v after cancellation", elapsed)
	}
}

func assertDelayAround(t *testing.T, got, center time.Duration) {
	t.Helper()
	lower := center - center/5
	upper := center + center/5
	if got < lower || got >= upper {
		t.Fatalf("delay = %v, want in [%v, %v)", got, lower, upper)
	}
}
