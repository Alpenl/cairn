package database

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrPersistenceAdmissionClosed is the sentinel returned when a caller is
	// not permitted to acquire persistence resources after admission closes.
	// Callers should match it with errors.Is.
	ErrPersistenceAdmissionClosed = errors.New("persistence acquisition admission closed")
	// ErrShutdownDeadline is the sentinel joined into a Drain error when its
	// context ends before every owner and connection acquisition has drained.
	// Callers should match it with errors.Is.
	ErrShutdownDeadline = errors.New("persistence shutdown deadline")
)

type acquisitionOwnerContextKey struct{}

// AcquisitionGate coordinates persistence acquisition admission during
// shutdown. It is safe for concurrent use. Before admission closes, Check
// permits every caller. After admission closes, Check permits only contexts
// carrying an active owner admitted by this gate. Admission cannot be reopened.
type AcquisitionGate struct {
	mu        sync.RWMutex
	accepting bool
	owners    int
}

// AcquisitionOwner represents one caller admitted before its gate closed. The
// derived context returned with the owner retains acquisition rights until
// Revoke is called. An owner is tied to one gate, is safe for concurrent use,
// and must not be copied after first use.
type AcquisitionOwner struct {
	gate   *AcquisitionGate
	active atomic.Bool
}

// NewAcquisitionGate returns a gate with persistence admission open.
func NewAcquisitionGate() *AcquisitionGate {
	return &AcquisitionGate{accepting: true}
}

// AdmitOwner atomically admits a new owner while the gate is open. It returns
// a derived context carrying that owner's acquisition rights; the derived
// context preserves the parent context's cancellation, deadline, and values.
// The caller must eventually call Revoke, which is safe to defer and call more
// than once. A closed or nil gate returns ErrPersistenceAdmissionClosed and
// leaves ctx unchanged.
func (g *AcquisitionGate) AdmitOwner(ctx context.Context) (context.Context, *AcquisitionOwner, error) {
	if g == nil {
		return ctx, nil, ErrPersistenceAdmissionClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.accepting {
		return ctx, nil, ErrPersistenceAdmissionClosed
	}
	owner := &AcquisitionOwner{gate: g}
	owner.active.Store(true)
	g.owners++
	return context.WithValue(ctx, acquisitionOwnerContextKey{}, owner), owner, nil
}

// CloseAdmission permanently prevents new owners and unowned contexts from
// acquiring persistence resources. Owners already admitted by this gate remain
// valid until revoked. CloseAdmission is idempotent and safe for concurrent use;
// calling it on a nil gate is a no-op.
func (g *AcquisitionGate) CloseAdmission() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.accepting = false
	g.mu.Unlock()
}

// Check reports whether ctx may acquire a persistence resource. It permits all
// callers while admission is open. Once admission is closed, it permits only a
// context carrying an active owner from this gate; revoked and foreign owners
// are rejected. Check is safe for concurrent use and returns
// ErrPersistenceAdmissionClosed for a rejected caller or a nil gate.
func (g *AcquisitionGate) Check(ctx context.Context) error {
	if g == nil {
		return ErrPersistenceAdmissionClosed
	}
	g.mu.RLock()
	accepting := g.accepting
	g.mu.RUnlock()
	if accepting {
		return nil
	}
	owner, _ := ctx.Value(acquisitionOwnerContextKey{}).(*AcquisitionOwner)
	if owner != nil && owner.gate == g && owner.active.Load() {
		return nil
	}
	return ErrPersistenceAdmissionClosed
}

// Revoke idempotently removes the owner's acquisition rights and releases its
// gate's drain hold. Once admission is closed, every context carrying the
// revoked owner fails Check. Revoke is safe for concurrent use and is a no-op
// on a nil owner.
func (o *AcquisitionOwner) Revoke() {
	if o == nil || !o.active.CompareAndSwap(true, false) {
		return
	}
	if o.gate != nil {
		o.gate.mu.Lock()
		o.gate.owners--
		o.gate.mu.Unlock()
	}
}

type acquisitionCounts struct {
	acquired     int32
	constructing int32
}

// Drain closes admission, then waits until all admitted owners are revoked and
// pool reports no acquired or constructing connections. It does not close the
// pool. If ctx ends first, Drain returns an error matching both
// ErrShutdownDeadline and ctx.Err. A nil pool contributes no connection counts;
// a nil gate returns nil. Drain is safe to call concurrently.
func (g *AcquisitionGate) Drain(ctx context.Context, pool *pgxpool.Pool) error {
	return g.drain(ctx, func() acquisitionCounts {
		if pool == nil {
			return acquisitionCounts{}
		}
		stats := pool.Stat()
		return acquisitionCounts{
			acquired:     stats.AcquiredConns(),
			constructing: stats.ConstructingConns(),
		}
	})
}

func (g *AcquisitionGate) drain(ctx context.Context, counts func() acquisitionCounts) error {
	if g == nil {
		return nil
	}
	g.CloseAdmission()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		g.mu.RLock()
		owners := g.owners
		g.mu.RUnlock()
		current := counts()
		if owners == 0 && current.acquired == 0 && current.constructing == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(ErrShutdownDeadline, ctx.Err())
		case <-ticker.C:
		}
	}
}

func installAcquisitionGate(cfg *pgxpool.Config, gate *AcquisitionGate) {
	if cfg == nil || gate == nil {
		return
	}

	priorBeforeConnect := cfg.BeforeConnect
	cfg.BeforeConnect = func(ctx context.Context, connConfig *pgx.ConnConfig) error {
		if err := gate.Check(ctx); err != nil {
			return err
		}
		if priorBeforeConnect != nil {
			return priorBeforeConnect(ctx, connConfig)
		}
		return nil
	}

	priorPrepareConn := cfg.PrepareConn
	// pgx still exposes BeforeAcquire for compatibility. Preserve a caller that
	// configured it, but move its behavior into PrepareConn so the gate remains
	// the single admission decision and pgx precedence is unambiguous.
	priorBeforeAcquire := cfg.BeforeAcquire //nolint:staticcheck // reason: compatibility bridge for pgx's deprecated hook
	cfg.BeforeAcquire = nil                 //nolint:staticcheck // reason: PrepareConn takes precedence and now composes the legacy hook
	cfg.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		if err := gate.Check(ctx); err != nil {
			return true, err
		}
		if priorPrepareConn != nil {
			return priorPrepareConn(ctx, conn)
		}
		if priorBeforeAcquire != nil {
			return priorBeforeAcquire(ctx, conn), nil
		}
		return true, nil
	}

	priorShouldPing := cfg.ShouldPing
	cfg.ShouldPing = func(ctx context.Context, params pgxpool.ShouldPingParams) bool {
		if err := gate.Check(ctx); err != nil {
			return false
		}
		if priorShouldPing != nil {
			return priorShouldPing(ctx, params)
		}
		return params.IdleDuration > time.Second
	}
}
