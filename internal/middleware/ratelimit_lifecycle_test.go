package middleware

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRateLimiterControllerLifecycle(t *testing.T) {
	t.Parallel()

	_, ctrl := RateLimit(RateLimitOptions{
		RPS:           1,
		Burst:         1,
		SweepInterval: time.Hour,
	})
	if ctrl.state != rateLimiterConstructed {
		t.Fatalf("state after construction = %v, want constructed", ctrl.state)
	}
	if ctrl.sweeperCancel != nil {
		t.Fatal("constructor installed a sweeper cancel function; background ownership must begin in Start")
	}

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ctrl.Start(cancelledCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start(cancelled context) error = %v, want context.Canceled", err)
	}
	if ctrl.state != rateLimiterConstructed {
		t.Fatalf("state after cancelled Start = %v, want constructed", ctrl.state)
	}

	if err := ctrl.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := ctrl.Start(context.Background()); !errors.Is(err, ErrRateLimiterAlreadyStarted) {
		t.Fatalf("second Start() error = %v, want ErrRateLimiterAlreadyStarted", err)
	}
	if err := ctrl.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := ctrl.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() error = %v, want nil", err)
	}
	if err := ctrl.Start(context.Background()); !errors.Is(err, ErrRateLimiterStopped) {
		t.Fatalf("Start() after Stop error = %v, want ErrRateLimiterStopped", err)
	}
}

func TestRateLimiterControllerStopBeforeStart(t *testing.T) {
	t.Parallel()

	_, ctrl := RateLimit(RateLimitOptions{RPS: 1, Burst: 1})
	if err := ctrl.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() before Start error = %v", err)
	}
	if err := ctrl.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	if err := ctrl.Start(context.Background()); !errors.Is(err, ErrRateLimiterStopped) {
		t.Fatalf("Start() after Stop-before-Start error = %v, want ErrRateLimiterStopped", err)
	}
}

func TestRateLimiterSweeperExitsOnOwnerCancellation(t *testing.T) {
	t.Parallel()

	_, ctrl := RateLimit(RateLimitOptions{
		RPS:           1,
		Burst:         1,
		SweepInterval: time.Hour,
	})
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	if err := ctrl.Start(ownerCtx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	cancelOwner()
	select {
	case <-ctrl.limiter.doneCh:
	case <-time.After(time.Second):
		stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
		defer cancelStop()
		_ = ctrl.Stop(stopCtx)
		t.Fatal("rate limiter sweeper did not exit after owner cancellation")
	}

	joinCtx, cancelJoin := context.WithTimeout(context.Background(), time.Second)
	defer cancelJoin()
	if err := ctrl.Stop(joinCtx); err != nil {
		t.Fatalf("Stop() after owner cancellation error = %v", err)
	}
}

func TestRateLimiterControllerStopHonorsDeadline(t *testing.T) {
	t.Parallel()

	ctrl := &RateLimiterController{
		limiter: &ipRateLimiter{
			stopCh: make(chan struct{}),
			doneCh: make(chan struct{}),
		},
		state:         rateLimiterStarted,
		sweeperCancel: func() {},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	startedAt := time.Now()
	err := ctrl.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("Stop() took %v, want a deadline-bounded return", elapsed)
	}
}
