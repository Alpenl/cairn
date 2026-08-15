package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type poolCloserFunc func()

func (f poolCloserFunc) Close() {
	f()
}

func TestPoolShutdownUsesCallerDeadlineForConnectionDestructors(t *testing.T) {
	t.Parallel()

	var destructorDeadline time.Time
	shutdown := newPoolShutdown(func(ctx context.Context, _ *pgx.Conn) error {
		var ok bool
		destructorDeadline, ok = ctx.Deadline()
		if !ok {
			t.Fatal("connection destructor context has no deadline")
		}
		<-ctx.Done()
		return ctx.Err()
	}, time.Second)
	pool := poolCloserFunc(func() { shutdown.beforeClose(nil) })

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelClose()
	wantDeadline, ok := closeCtx.Deadline()
	if !ok {
		t.Fatal("close context has no deadline")
	}
	startedAt := time.Now()
	err := shutdown.close(closeCtx, pool)
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("Close() elapsed = %v, want bounded by caller deadline", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want context.DeadlineExceeded", err)
	}
	if !destructorDeadline.Equal(wantDeadline) {
		t.Fatalf("destructor deadline = %v, want caller deadline %v", destructorDeadline, wantDeadline)
	}
}

func TestPoolShutdownAggregatesConnectionCloseErrorsAndReplaysResult(t *testing.T) {
	t.Parallel()

	firstErr := errors.New("first connection close")
	secondErr := errors.New("second connection close")
	closeErrors := []error{firstErr, secondErr}
	closeIndex := 0
	shutdown := newPoolShutdown(func(context.Context, *pgx.Conn) error {
		err := closeErrors[closeIndex]
		closeIndex++
		return err
	}, time.Second)
	pool := poolCloserFunc(func() {
		shutdown.beforeClose(nil)
		shutdown.beforeClose(nil)
	})

	err := shutdown.close(context.Background(), pool)
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("Close() error = %v, want both connection close errors", err)
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	replayedErr := shutdown.close(canceledCtx, pool)
	if !errors.Is(replayedErr, firstErr) || !errors.Is(replayedErr, secondErr) {
		t.Fatalf("repeated Close() error = %v, want stable aggregated result", replayedErr)
	}
	if closeIndex != 2 {
		t.Fatalf("connection close calls = %d, want 2 from first pool Close only", closeIndex)
	}
}

func TestPoolShutdownSuppliesFallbackDeadline(t *testing.T) {
	t.Parallel()

	fallbackTimeout := 200 * time.Millisecond
	startedAt := time.Now()
	var destructorDeadline time.Time
	shutdown := newPoolShutdown(func(ctx context.Context, _ *pgx.Conn) error {
		var ok bool
		destructorDeadline, ok = ctx.Deadline()
		if !ok {
			t.Fatal("connection destructor context has no deadline")
		}
		return nil
	}, fallbackTimeout)
	pool := poolCloserFunc(func() { shutdown.beforeClose(nil) })

	if err := shutdown.close(context.Background(), pool); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if minimum := startedAt.Add(fallbackTimeout / 2); destructorDeadline.Before(minimum) {
		t.Fatalf("destructor deadline = %v, want after %v", destructorDeadline, minimum)
	}
	if maximum := startedAt.Add(2 * fallbackTimeout); destructorDeadline.After(maximum) {
		t.Fatalf("destructor deadline = %v, want before %v", destructorDeadline, maximum)
	}
}

func TestPoolShutdownCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	closeCalls := 0
	shutdown := newPoolShutdown(func(context.Context, *pgx.Conn) error { return nil }, time.Second)
	pool := poolCloserFunc(func() {
		closeCalls++
		shutdown.beforeClose(nil)
	})

	if err := shutdown.close(context.Background(), pool); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := shutdown.close(canceledCtx, pool); err != nil {
		t.Fatalf("second Close() error = %v, want stable first result", err)
	}
	if closeCalls != 1 {
		t.Fatalf("pool Close() calls = %d, want 1", closeCalls)
	}
}

func TestPoolShutdownConcurrentCloseDoesNotDuplicatePoolClose(t *testing.T) {
	t.Parallel()

	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	firstResult := make(chan error, 1)
	closeCalls := 0
	shutdown := newPoolShutdown(func(context.Context, *pgx.Conn) error { return nil }, time.Second)
	pool := poolCloserFunc(func() {
		closeCalls++
		close(closeStarted)
		<-releaseClose
	})

	go func() {
		firstResult <- shutdown.close(context.Background(), pool)
	}()
	select {
	case <-closeStarted:
	case <-time.After(time.Second):
		t.Fatal("first Close() did not reach pool")
	}

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelWait()
	if err := shutdown.close(waitCtx, pool); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent Close() error = %v, want caller deadline", err)
	}
	close(releaseClose)
	if err := <-firstResult; err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if closeCalls != 1 {
		t.Fatalf("pool Close() calls = %d, want 1", closeCalls)
	}
}

func TestPoolShutdownOwnsInitialConstructionUntilShutdown(t *testing.T) {
	t.Parallel()

	shutdown := NewPoolShutdown()
	constructionCtx, err := shutdown.bindInitialConstructionContext(context.Background())
	if err != nil {
		t.Fatalf("bindInitialConstructionContext() error = %v", err)
	}
	select {
	case <-constructionCtx.Done():
		t.Fatal("initial construction canceled before shutdown admission")
	default:
	}

	shutdown.BeginShutdown()
	select {
	case <-constructionCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("initial construction was not canceled at shutdown admission")
	}
	if !errors.Is(constructionCtx.Err(), context.Canceled) {
		t.Fatalf("initial construction error = %v, want context.Canceled", constructionCtx.Err())
	}
}

func TestPoolShutdownRejectsSecondInitialConstructionOwner(t *testing.T) {
	t.Parallel()

	shutdown := NewPoolShutdown()
	if _, err := shutdown.bindInitialConstructionContext(context.Background()); err != nil {
		t.Fatalf("first bindInitialConstructionContext() error = %v", err)
	}
	if _, err := shutdown.bindInitialConstructionContext(context.Background()); err == nil {
		t.Fatal("second bindInitialConstructionContext() error = nil, want single-pool ownership error")
	}
}

func TestPoolShutdownInterruptsDestructorStartedBeforeShutdown(t *testing.T) {
	t.Parallel()

	destructorStarted := make(chan struct{})
	destructorDone := make(chan struct{})
	var destructorErr error
	shutdown := newPoolShutdown(func(ctx context.Context, _ *pgx.Conn) error {
		close(destructorStarted)
		<-ctx.Done()
		destructorErr = ctx.Err()
		close(destructorDone)
		return destructorErr
	}, 2*time.Second)
	go shutdown.beforeClose(nil)
	select {
	case <-destructorStarted:
	case <-time.After(time.Second):
		t.Fatal("pre-shutdown destructor did not start")
	}

	pool := poolCloserFunc(func() { <-destructorDone })
	closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClose()
	startedAt := time.Now()
	if err := shutdown.close(closeCtx, pool); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("Close() elapsed = %v, want in-flight destructor interrupted", elapsed)
	}
	if !errors.Is(destructorErr, context.Canceled) {
		t.Fatalf("pre-shutdown destructor error = %v, want context.Canceled", destructorErr)
	}
}

func TestPoolShutdownInstallsIntoPgxpoolConfig(t *testing.T) {
	t.Parallel()

	closeCalls := 0
	shutdown := newPoolShutdown(func(context.Context, *pgx.Conn) error {
		closeCalls++
		return nil
	}, time.Second)
	cfg, err := pgxpool.ParseConfig("postgres://test:test@127.0.0.1:1/test")
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if err := installPoolShutdown(cfg, shutdown); err != nil {
		t.Fatalf("installPoolShutdown() error = %v", err)
	}
	if cfg.BeforeClose == nil {
		t.Fatal("pgxpool BeforeClose hook is nil")
	}
	cfg.BeforeClose(nil)
	if closeCalls != 1 {
		t.Fatalf("connection close calls = %d, want 1", closeCalls)
	}
}

func TestPoolShutdownRejectsExistingBeforeCloseHook(t *testing.T) {
	t.Parallel()

	cfg, err := pgxpool.ParseConfig("postgres://test:test@127.0.0.1:1/test")
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	cfg.BeforeClose = func(*pgx.Conn) {}

	err = installPoolShutdown(cfg, NewPoolShutdown())
	if err == nil || !strings.Contains(err.Error(), "BeforeClose") {
		t.Fatalf("installPoolShutdown() error = %v, want existing BeforeClose rejection", err)
	}
}

func TestPoolShutdownRejectsZeroValueCapability(t *testing.T) {
	t.Parallel()

	cfg, err := pgxpool.ParseConfig("postgres://test:test@127.0.0.1:1/test")
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}

	err = installPoolShutdown(cfg, &PoolShutdown{})
	if err == nil || !strings.Contains(err.Error(), "NewPoolShutdown") {
		t.Fatalf("installPoolShutdown() error = %v, want constructor invariant", err)
	}
	if cfg.BeforeClose != nil {
		t.Fatal("installPoolShutdown() mutated config after rejecting zero-value capability")
	}
}
