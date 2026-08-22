package dbintegration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/database"
)

func TestClosePoolReturnsWhenCallerDeadlineExpiresWithCheckedOutConnection(t *testing.T) {
	_ = StartPostgres(t)

	pool, err := pgxpool.New(t.Context(), DSN(t))
	if err != nil {
		t.Fatalf("open independent pool: %v", err)
	}
	t.Cleanup(pool.Close)

	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire held connection: %v", err)
	}
	released := false
	defer func() {
		if !released {
			conn.Release()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	err = database.ClosePool(ctx, pool)
	elapsed := time.Since(startedAt)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ClosePool() error = %v, want context deadline exceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ClosePool() elapsed = %v, want caller deadline to bound wait", elapsed)
	}

	acquireCtx, cancelAcquire := context.WithTimeout(context.Background(), time.Second)
	defer cancelAcquire()
	if _, err := pool.Acquire(acquireCtx); err == nil {
		t.Fatal("Acquire() after ClosePool began succeeded; want pool closing error")
	}

	conn.Release()
	released = true

	waitCtx, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	if err := database.ClosePool(waitCtx, pool); err != nil {
		t.Fatalf("ClosePool() after releasing held connection = %v, want nil", err)
	}
}

func TestConcurrentClosePoolCallersHonorTheirOwnContext(t *testing.T) {
	_ = StartPostgres(t)

	pool, err := pgxpool.New(t.Context(), DSN(t))
	if err != nil {
		t.Fatalf("open independent pool: %v", err)
	}
	t.Cleanup(pool.Close)

	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire held connection: %v", err)
	}
	released := false
	defer func() {
		if !released {
			conn.Release()
		}
	}()

	shortCtx, cancelShort := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelShort()
	longCtx, cancelLong := context.WithTimeout(context.Background(), time.Second)
	defer cancelLong()

	shortResult := make(chan error, 1)
	longResult := make(chan error, 1)
	go func() { shortResult <- database.ClosePool(shortCtx, pool) }()
	go func() { longResult <- database.ClosePool(longCtx, pool) }()

	select {
	case err := <-shortResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("short ClosePool() error = %v, want context deadline exceeded", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("short ClosePool() caller did not return at its deadline")
	}

	conn.Release()
	released = true

	select {
	case err := <-longResult:
		if err != nil {
			t.Fatalf("long ClosePool() error = %v, want nil after held connection is released", err)
		}
	case <-time.After(time.Second):
		t.Fatal("long ClosePool() caller did not complete after held connection was released")
	}
}

func TestClosePoolWaitsForNormalPoolClose(t *testing.T) {
	_ = StartPostgres(t)

	pool, err := pgxpool.New(t.Context(), DSN(t))
	if err != nil {
		t.Fatalf("open independent pool: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := database.ClosePool(ctx, pool); err != nil {
		t.Fatalf("ClosePool() normal path error = %v", err)
	}

	acquireCtx, cancelAcquire := context.WithTimeout(context.Background(), time.Second)
	defer cancelAcquire()
	if _, err := pool.Acquire(acquireCtx); err == nil {
		t.Fatal("Acquire() after normal ClosePool succeeded; want pool closing error")
	}
}
