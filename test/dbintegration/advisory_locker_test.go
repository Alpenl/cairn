package dbintegration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/service/urllock"
)

// TestAdvisoryURLLockerSerializesAcrossInstances proves that two locker
// instances backed by different PostgreSQL pools cannot enter the same URL's
// critical section concurrently. It also covers the normal unlock path: once
// the first callback returns, the waiter acquires the lock and completes.
func TestAdvisoryURLLockerSerializesAcrossInstances(t *testing.T) {
	poolA := StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	poolB, err := pgxpool.New(ctx, DSN(t))
	if err != nil {
		t.Fatalf("open second PostgreSQL pool: %v", err)
	}
	t.Cleanup(poolB.Close)

	const (
		lockClass int32 = 712113
		lockKey         = "https://example.com/shared-advisory-lock"
	)
	lockerA := urllock.NewAdvisoryURLLocker(poolA, lockClass)
	lockerB := urllock.NewAdvisoryURLLocker(poolB, lockClass)

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- lockerA.WithURL(ctx, lockKey, func(callbackCtx context.Context) error {
			close(firstEntered)
			select {
			case <-releaseFirst:
				return nil
			case <-callbackCtx.Done():
				return callbackCtx.Err()
			}
		})
	}()

	select {
	case <-firstEntered:
	case <-ctx.Done():
		t.Fatalf("first locker did not enter critical section: %v", ctx.Err())
	}

	secondAttempted := make(chan struct{})
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondAttempted)
		secondDone <- lockerB.WithURL(ctx, lockKey, func(context.Context) error {
			close(secondEntered)
			return nil
		})
	}()
	<-secondAttempted

	// Observe PostgreSQL itself reporting an ungranted advisory lock. This is
	// stronger than relying on a short sleep: it proves the second connection
	// reached pg_advisory_lock and is blocked behind the first session.
	waitForAdvisoryWaiter(t, ctx, poolA, lockClass)
	select {
	case <-secondEntered:
		t.Fatal("second locker entered while the first still held the same key")
	default:
	}

	close(releaseFirst)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first locker returned error: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("first locker did not release normally: %v", ctx.Err())
	}

	select {
	case <-secondEntered:
	case <-ctx.Done():
		t.Fatalf("second locker did not acquire after release: %v", ctx.Err())
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second locker returned error: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("second locker did not complete: %v", ctx.Err())
	}
}

func TestAdvisoryURLLockersShareConnectionBudget(t *testing.T) {
	_ = StartPostgres(t)
	pool := openPoolWithMaxConns(t, 2, "advisory-budget")
	gate := urllock.NewAdvisoryLockGate(1)
	first := urllock.NewAdvisoryURLLockerWithGate(pool, urllock.AdvisoryLockClassSubmit, gate)
	second := urllock.NewAdvisoryURLLockerWithGate(pool, urllock.AdvisoryLockClassSubmit+1, gate)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.WithURL(ctx, "https://example.com/first", func(callbackCtx context.Context) error {
			close(firstEntered)
			select {
			case <-releaseFirst:
			case <-callbackCtx.Done():
				return callbackCtx.Err()
			}
			var one int
			return pool.QueryRow(callbackCtx, "SELECT 1").Scan(&one)
		})
	}()

	select {
	case <-firstEntered:
	case <-ctx.Done():
		t.Fatalf("first locker did not enter: %v", ctx.Err())
	}
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- second.WithURL(ctx, "https://example.com/second", func(callbackCtx context.Context) error {
			close(secondEntered)
			var one int
			return pool.QueryRow(callbackCtx, "SELECT 1").Scan(&one)
		})
	}()

	select {
	case <-secondEntered:
		t.Fatal("second locker entered while the shared connection budget was occupied")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first locker callback query failed: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("first locker could not use the reserved callback connection: %v", ctx.Err())
	}
	select {
	case <-secondEntered:
	case <-ctx.Done():
		t.Fatalf("second locker did not enter after budget release: %v", ctx.Err())
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second locker callback query failed: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("second locker did not finish: %v", ctx.Err())
	}
}

func TestAdvisoryURLLockerWithURLsReleasesWholeSet(t *testing.T) {
	pool := StartPostgres(t)
	const lockClass int32 = 712114
	locker := urllock.NewAdvisoryURLLocker(pool, lockClass)
	urls := []string{
		"https://example.com/multi-lock-a",
		"https://example.com/multi-lock-b",
		"https://example.com/multi-lock-c",
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	var held int
	err := locker.WithURLs(ctx, urls, func(callbackCtx context.Context) error {
		return pool.QueryRow(callbackCtx, `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND classid::bigint = $1 AND granted`, lockClass).Scan(&held)
	})
	if err != nil {
		t.Fatalf("WithURLs() error = %v", err)
	}
	if held != len(urls) {
		t.Fatalf("held advisory locks = %d, want %d", held, len(urls))
	}

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND classid::bigint = $1 AND granted`, lockClass).Scan(&remaining); err != nil {
		t.Fatalf("count remaining advisory locks: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("remaining advisory locks = %d, want 0 after WithURLs", remaining)
	}
}

// TestAdvisoryURLLockerDoesNotDeadlockAgainstBatchURLLocks guards the unified
// lock order with a two-connection pool:
//
//	single: session advisory lock -> wait for a callback DB connection
//	batch:  wait for the same session URL lock -> open its DB transaction
//
// Batch cannot occupy the callback connection while waiting for the URL because
// WithURLs acquires the complete lock set before invoking the transaction.
func TestAdvisoryURLLockerDoesNotDeadlockAgainstBatchURLLocks(t *testing.T) {
	_ = StartPostgres(t)
	pool := openPoolWithMaxConns(t, 2, "advisory-lock-order")
	locker := urllock.NewAdvisoryURLLockerWithGate(
		pool,
		urllock.AdvisoryLockClassSubmit,
		urllock.NewAdvisoryLockGate(1),
	)
	const rawURL = "https://example.com/lock-order"

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	singleEntered := make(chan struct{})
	batchAttempted := make(chan struct{})
	singleDone := make(chan error, 1)
	batchDone := make(chan error, 1)

	go func() {
		singleDone <- locker.WithURL(ctx, rawURL, func(callbackCtx context.Context) error {
			close(singleEntered)
			select {
			case <-batchAttempted:
			case <-callbackCtx.Done():
				return callbackCtx.Err()
			}
			_, err := pool.Exec(callbackCtx, "SELECT 1")
			return err
		})
	}()

	select {
	case <-singleEntered:
	case <-ctx.Done():
		t.Fatalf("single did not acquire its session lock: %v", ctx.Err())
	}

	go func() {
		close(batchAttempted)
		batchDone <- locker.WithURLs(ctx, []string{rawURL}, func(callbackCtx context.Context) error {
			tx, err := pool.Begin(callbackCtx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := tx.Exec(callbackCtx, "SELECT 1"); err != nil {
				return err
			}
			return tx.Commit(callbackCtx)
		})
	}()

	if err := <-singleDone; err != nil {
		t.Fatalf("single callback was trapped in the lock-order cycle: %v", err)
	}
	if err := <-batchDone; err != nil {
		t.Fatalf("batch transaction was trapped in the lock-order cycle: %v", err)
	}
}

func waitForAdvisoryWaiter(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lockClass int32) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		var waiters int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks
			 WHERE locktype = 'advisory'
			   AND classid::bigint = $1
			   AND NOT granted`,
			lockClass,
		).Scan(&waiters); err != nil {
			t.Fatalf("inspect advisory lock waiters: %v", err)
		}
		if waiters > 0 {
			return
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("second locker never waited in PostgreSQL: %v", ctx.Err())
		}
	}
}

func openPoolWithMaxConns(t *testing.T, maxConns int32, applicationName string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(DSN(t))
	if err != nil {
		t.Fatalf("parse PostgreSQL pool config: %v", err)
	}
	config.MaxConns = maxConns
	config.ConnConfig.RuntimeParams["application_name"] = applicationName
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("open PostgreSQL pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
