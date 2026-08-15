//go:build dbintegration

package dbintegration

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/database"
)

const poolShutdownSafetyRelease = 750 * time.Millisecond

type terminateWriteBarrier struct {
	armed         atomic.Bool
	writeStarted  chan struct{}
	deadlineSet   chan struct{}
	safetyRelease chan struct{}
	writeOnce     sync.Once
	deadlineOnce  sync.Once
	releaseOnce   sync.Once
}

func newTerminateWriteBarrier() *terminateWriteBarrier {
	return &terminateWriteBarrier{
		writeStarted:  make(chan struct{}),
		deadlineSet:   make(chan struct{}),
		safetyRelease: make(chan struct{}),
	}
}

func (b *terminateWriteBarrier) arm() {
	b.armed.Store(true)
	time.AfterFunc(poolShutdownSafetyRelease, b.release)
}

func (b *terminateWriteBarrier) release() {
	b.releaseOnce.Do(func() { close(b.safetyRelease) })
}

type terminateBarrierConn struct {
	net.Conn
	barrier *terminateWriteBarrier
}

func (c *terminateBarrierConn) Write(payload []byte) (int, error) {
	if !c.barrier.armed.Load() {
		return c.Conn.Write(payload)
	}
	c.barrier.writeOnce.Do(func() { close(c.barrier.writeStarted) })
	select {
	case <-c.barrier.deadlineSet:
		return 0, os.ErrDeadlineExceeded
	case <-c.barrier.safetyRelease:
		return c.Conn.Write(payload)
	}
}

func (c *terminateBarrierConn) SetDeadline(deadline time.Time) error {
	err := c.Conn.SetDeadline(deadline)
	if c.barrier.armed.Load() && !deadline.IsZero() {
		c.barrier.deadlineOnce.Do(func() { close(c.barrier.deadlineSet) })
	}
	return err
}

func TestPoolShutdownBoundsRealPgxpoolDestructors(t *testing.T) {
	t.Run("caller deadline bounds idle destructor", func(t *testing.T) {
		pool, shutdown, barrier := newPoolShutdownFaultPool(t)
		barrier.arm()

		closeCtx, cancelClose := context.WithTimeout(context.Background(), 75*time.Millisecond)
		defer cancelClose()
		startedAt := time.Now()
		err := shutdown.Close(closeCtx, pool)
		if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
			t.Fatalf("PoolShutdown.Close() elapsed = %v, want caller-bounded destructor", elapsed)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("PoolShutdown.Close() error = %v, want context.DeadlineExceeded", err)
		}
		select {
		case <-barrier.writeStarted:
		default:
			t.Fatal("real pgx connection destructor did not reach Terminate write barrier")
		}
	})

	t.Run("shutdown interrupts destructor started before admission", func(t *testing.T) {
		pool, shutdown, barrier := newPoolShutdownFaultPool(t)
		barrier.arm()
		pool.Reset()
		select {
		case <-barrier.writeStarted:
		case <-time.After(time.Second):
			t.Fatal("pool.Reset() destructor did not reach Terminate write barrier")
		}

		closeCtx, cancelClose := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelClose()
		startedAt := time.Now()
		if err := shutdown.Close(closeCtx, pool); err != nil {
			t.Fatalf("PoolShutdown.Close() error = %v", err)
		}
		if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
			t.Fatalf("PoolShutdown.Close() elapsed = %v, want pre-existing destructor interrupted", elapsed)
		}
	})
}

func newPoolShutdownFaultPool(
	t *testing.T,
) (*pgxpool.Pool, *database.PoolShutdown, *terminateWriteBarrier) {
	t.Helper()
	StartPostgres(t)
	shutdown := database.NewPoolShutdown()
	cfg, err := database.PoolConfigForDBIntegration(DSN(t), database.Options{
		MaxConns:     1,
		PoolShutdown: shutdown,
	})
	if err != nil {
		t.Fatalf("PoolConfigForDBIntegration() error = %v", err)
	}
	barrier := newTerminateWriteBarrier()
	dial := cfg.ConnConfig.DialFunc
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, dialErr := dial(ctx, network, address)
		if dialErr != nil {
			return nil, dialErr
		}
		return &terminateBarrierConn{Conn: conn, barrier: barrier}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig() error = %v", err)
	}
	t.Cleanup(func() {
		barrier.release()
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelCleanup()
		_ = shutdown.Close(cleanupCtx, pool)
	})
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatalf("pool.Ping() error = %v", err)
	}
	return pool, shutdown, barrier
}
