package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"webtag/internal/database"
	"webtag/internal/lifecycle"
)

type runtimeManagedBackground interface {
	Start(context.Context) error
	Stop(context.Context) error
}

type namedRuntimeBackground struct {
	name       string
	background runtimeManagedBackground
}

type runtimePersistence interface {
	AdmitOwner(context.Context) (context.Context, func(), error)
	CloseAdmission()
	Drain(context.Context) error
	Close(context.Context) error
}

type runtimeLifecycleOptions struct {
	backgrounds    []namedRuntimeBackground
	cleanupTimeout time.Duration
	persistence    runtimePersistence
}

type runtimeLifecycle struct {
	backgrounds    []namedRuntimeBackground
	cleanupTimeout time.Duration
	persistence    runtimePersistence
	started        cleanupStack
	constructed    cleanupStack
	ownerRevoke    func()
	stopIncomplete bool
	startSucceeded bool
}

func newRuntimeLifecycle(options runtimeLifecycleOptions) *runtimeLifecycle {
	if options.cleanupTimeout <= 0 {
		options.cleanupTimeout = defaultRuntimeCleanupTimeout
	}
	lifecycle := &runtimeLifecycle{
		backgrounds:    options.backgrounds,
		cleanupTimeout: options.cleanupTimeout,
		persistence:    options.persistence,
	}
	for _, entry := range options.backgrounds {
		if entry.background != nil {
			lifecycle.constructed.Add(entry.name, entry.background.Stop)
		}
	}
	return lifecycle
}

func (l *runtimeLifecycle) Start(ctx context.Context) error {
	if l == nil {
		return nil
	}
	ownerCtx := ctx
	var ownerRevoke func()
	if l.persistence != nil {
		var err error
		ownerCtx, ownerRevoke, err = l.persistence.AdmitOwner(ctx)
		if err != nil {
			return fmt.Errorf("admit runtime persistence owner: %w", err)
		}
	}
	var rollback cleanupStack
	startedCount := 0
	for _, entry := range l.backgrounds {
		if entry.background == nil {
			continue
		}
		if err := entry.background.Start(ownerCtx); err != nil {
			startErr := fmt.Errorf("start %s: %w", entry.name, err)
			// The successful prefix belongs to rollback. Runtime.Close owns only
			// the failed component and the not-yet-started suffix, so every
			// component receives exactly one terminal Stop callback.
			l.constructed.actions = l.constructed.actions[startedCount:]
			l.ownerRevoke = ownerRevoke
			cleanupCtx, cancelCleanup := lifecycle.CleanupContext(ownerCtx, l.cleanupTimeout)
			rollbackErr := rollback.Run(cleanupCtx)
			cancelCleanup()
			if rollbackErr != nil {
				// A failed Stop means a runtime-owned database user may still be
				// alive. Keep its owner lease so Close cannot mistake pool stats
				// for a complete drain (River also owns a hijacked listener).
				l.stopIncomplete = true
				return errors.Join(startErr, fmt.Errorf("runtime start rollback: %w", rollbackErr))
			}
			return startErr
		}
		background := entry.background
		rollback.Add(entry.name, background.Stop)
		startedCount++
	}
	l.started = rollback
	l.constructed.actions = nil
	l.ownerRevoke = ownerRevoke
	l.startSucceeded = true
	return nil
}

func (l *runtimeLifecycle) Close(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if l.persistence != nil {
		l.persistence.CloseAdmission()
	}
	var stopErr error
	if l.startSucceeded {
		stopErr = l.started.Run(ctx)
		l.constructed.actions = nil
	} else {
		// Close-before-Start and a successfully rolled-back Start failure still
		// terminally close every constructed controller. Their Stop-before-Start
		// contract is idempotent and must not create background work.
		stopErr = l.constructed.Run(ctx)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		stopErr = errors.Join(stopErr, ctxErr)
	}
	if stopErr != nil || l.stopIncomplete {
		// Do not revoke the owner after an incomplete Stop. Retaining it is
		// intentional: a later Drain must not report zero while a producer,
		// River listener, or recorder may still touch persistence. The process
		// supervisor owns the hard-termination policy after this sentinel.
		return errors.Join(stopErr, database.ErrShutdownDeadline)
	}
	if l.ownerRevoke != nil {
		l.ownerRevoke()
		l.ownerRevoke = nil
	}

	if l.persistence != nil {
		if err := l.persistence.Drain(ctx); err != nil {
			return err
		}
		// Drain proves there are no owners, acquired connections, or
		// connections still being constructed. Pool destruction remains
		// synchronous and must consume the same shutdown context.
		if err := l.persistence.Close(ctx); err != nil {
			return errors.Join(err, database.ErrShutdownDeadline)
		}
	}

	return nil
}

func durationMS(ms int) time.Duration {
	return time.Duration(ms) * time.Millisecond
}
