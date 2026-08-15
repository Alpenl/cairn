package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/puddle/v2"

	"webtag/internal/config"
	"webtag/internal/database"
)

func TestPersistenceLayerRuntimeOwnerSurvivesAdmissionCloseUntilRevoked(t *testing.T) {
	t.Parallel()

	gate := database.NewAcquisitionGate()
	layer := &persistenceLayer{acquisitionGate: gate}
	var persistence runtimePersistence = layer

	type contextKey struct{}
	ownerCtx, revoke, err := persistence.AdmitOwner(context.WithValue(context.Background(), contextKey{}, "owner-value"))
	if err != nil {
		t.Fatalf("AdmitOwner() error = %v", err)
	}
	if got := ownerCtx.Value(contextKey{}); got != "owner-value" {
		t.Fatalf("owner context value = %v, want owner-value", got)
	}

	persistence.CloseAdmission()
	if err := gate.Check(ownerCtx); err != nil {
		t.Fatalf("owner Check() after CloseAdmission() error = %v", err)
	}
	if _, _, err := persistence.AdmitOwner(context.Background()); !errors.Is(err, database.ErrPersistenceAdmissionClosed) {
		t.Fatalf("AdmitOwner() after CloseAdmission() error = %v, want ErrPersistenceAdmissionClosed", err)
	}

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err = persistence.Drain(drainCtx)
	cancelDrain()
	if !errors.Is(err, database.ErrShutdownDeadline) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain() with active owner error = %v, want ErrShutdownDeadline and DeadlineExceeded", err)
	}

	revoke()
	revoke()
	if err := gate.Check(ownerCtx); !errors.Is(err, database.ErrPersistenceAdmissionClosed) {
		t.Fatalf("revoked owner Check() error = %v, want ErrPersistenceAdmissionClosed", err)
	}
	if err := persistence.Drain(context.Background()); err != nil {
		t.Fatalf("Drain() after revoke error = %v", err)
	}
}

func TestPersistenceLayerRuntimeCloseSynchronouslyClosesPool(t *testing.T) {
	t.Parallel()

	cfg, err := pgxpool.ParseConfig("postgres://test:test@127.0.0.1:1/test?connect_timeout=1")
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig() error = %v", err)
	}

	layer := &persistenceLayer{
		pool:            pool,
		acquisitionGate: database.NewAcquisitionGate(),
		poolShutdown:    database.NewPoolShutdown(),
	}
	var persistence runtimePersistence = layer
	if err := persistence.Close(t.Context()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	acquireCtx, cancelAcquire := context.WithTimeout(context.Background(), time.Second)
	defer cancelAcquire()
	conn, err := pool.Acquire(acquireCtx)
	if conn != nil {
		conn.Release()
	}
	if !errors.Is(err, puddle.ErrClosedPool) {
		t.Fatalf("Acquire() after Close() error = %v, want ErrClosedPool", err)
	}
}

type persistenceAcquisitionGateProbe struct {
	closed                 int
	drainCtx               context.Context
	drainPool              *pgxpool.Pool
	drainErrAtCall         error
	drainHadDeadlineAtCall bool
}

func (p *persistenceAcquisitionGateProbe) AdmitOwner(ctx context.Context) (context.Context, *database.AcquisitionOwner, error) {
	return ctx, nil, nil
}

func (p *persistenceAcquisitionGateProbe) CloseAdmission() {
	p.closed++
}

func (p *persistenceAcquisitionGateProbe) Drain(ctx context.Context, pool *pgxpool.Pool) error {
	p.drainCtx = ctx
	p.drainPool = pool
	p.drainErrAtCall = ctx.Err()
	_, p.drainHadDeadlineAtCall = ctx.Deadline()
	return nil
}

type persistencePoolShutdownProbe struct {
	begun     int
	closeCtx  context.Context
	closePool *pgxpool.Pool
	closeErr  error
}

func (p *persistencePoolShutdownProbe) BeginShutdown() {
	p.begun++
}

func (p *persistencePoolShutdownProbe) Close(ctx context.Context, pool *pgxpool.Pool) error {
	p.closeCtx = ctx
	p.closePool = pool
	return p.closeErr
}

type persistenceCallerContext struct {
	context.Context
}

func TestPersistenceLayerRuntimeDelegatesShutdownToProductionCapabilities(t *testing.T) {
	t.Parallel()

	pool := new(pgxpool.Pool)
	gate := &persistenceAcquisitionGateProbe{}
	closeErr := errors.New("close probe")
	shutdown := &persistencePoolShutdownProbe{closeErr: closeErr}
	layer := &persistenceLayer{
		pool:            pool,
		acquisitionGate: gate,
		poolShutdown:    shutdown,
	}
	callerCtx := &persistenceCallerContext{Context: t.Context()}

	layer.CloseAdmission()
	if gate.closed != 1 {
		t.Fatalf("CloseAdmission() gate calls = %d, want 1", gate.closed)
	}
	if shutdown.begun != 1 {
		t.Fatalf("CloseAdmission() BeginShutdown calls = %d, want 1", shutdown.begun)
	}

	if err := layer.Drain(callerCtx); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	if gate.drainCtx != callerCtx {
		t.Fatal("Drain() did not receive the caller context")
	}
	if gate.drainPool != pool {
		t.Fatal("Drain() did not receive the persistence layer pool")
	}

	if err := layer.Close(callerCtx); !errors.Is(err, closeErr) {
		t.Fatalf("Close() error = %v, want %v", err, closeErr)
	}
	if shutdown.closeCtx != callerCtx {
		t.Fatal("PoolShutdown.Close() did not receive the caller context")
	}
	if shutdown.closePool != pool {
		t.Fatal("PoolShutdown.Close() did not receive the persistence layer pool")
	}
}

func TestOpenPersistenceLayerWiresRuntimePoolLifecycleOptions(t *testing.T) {
	t.Parallel()

	poolConfig, err := pgxpool.ParseConfig("postgres://test:test@127.0.0.1:1/test")
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatalf("NewWithConfig() error = %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			pool.Close()
		}
	})
	var captured database.Options
	layer, err := openPersistenceLayerWithDatabase(
		t.Context(),
		config.Config{TranslationSourceRollout: config.TranslationSourceRolloutStrict},
		func(_ context.Context, _ string, opts database.Options) (*pgxpool.Pool, error) {
			captured = opts
			return pool, nil
		},
	)
	if err != nil {
		t.Fatalf("openPersistenceLayerWithDatabase() error = %v", err)
	}
	if captured.AcquisitionGate == nil {
		t.Fatal("runtime database options omitted AcquisitionGate")
	}
	if captured.PoolShutdown == nil {
		t.Fatal("runtime database options omitted PoolShutdown")
	}
	if layer.acquisitionGate != captured.AcquisitionGate {
		t.Fatal("persistence layer did not retain the AcquisitionGate passed to database.Open")
	}
	if layer.poolShutdown != captured.PoolShutdown {
		t.Fatal("persistence layer did not retain the PoolShutdown passed to database.Open")
	}
	layer.CloseAdmission()
	if err := layer.Drain(t.Context()); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	if err := layer.Close(t.Context()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	closed = true
}

func TestPersistenceLayerMissingPoolShutdownBlocksTracer(t *testing.T) {
	t.Parallel()

	poolConfig, err := pgxpool.ParseConfig("postgres://test:test@127.0.0.1:1/test")
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatalf("NewWithConfig() error = %v", err)
	}
	t.Cleanup(pool.Close)
	tracerCalled := false
	lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
		persistence: &persistenceLayer{
			pool:            pool,
			acquisitionGate: database.NewAcquisitionGate(),
		},
		tracerShutdown: func(context.Context) error {
			tracerCalled = true
			return nil
		},
	})
	if err := lifecycle.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	err = lifecycle.Close(t.Context())
	if !errors.Is(err, database.ErrShutdownDeadline) || !strings.Contains(err.Error(), "NewPoolShutdown") {
		t.Fatalf("Close() error = %v, want PoolShutdown invariant and ErrShutdownDeadline", err)
	}
	if tracerCalled {
		t.Fatal("tracer shutdown ran after persistence layer lost PoolShutdown capability")
	}
}
