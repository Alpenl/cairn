package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBuildRuntimeCleanupDetachesCanceledContextWithOneSharedDeadline(t *testing.T) {
	t.Parallel()

	type cleanupValueKey struct{}
	parent, cancelParent := context.WithTimeout(
		context.WithValue(context.Background(), cleanupValueKey{}, "build-value"),
		time.Millisecond,
	)
	cancelParent()

	var (
		acquisitions runtimeBuildAcquisitions
		deadlines    []time.Time
	)
	for _, name := range []string{"tracer", "persistence"} {
		name := name
		if err := acquisitions.Acquire(name, func() (runtimeAcquiredResource, error) {
			cleanup := func(cleanupCtx context.Context) error {
				if err := cleanupCtx.Err(); err != nil {
					t.Errorf("%s cleanup context error = %v, want nil", name, err)
				}
				if cause := context.Cause(cleanupCtx); cause != nil {
					t.Errorf("%s cleanup context cause = %v, want nil", name, cause)
				}
				if got := cleanupCtx.Value(cleanupValueKey{}); got != "build-value" {
					t.Errorf("%s cleanup context value = %v, want build-value", name, got)
				}
				deadline, ok := cleanupCtx.Deadline()
				if !ok {
					t.Errorf("%s cleanup context has no deadline", name)
				}
				deadlines = append(deadlines, deadline)
				return nil
			}
			return runtimeAcquiredResource{resource: name, cleanup: cleanup}, nil
		}); err != nil {
			t.Fatalf("Acquire(%s) error = %v", name, err)
		}
	}

	primaryErr := errors.New("later constructor failed")
	finalErr, cleanupErr := acquisitions.CleanupFailure(parent, time.Second, primaryErr)
	if cleanupErr != nil {
		t.Fatalf("CleanupFailure() cleanup error = %v, want nil", cleanupErr)
	}
	if !errors.Is(finalErr, primaryErr) {
		t.Fatalf("CleanupFailure() error = %v, want primary error", finalErr)
	}
	if len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) {
		t.Fatalf("cleanup deadlines = %v, want one shared deadline", deadlines)
	}
	remaining := time.Until(deadlines[0])
	if remaining < 500*time.Millisecond || remaining > 1100*time.Millisecond {
		t.Fatalf("cleanup deadline remaining = %v, want detached ~1s budget", remaining)
	}
}

func TestBuildRuntimeCleanupJoinsPrimaryAndEveryCleanupError(t *testing.T) {
	t.Parallel()

	primaryErr := errors.New("constructor failed")
	firstErr := errors.New("tracer shutdown failed")
	secondErr := errors.New("persistence close failed")
	var acquisitions runtimeBuildAcquisitions
	for name, cleanupErr := range map[string]error{
		"tracer":      firstErr,
		"persistence": secondErr,
	} {
		cleanupErr := cleanupErr
		if err := acquisitions.Acquire(name, func() (runtimeAcquiredResource, error) {
			return runtimeAcquiredResource{
				resource: name,
				cleanup:  func(context.Context) error { return cleanupErr },
			}, nil
		}); err != nil {
			t.Fatalf("Acquire(%s) error = %v", name, err)
		}
	}

	finalErr, cleanupErr := acquisitions.CleanupFailure(context.Background(), time.Second, primaryErr)
	for index, wantErr := range []error{primaryErr, firstErr, secondErr} {
		if !errors.Is(finalErr, wantErr) {
			t.Fatalf("CleanupFailure() error = %v, want %v", finalErr, wantErr)
		}
		if index > 0 && !errors.Is(cleanupErr, wantErr) {
			t.Fatalf("cleanup error = %v, want %v", cleanupErr, wantErr)
		}
	}
}

func TestBuildRuntimeOwnsCleanupReturnedWithConstructorError(t *testing.T) {
	t.Parallel()

	constructorErr := errors.New("constructor failed after opening resource")
	cleanupCalls := 0
	var acquisitions runtimeBuildAcquisitions
	err := acquisitions.Acquire("partial resource", func() (runtimeAcquiredResource, error) {
		return runtimeAcquiredResource{
			resource: "partial resource",
			cleanup: func(context.Context) error {
				cleanupCalls++
				return nil
			},
		}, constructorErr
	})
	if !errors.Is(err, constructorErr) {
		t.Fatalf("Acquire() error = %v, want %v", err, constructorErr)
	}

	finalErr, cleanupErr := acquisitions.CleanupFailure(context.Background(), time.Second, err)
	if !errors.Is(finalErr, constructorErr) || cleanupErr != nil {
		t.Fatalf("CleanupFailure() = (%v, %v), want constructor error and nil cleanup error", finalErr, cleanupErr)
	}
	if cleanupCalls != 1 {
		t.Fatalf("partial resource cleanup calls = %d, want 1", cleanupCalls)
	}
}

func TestBuildRuntimeSuccessfulAcquisitionTransfersOwnership(t *testing.T) {
	t.Parallel()

	cleanupCalls := 0
	var acquisitions runtimeBuildAcquisitions
	if err := acquisitions.Acquire("persistence", func() (runtimeAcquiredResource, error) {
		return runtimeAcquiredResource{
			resource: "persistence",
			cleanup: func(context.Context) error {
				cleanupCalls++
				return nil
			},
		}, nil
	}); err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	finalErr, cleanupErr := acquisitions.CleanupFailure(context.Background(), time.Second, nil)
	if finalErr != nil || cleanupErr != nil {
		t.Fatalf("CleanupFailure(success) = (%v, %v), want nil, nil", finalErr, cleanupErr)
	}
	if cleanupCalls != 0 {
		t.Fatalf("cleanup calls after ownership transfer = %d, want 0", cleanupCalls)
	}
	if len(acquisitions.owned.actions) != 0 {
		t.Fatalf("build cleanup stack length = %d, want disarmed", len(acquisitions.owned.actions))
	}
}
