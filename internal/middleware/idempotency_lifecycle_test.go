package middleware

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type manualIdempotencyPurgeTicker struct {
	ticks    chan time.Time
	stopped  chan struct{}
	stopOnce sync.Once
}

func newManualIdempotencyPurgeTicker() *manualIdempotencyPurgeTicker {
	return &manualIdempotencyPurgeTicker{
		ticks:   make(chan time.Time, 1),
		stopped: make(chan struct{}),
	}
}

func (t *manualIdempotencyPurgeTicker) C() <-chan time.Time { return t.ticks }

func (t *manualIdempotencyPurgeTicker) Stop() {
	t.stopOnce.Do(func() { close(t.stopped) })
}

type purgeProbeStore struct {
	*fakeIdempotencyStore
	purge func(context.Context, time.Time) (int64, error)
}

func newPurgeProbeStore(purge func(context.Context, time.Time) (int64, error)) *purgeProbeStore {
	return &purgeProbeStore{
		fakeIdempotencyStore: newFakeIdempotencyStore(),
		purge:                purge,
	}
}

func (s *purgeProbeStore) PurgeExpired(ctx context.Context, now time.Time) (int64, error) {
	if s.purge != nil {
		return s.purge(ctx, now)
	}
	return s.fakeIdempotencyStore.PurgeExpired(ctx, now)
}

func TestNewPGIdempotencyCacheRequiresStore(t *testing.T) {
	t.Parallel()

	cache, err := NewPGIdempotencyCache(PGIdempotencyOptions{})
	if cache != nil {
		t.Fatal("NewPGIdempotencyCache(nil store) returned a cache")
	}
	if !errors.Is(err, ErrPGIdempotencyStoreRequired) {
		t.Fatalf("NewPGIdempotencyCache(nil store) error = %v, want ErrPGIdempotencyStoreRequired", err)
	}
}

func TestPGIdempotencyCacheConstructorDoesNotStartPurge(t *testing.T) {
	t.Parallel()

	ticker := newManualIdempotencyPurgeTicker()
	tickerCreated := make(chan time.Duration, 1)
	purged := make(chan time.Time, 1)
	store := newPurgeProbeStore(func(_ context.Context, now time.Time) (int64, error) {
		purged <- now
		return 0, nil
	})
	now := time.Unix(123, 0)
	cache, err := NewPGIdempotencyCache(PGIdempotencyOptions{
		Store: store,
		Now:   func() time.Time { return now },
		newTicker: func(interval time.Duration) idempotencyPurgeTicker {
			tickerCreated <- interval
			return ticker
		},
	})
	if err != nil {
		t.Fatalf("NewPGIdempotencyCache() error = %v", err)
	}

	select {
	case <-tickerCreated:
		t.Fatal("constructor created the purge ticker before Start")
	default:
	}
	select {
	case <-purged:
		t.Fatal("constructor purged before Start")
	default:
	}

	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case interval := <-tickerCreated:
		if interval != idempotencyPurgeInterval {
			t.Fatalf("purge ticker interval = %s, want %s", interval, idempotencyPurgeInterval)
		}
	default:
		t.Fatal("Start did not create the purge ticker")
	}

	ticker.ticks <- now
	select {
	case got := <-purged:
		if !got.Equal(now) {
			t.Fatalf("PurgeExpired now = %s, want %s", got, now)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not run purge after a ticker signal")
	}

	if err := cache.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case <-ticker.stopped:
	default:
		t.Fatal("Stop returned before the purge ticker stopped")
	}
}

func TestPGIdempotencyCacheLifecycleStateMachine(t *testing.T) {
	t.Parallel()

	t.Run("cancelled start remains constructed", func(t *testing.T) {
		ticker := newManualIdempotencyPurgeTicker()
		var tickerCreations atomic.Int32
		cache, err := NewPGIdempotencyCache(PGIdempotencyOptions{
			Store: newFakeIdempotencyStore(),
			newTicker: func(time.Duration) idempotencyPurgeTicker {
				tickerCreations.Add(1)
				return ticker
			},
		})
		if err != nil {
			t.Fatalf("NewPGIdempotencyCache() error = %v", err)
		}

		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := cache.Start(cancelledCtx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Start(cancelled context) error = %v, want context.Canceled", err)
		}
		if got := tickerCreations.Load(); got != 0 {
			t.Fatalf("cancelled Start created %d tickers, want 0", got)
		}

		if err := cache.Start(context.Background()); err != nil {
			t.Fatalf("Start() after cancelled Start error = %v", err)
		}
		if got := tickerCreations.Load(); got != 1 {
			t.Fatalf("successful Start created %d tickers, want 1", got)
		}
		if err := cache.Start(context.Background()); !errors.Is(err, ErrPGIdempotencyAlreadyStarted) {
			t.Fatalf("second Start() error = %v, want ErrPGIdempotencyAlreadyStarted", err)
		}
		if err := cache.Start(cancelledCtx); !errors.Is(err, ErrPGIdempotencyAlreadyStarted) {
			t.Fatalf("second Start(cancelled context) error = %v, want ErrPGIdempotencyAlreadyStarted", err)
		}

		if err := cache.Stop(context.Background()); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
		if err := cache.Start(context.Background()); !errors.Is(err, ErrPGIdempotencyStopped) {
			t.Fatalf("Start() after Stop error = %v, want ErrPGIdempotencyStopped", err)
		}
		if err := cache.Start(cancelledCtx); !errors.Is(err, ErrPGIdempotencyStopped) {
			t.Fatalf("Start(cancelled context) after Stop error = %v, want ErrPGIdempotencyStopped", err)
		}
		if err := cache.Stop(context.Background()); err != nil {
			t.Fatalf("second Stop() error = %v", err)
		}
	})

	t.Run("stop before start is terminal and idempotent", func(t *testing.T) {
		var tickerCreations atomic.Int32
		cache, err := NewPGIdempotencyCache(PGIdempotencyOptions{
			Store: newFakeIdempotencyStore(),
			newTicker: func(time.Duration) idempotencyPurgeTicker {
				tickerCreations.Add(1)
				return newManualIdempotencyPurgeTicker()
			},
		})
		if err != nil {
			t.Fatalf("NewPGIdempotencyCache() error = %v", err)
		}

		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := cache.Stop(cancelledCtx); err != nil {
			t.Fatalf("Stop() before Start error = %v", err)
		}
		if err := cache.Stop(context.Background()); err != nil {
			t.Fatalf("second Stop() error = %v", err)
		}
		if err := cache.Start(context.Background()); !errors.Is(err, ErrPGIdempotencyStopped) {
			t.Fatalf("Start() after Stop-before-Start error = %v, want ErrPGIdempotencyStopped", err)
		}
		if got := tickerCreations.Load(); got != 0 {
			t.Fatalf("terminal cache created %d tickers, want 0", got)
		}
	})
}

func TestPGIdempotencyCacheConcurrentStartSucceedsOnce(t *testing.T) {
	t.Parallel()

	ticker := newManualIdempotencyPurgeTicker()
	var tickerCreations atomic.Int32
	cache, err := NewPGIdempotencyCache(PGIdempotencyOptions{
		Store: newFakeIdempotencyStore(),
		newTicker: func(time.Duration) idempotencyPurgeTicker {
			tickerCreations.Add(1)
			return ticker
		},
	})
	if err != nil {
		t.Fatalf("NewPGIdempotencyCache() error = %v", err)
	}

	const callers = 16
	start := make(chan struct{})
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- cache.Start(context.Background())
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var successes int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrPGIdempotencyAlreadyStarted):
		default:
			t.Fatalf("concurrent Start() error = %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent Start calls = %d, want 1", successes)
	}
	if got := tickerCreations.Load(); got != 1 {
		t.Fatalf("concurrent Start created %d tickers, want 1", got)
	}
	if err := cache.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestPGIdempotencyCacheConcurrentStartAndStopLinearize(t *testing.T) {
	t.Parallel()

	for iteration := range 32 {
		ticker := newManualIdempotencyPurgeTicker()
		var tickerCreations atomic.Int32
		cache, err := NewPGIdempotencyCache(PGIdempotencyOptions{
			Store: newFakeIdempotencyStore(),
			newTicker: func(time.Duration) idempotencyPurgeTicker {
				tickerCreations.Add(1)
				return ticker
			},
		})
		if err != nil {
			t.Fatalf("iteration %d: NewPGIdempotencyCache() error = %v", iteration, err)
		}

		begin := make(chan struct{})
		startResult := make(chan error, 1)
		stopResult := make(chan error, 1)
		go func() {
			<-begin
			startResult <- cache.Start(context.Background())
		}()
		go func() {
			<-begin
			stopResult <- cache.Stop(context.Background())
		}()
		close(begin)

		startErr := <-startResult
		if startErr != nil && !errors.Is(startErr, ErrPGIdempotencyStopped) {
			t.Fatalf("iteration %d: concurrent Start() error = %v", iteration, startErr)
		}
		if stopErr := <-stopResult; stopErr != nil {
			t.Fatalf("iteration %d: concurrent Stop() error = %v", iteration, stopErr)
		}
		if err := cache.Stop(context.Background()); err != nil {
			t.Fatalf("iteration %d: final Stop() error = %v", iteration, err)
		}
		if err := cache.Start(context.Background()); !errors.Is(err, ErrPGIdempotencyStopped) {
			t.Fatalf("iteration %d: final Start() error = %v, want ErrPGIdempotencyStopped", iteration, err)
		}
		var wantTickerCreations int32
		if startErr == nil {
			wantTickerCreations = 1
		}
		if got := tickerCreations.Load(); got != wantTickerCreations {
			t.Fatalf("iteration %d: ticker creations = %d, want %d", iteration, got, wantTickerCreations)
		}
	}
}

func TestPGIdempotencyCacheOwnerCancellationCancelsBlockedPurge(t *testing.T) {
	t.Parallel()

	type ownerKey struct{}
	const ownerValue = "runtime-owner"
	entered := make(chan any, 1)
	finished := make(chan error, 1)
	store := newPurgeProbeStore(func(ctx context.Context, _ time.Time) (int64, error) {
		entered <- ctx.Value(ownerKey{})
		<-ctx.Done()
		finished <- ctx.Err()
		return 0, ctx.Err()
	})
	ticker := newManualIdempotencyPurgeTicker()
	cache, err := NewPGIdempotencyCache(PGIdempotencyOptions{
		Store:     store,
		newTicker: func(time.Duration) idempotencyPurgeTicker { return ticker },
	})
	if err != nil {
		t.Fatalf("NewPGIdempotencyCache() error = %v", err)
	}
	ownerCtx, cancelOwner := context.WithCancel(context.WithValue(context.Background(), ownerKey{}, ownerValue))
	if err := cache.Start(ownerCtx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	ticker.ticks <- time.Now()
	select {
	case got := <-entered:
		if got != ownerValue {
			t.Fatalf("PurgeExpired owner value = %v, want %q", got, ownerValue)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PurgeExpired did not reach the blocking barrier")
	}
	cancelOwner()

	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("PurgeExpired context error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner cancellation did not unblock PurgeExpired")
	}
	if err := cache.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() after owner cancellation error = %v", err)
	}
	select {
	case <-ticker.stopped:
	default:
		t.Fatal("Stop returned before the owner-cancelled loop stopped")
	}
}

func TestPGIdempotencyCacheStopDeadlineCancelsBlockedPurgeAndJoins(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	finished := make(chan error, 1)
	store := newPurgeProbeStore(func(ctx context.Context, _ time.Time) (int64, error) {
		close(entered)
		<-ctx.Done()
		finished <- ctx.Err()
		return 0, ctx.Err()
	})
	ticker := newManualIdempotencyPurgeTicker()
	cache, err := NewPGIdempotencyCache(PGIdempotencyOptions{
		Store:     store,
		newTicker: func(time.Duration) idempotencyPurgeTicker { return ticker },
	})
	if err != nil {
		t.Fatalf("NewPGIdempotencyCache() error = %v", err)
	}
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	ticker.ticks <- time.Now()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("PurgeExpired did not reach the blocking barrier")
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelStop()
	if err := cache.Stop(stopCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v, want context.DeadlineExceeded", err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("PurgeExpired context was not cancelled by the Stop deadline")
		}
	default:
		t.Fatal("Stop returned before the blocked purge goroutine finished")
	}
	select {
	case <-ticker.stopped:
	default:
		t.Fatal("Stop returned before the purge ticker stopped")
	}
	if err := cache.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
}
