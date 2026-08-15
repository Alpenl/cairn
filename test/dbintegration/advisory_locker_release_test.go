package dbintegration

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/service/urllock"
)

func TestAdvisoryURLLockerNormalUnlockUsesBoundedDetachedContext(t *testing.T) {
	adminPool := StartPostgres(t)
	probe := newUnlockContextProbe()
	pool := openAdvisoryReleasePool(t, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = probe
	})
	locker := urllock.NewAdvisoryURLLocker(pool, 712115)

	requestCtx, cancelRequest := context.WithCancel(t.Context())
	defer cancelRequest()
	err := locker.WithURL(requestCtx, "https://example.com/detached-unlock", func(callbackCtx context.Context) error {
		cancelRequest()
		return callbackCtx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithURL() error = %v, want context.Canceled", err)
	}

	observation := probe.wait(t)
	if observation.err != nil {
		t.Fatalf("unlock context error = %v, want nil", observation.err)
	}
	if !observation.hasDeadline {
		t.Fatal("unlock context has no deadline")
	}
	remaining := time.Until(observation.deadline)
	if remaining <= 0 || remaining > 6*time.Second {
		t.Fatalf("unlock context remaining deadline = %v, want within (0, 6s]", remaining)
	}
	if acquired := pool.Stat().AcquiredConns(); acquired != 0 {
		t.Fatalf("pool acquired connections after normal unlock = %d, want 0", acquired)
	}
	assertNoAdvisoryLocks(t, adminPool, 712115)
}

func TestAdvisoryURLLockerFailedUnlockRetainsPoolOwnershipUntilSessionCloses(t *testing.T) {
	adminPool := StartPostgres(t)
	const (
		lockClass     int32 = 712116
		failureSchema       = "rf7_unlock_failure"
	)
	if _, err := adminPool.Exec(t.Context(), "CREATE SCHEMA "+failureSchema); err != nil {
		t.Fatalf("create unlock failure schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+failureSchema+" CASCADE"); err != nil {
			t.Errorf("drop unlock failure schema: %v", err)
		}
	})
	if _, err := adminPool.Exec(t.Context(), `CREATE FUNCTION `+failureSchema+`.pg_advisory_unlock_all()
		RETURNS void
		LANGUAGE plpgsql
		AS $$ BEGIN RAISE EXCEPTION 'forced advisory unlock failure'; END $$`); err != nil {
		t.Fatalf("create failing advisory unlock function: %v", err)
	}

	closeBarrier := newConnectionCloseBarrier()
	pool := openAdvisoryReleasePool(t, func(config *pgxpool.Config) {
		config.ConnConfig.RuntimeParams["search_path"] = failureSchema + ",pg_catalog"
		config.ConnConfig.DialFunc = closeBarrier.dialContext
	})
	locker := urllock.NewAdvisoryURLLocker(pool, lockClass)

	done := make(chan error, 1)
	go func() {
		done <- locker.WithURL(t.Context(), "https://example.com/failed-unlock", func(context.Context) error {
			return nil
		})
	}()

	closeBarrier.waitUntilCloseStarts(t)
	if acquired := pool.Stat().AcquiredConns(); acquired != 1 {
		closeBarrier.allowClose()
		t.Fatalf("pool acquired connections while session close is blocked = %d, want 1", acquired)
	}
	closeBarrier.allowClose()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "release advisory lock set") || !strings.Contains(err.Error(), "forced advisory unlock failure") {
			t.Fatalf("WithURL() error = %v, want wrapped advisory unlock failure", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WithURL() did not return after the connection close was released")
	}

	waitForAcquiredConnections(t, pool, 0)
	assertNoAdvisoryLocks(t, adminPool, lockClass)
	var one int
	if err := pool.QueryRow(t.Context(), "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query after failed unlock connection was discarded: %v", err)
	}
	if one != 1 {
		t.Fatalf("query after failed unlock = %d, want 1", one)
	}
}

type unlockContextObservation struct {
	err         error
	deadline    time.Time
	hasDeadline bool
}

type unlockContextProbe struct {
	once        sync.Once
	observation chan unlockContextObservation
}

func newUnlockContextProbe() *unlockContextProbe {
	return &unlockContextProbe{observation: make(chan unlockContextObservation, 1)}
}

func (p *unlockContextProbe) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "pg_advisory_unlock_all") {
		p.once.Do(func() {
			deadline, ok := ctx.Deadline()
			p.observation <- unlockContextObservation{
				err:         ctx.Err(),
				deadline:    deadline,
				hasDeadline: ok,
			}
		})
	}
	return ctx
}

func (*unlockContextProbe) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (p *unlockContextProbe) wait(t *testing.T) unlockContextObservation {
	t.Helper()
	select {
	case observation := <-p.observation:
		return observation
	case <-time.After(time.Second):
		t.Fatal("unlock query was not observed")
		return unlockContextObservation{}
	}
}

type connectionCloseBarrier struct {
	started   chan struct{}
	allowed   chan struct{}
	startOnce sync.Once
	allowOnce sync.Once
}

func newConnectionCloseBarrier() *connectionCloseBarrier {
	return &connectionCloseBarrier{
		started: make(chan struct{}),
		allowed: make(chan struct{}),
	}
}

func (b *connectionCloseBarrier) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &barrierConn{Conn: conn, barrier: b}, nil
}

func (b *connectionCloseBarrier) waitUntilCloseStarts(t *testing.T) {
	t.Helper()
	select {
	case <-b.started:
	case <-time.After(3 * time.Second):
		b.allowClose()
		t.Fatal("advisory session connection close did not start")
	}
}

func (b *connectionCloseBarrier) allowClose() {
	b.allowOnce.Do(func() { close(b.allowed) })
}

type barrierConn struct {
	net.Conn
	barrier *connectionCloseBarrier
}

func (c *barrierConn) Close() error {
	c.barrier.startOnce.Do(func() { close(c.barrier.started) })
	<-c.barrier.allowed
	return c.Conn.Close()
}

func openAdvisoryReleasePool(t *testing.T, configure func(*pgxpool.Config)) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(DSN(t))
	if err != nil {
		t.Fatalf("parse advisory release pool config: %v", err)
	}
	config.MaxConns = 1
	configure(config)
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("open advisory release pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func waitForAcquiredConnections(t *testing.T, pool *pgxpool.Pool, want int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pool.Stat().AcquiredConns() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pool acquired connections = %d, want %d", pool.Stat().AcquiredConns(), want)
}

func assertNoAdvisoryLocks(t *testing.T, pool *pgxpool.Pool, lockClass int32) {
	t.Helper()
	var remaining int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND classid::bigint = $1 AND granted`, lockClass).Scan(&remaining); err != nil {
		t.Fatalf("count remaining advisory locks: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("remaining advisory locks = %d, want 0", remaining)
	}
}
