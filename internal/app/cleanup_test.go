package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestCleanupStackRunsEveryActionInReverseOrderAndJoinsErrors(t *testing.T) {
	t.Parallel()

	firstErr := errors.New("first cleanup failed")
	thirdErr := errors.New("third cleanup failed")
	var order []string

	var stack cleanupStack
	stack.Add("first", func(context.Context) error {
		order = append(order, "first")
		return firstErr
	})
	stack.Add("second", func(context.Context) error {
		order = append(order, "second")
		return nil
	})
	stack.Add("third", func(context.Context) error {
		order = append(order, "third")
		return thirdErr
	})

	err := stack.Run(context.Background())
	if !reflect.DeepEqual(order, []string{"third", "second", "first"}) {
		t.Fatalf("cleanup order = %v, want [third second first]", order)
	}
	if !errors.Is(err, firstErr) || !errors.Is(err, thirdErr) {
		t.Fatalf("cleanup error = %v, want both cleanup failures", err)
	}
	if got := err.Error(); got != "cleanup third: third cleanup failed\ncleanup first: first cleanup failed" {
		t.Fatalf("cleanup error text = %q", got)
	}
}

func TestCleanupStackDetachedRunIgnoresParentCancellationButKeepsDeadline(t *testing.T) {
	t.Parallel()

	type contextKey string
	const key contextKey = "build-id"
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key, "rf7"))
	cancel()

	var stack cleanupStack
	stack.Add("probe", func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			t.Fatalf("cleanup context was already cancelled: %v", err)
		}
		if got := ctx.Value(key); got != "rf7" {
			t.Fatalf("cleanup context value = %v, want rf7", got)
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("cleanup context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > 250*time.Millisecond {
			t.Fatalf("cleanup deadline remaining = %v, want (0, 250ms]", remaining)
		}
		return nil
	})

	if err := stack.RunDetached(parent, 250*time.Millisecond); err != nil {
		t.Fatalf("RunDetached() error = %v", err)
	}
}

func TestCleanupStackIsOneShotBeforeCallbacksRun(t *testing.T) {
	t.Parallel()

	var calls int
	var stack cleanupStack
	stack.Add("only", func(ctx context.Context) error {
		calls++
		return stack.Run(ctx)
	})

	if err := stack.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("cleanup callback calls = %d, want 1", calls)
	}
}
