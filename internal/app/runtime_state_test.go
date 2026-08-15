package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeStartSucceedsOnlyOnce(t *testing.T) {
	t.Parallel()

	starts := 0
	runtime := &Runtime{
		start: func(context.Context) error {
			starts++
			return nil
		},
	}

	if err := runtime.Start(context.Background()); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	if err := runtime.Start(context.Background()); !errors.Is(err, ErrRuntimeAlreadyStarted) {
		t.Fatalf("second Start() error = %v, want ErrRuntimeAlreadyStarted", err)
	}
	if starts != 1 {
		t.Fatalf("start callback calls = %d, want 1", starts)
	}
}

func TestRuntimeCloseBeforeStartReleasesOnceAndIsIdempotent(t *testing.T) {
	t.Parallel()

	closes := 0
	runtime := &Runtime{
		close: func(context.Context) error {
			closes++
			return nil
		},
	}

	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if closes != 1 {
		t.Fatalf("close callback calls = %d, want 1", closes)
	}
	if err := runtime.Start(context.Background()); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("Start() after Close error = %v, want ErrRuntimeClosed", err)
	}
}

func TestRuntimeFailedStartCannotBeRetried(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("background start failed")
	starts := 0
	runtime := &Runtime{
		start: func(context.Context) error {
			starts++
			return wantErr
		},
	}

	if err := runtime.Start(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("first Start() error = %v, want %v", err, wantErr)
	}
	if err := runtime.Start(context.Background()); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("second Start() error = %v, want ErrRuntimeClosed", err)
	}
	if starts != 1 {
		t.Fatalf("start callback calls = %d, want 1", starts)
	}
}

func TestRuntimeBackgroundLifetimeDoesNotInheritStartCallerCancellation(t *testing.T) {
	t.Parallel()

	type contextKey string
	const key contextKey = "runtime-id"
	startCtx, cancelStart := context.WithCancel(context.WithValue(context.Background(), key, "rf7"))
	var backgroundCtx context.Context
	runtime := &Runtime{
		start: func(ctx context.Context) error {
			backgroundCtx = ctx
			return nil
		},
	}

	if err := runtime.Start(startCtx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	cancelStart()
	if backgroundCtx == nil {
		t.Fatal("start callback did not receive a context")
	}
	if got := backgroundCtx.Value(key); got != "rf7" {
		t.Fatalf("background context value = %v, want rf7", got)
	}
	if err := backgroundCtx.Err(); err != nil {
		t.Fatalf("background context inherited caller cancellation: %v", err)
	}
}

func TestRuntimeCloseWaitingForStartHonorsItsDeadline(t *testing.T) {
	t.Parallel()

	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	startDone := make(chan error, 1)
	closes := 0
	runtime := &Runtime{
		start: func(context.Context) error {
			close(startEntered)
			<-releaseStart
			return nil
		},
		close: func(context.Context) error {
			closes++
			return nil
		},
	}

	go func() { startDone <- runtime.Start(context.Background()) }()
	<-startEntered

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelClose()
	closeDone := make(chan error, 1)
	go func() { closeDone <- runtime.Close(closeCtx) }()
	select {
	case err := <-closeDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close() error = %v, want context deadline exceeded", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Close() remained blocked behind Start after its deadline")
	}

	close(releaseStart)
	if err := <-startDone; err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("Close() after Start completed error = %v", err)
	}
	if closes != 1 {
		t.Fatalf("close callback calls = %d, want 1", closes)
	}
}

func TestRuntimeConcurrentStartExecutesCallbackOnce(t *testing.T) {
	t.Parallel()

	const callers = 32
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	var startCalls atomic.Int32
	runtime := &Runtime{
		start: func(context.Context) error {
			startCalls.Add(1)
			close(startEntered)
			<-releaseStart
			return nil
		},
	}

	results := make(chan error, callers)
	go func() { results <- runtime.Start(t.Context()) }()
	<-startEntered
	for range callers - 1 {
		go func() { results <- runtime.Start(t.Context()) }()
	}

	for range callers - 1 {
		if err := <-results; !errors.Is(err, ErrRuntimeAlreadyStarted) {
			t.Fatalf("concurrent Start() error = %v, want ErrRuntimeAlreadyStarted", err)
		}
	}
	close(releaseStart)
	if err := <-results; err != nil {
		t.Fatalf("winning Start() error = %v", err)
	}
	if got := startCalls.Load(); got != 1 {
		t.Fatalf("start callback calls = %d, want 1", got)
	}
}

func TestRuntimeConcurrentCloseExecutesCallbackOnceAndReplaysTerminalError(t *testing.T) {
	t.Parallel()

	const callers = 32
	wantErr := errors.New("terminal close failure")
	closeEntered := make(chan struct{})
	releaseClose := make(chan struct{})
	var closeCalls atomic.Int32
	runtime := &Runtime{
		close: func(context.Context) error {
			closeCalls.Add(1)
			close(closeEntered)
			<-releaseClose
			return wantErr
		},
	}

	results := make(chan error, callers)
	go func() { results <- runtime.Close(t.Context()) }()
	<-closeEntered
	var ready sync.WaitGroup
	ready.Add(callers - 1)
	begin := make(chan struct{})
	for range callers - 1 {
		go func() {
			ready.Done()
			<-begin
			results <- runtime.Close(t.Context())
		}()
	}
	ready.Wait()
	close(begin)
	close(releaseClose)

	for range callers {
		if err := <-results; err != wantErr { //nolint:errorlint // exact terminal error identity must be replayed
			t.Fatalf("concurrent Close() error = %v, want original terminal error %v", err, wantErr)
		}
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("close callback calls = %d, want 1", got)
	}
	if err := runtime.Close(t.Context()); err != wantErr { //nolint:errorlint // exact terminal error identity must be replayed
		t.Fatalf("later Close() error = %v, want original terminal error %v", err, wantErr)
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("close callback calls after replay = %d, want 1", got)
	}
}

func TestRuntimeConcurrentCloseWaitersHonorContextWhileCallbackRuns(t *testing.T) {
	t.Parallel()

	const waiters = 16
	closeEntered := make(chan struct{})
	releaseClose := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseClose) }) }
	defer release()
	var closeCalls atomic.Int32
	runtime := &Runtime{
		close: func(context.Context) error {
			closeCalls.Add(1)
			close(closeEntered)
			<-releaseClose
			return nil
		},
	}

	winner := make(chan error, 1)
	go func() { winner <- runtime.Close(t.Context()) }()
	<-closeEntered

	waitCtx, cancelWaiters := context.WithCancel(t.Context())
	waiterResults := make(chan error, waiters)
	for range waiters {
		go func() { waiterResults <- runtime.Close(waitCtx) }()
	}
	cancelWaiters()
	for range waiters {
		select {
		case err := <-waiterResults:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Close() waiter error = %v, want context canceled", err)
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatal("Close() waiter did not honor its context while terminal callback was running")
		}
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("close callback calls while waiters canceled = %d, want 1", got)
	}

	release()
	if err := <-winner; err != nil {
		t.Fatalf("winning Close() error = %v", err)
	}
}
