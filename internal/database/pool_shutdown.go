package database

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultPoolConnectionCloseTimeout = 15 * time.Second

var errPoolShutdownNotInitialized = errors.New("database: PoolShutdown must be constructed with NewPoolShutdown")

type synchronousPoolCloser interface {
	Close()
}

// PoolShutdown makes pgxpool's synchronous Close consume the Runtime cleanup
// deadline. Its BeforeClose hook closes each connection first, so pgxpool's
// detached 15-second destructor becomes an immediate idempotent close.
type PoolShutdown struct {
	mu                sync.Mutex
	closeDone         chan struct{}
	closeErr          error
	shutdownCtx       context.Context
	shutdownCancel    context.CancelFunc
	initialCancel     context.CancelFunc
	fallbackCtx       context.Context
	cancelFallback    context.CancelFunc
	fallbackTimeout   time.Duration
	closeConnection   func(context.Context, *pgx.Conn) error
	shutdownCloseErrs error
}

// NewPoolShutdown constructs the shutdown capability that must be installed
// into database.Options before opening an application Runtime pool.
func NewPoolShutdown() *PoolShutdown {
	return newPoolShutdown(func(ctx context.Context, conn *pgx.Conn) error {
		if conn == nil {
			return nil
		}
		return conn.Close(ctx)
	}, defaultPoolConnectionCloseTimeout)
}

func newPoolShutdown(
	closeConnection func(context.Context, *pgx.Conn) error,
	fallbackTimeout time.Duration,
) *PoolShutdown {
	if fallbackTimeout <= 0 {
		fallbackTimeout = defaultPoolConnectionCloseTimeout
	}
	if closeConnection == nil {
		closeConnection = func(context.Context, *pgx.Conn) error { return nil }
	}
	fallbackCtx, cancelFallback := context.WithCancel(context.Background())
	return &PoolShutdown{
		fallbackCtx:     fallbackCtx,
		cancelFallback:  cancelFallback,
		fallbackTimeout: fallbackTimeout,
		closeConnection: closeConnection,
	}
}

// Close synchronously destroys pool resources under ctx. No cleanup goroutine
// outlives this call.
func (s *PoolShutdown) Close(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return nil
	}
	return s.close(ctx, pool)
}

func (s *PoolShutdown) close(ctx context.Context, pool synchronousPoolCloser) error {
	if err := s.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.closeDone != nil {
		done := s.closeDone
		s.mu.Unlock()
		select {
		case <-done:
			return s.result()
		default:
		}
		select {
		case <-done:
			return s.result()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.closeDone = make(chan struct{})
	done := s.closeDone
	s.mu.Unlock()

	shutdownCtx := s.begin(ctx)
	pool.Close()

	s.mu.Lock()
	closeErr := s.shutdownCloseErrs
	shutdownCancel := s.shutdownCancel
	s.mu.Unlock()
	ctxErr := shutdownCtx.Err()
	if shutdownCancel != nil {
		shutdownCancel()
	}
	result := errors.Join(closeErr, ctxErr)

	s.mu.Lock()
	s.closeErr = result
	close(done)
	s.mu.Unlock()
	return result
}

func (s *PoolShutdown) result() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeErr
}

func (s *PoolShutdown) bindInitialConstructionContext(parent context.Context) (context.Context, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initialCancel != nil {
		return nil, errors.New("database: PoolShutdown already owns an initial pool construction")
	}
	if s.closeDone != nil || s.shutdownCtx != nil {
		return nil, errors.New("database: cannot bind initial pool construction after shutdown")
	}
	ctx, cancel := context.WithCancel(parent)
	s.initialCancel = cancel
	return ctx, nil
}

// BeginShutdown cancels pgxpool's asynchronous initial MinConns construction.
// Runtime calls it when persistence admission closes so Drain cannot wait on a
// constructor that still carries the long-lived Build context.
func (s *PoolShutdown) BeginShutdown() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.initialCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func installPoolShutdown(cfg *pgxpool.Config, shutdown *PoolShutdown) error {
	if shutdown == nil {
		return nil
	}
	if err := shutdown.validate(); err != nil {
		return err
	}
	if cfg == nil {
		return errors.New("database: nil pgxpool config for PoolShutdown")
	}
	if cfg.BeforeClose != nil {
		return errors.New("database: PoolShutdown must be the sole pgxpool BeforeClose owner")
	}
	cfg.BeforeClose = func(conn *pgx.Conn) {
		shutdown.beforeClose(conn)
	}
	return nil
}

func (s *PoolShutdown) validate() error {
	if s == nil {
		return errPoolShutdownNotInitialized
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fallbackCtx == nil || s.cancelFallback == nil || s.fallbackTimeout <= 0 || s.closeConnection == nil {
		return errPoolShutdownNotInitialized
	}
	return nil
}

func (s *PoolShutdown) begin(ctx context.Context) context.Context {
	s.mu.Lock()
	if s.shutdownCtx != nil {
		shutdownCtx := s.shutdownCtx
		s.mu.Unlock()
		return shutdownCtx
	}
	shutdownCtx := ctx
	shutdownCancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		shutdownCtx, shutdownCancel = context.WithTimeout(ctx, s.fallbackTimeout)
	}
	s.shutdownCtx = shutdownCtx
	s.shutdownCancel = shutdownCancel
	cancelInitial := s.initialCancel
	cancelFallback := s.cancelFallback
	s.mu.Unlock()

	// Destructors that started as ordinary lifetime/idle eviction must not keep
	// pgxpool's destructor wait group alive past Runtime shutdown admission.
	if cancelInitial != nil {
		cancelInitial()
	}
	cancelFallback()
	return shutdownCtx
}

func (s *PoolShutdown) beforeClose(conn *pgx.Conn) {
	ctx, cancel, shuttingDown := s.connectionCloseContext()
	err := s.closeConnection(ctx, conn)
	cancel()
	if !shuttingDown || err == nil {
		return
	}
	s.mu.Lock()
	s.shutdownCloseErrs = errors.Join(s.shutdownCloseErrs, err)
	s.mu.Unlock()
}

func (s *PoolShutdown) connectionCloseContext() (context.Context, context.CancelFunc, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shutdownCtx != nil {
		return s.shutdownCtx, func() {}, true
	}
	ctx, cancel := context.WithTimeout(s.fallbackCtx, s.fallbackTimeout)
	return ctx, cancel, false
}
