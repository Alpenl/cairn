package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"webtag/internal/observability"
)

var (
	// ErrBackgroundAlreadyStarted reports a repeated background Start call.
	ErrBackgroundAlreadyStarted = errors.New("background already started")
	// ErrBackgroundStopped reports Start after a terminal Stop.
	ErrBackgroundStopped = errors.New("background stopped")
)

type backgroundState uint8

const (
	backgroundConstructed backgroundState = iota
	backgroundStarted
	backgroundStopping
	backgroundStopped
)

type backgroundLoop struct {
	mu        sync.Mutex
	state     backgroundState
	cancel    context.CancelFunc
	done      chan struct{}
	component string
	logger    *slog.Logger
}

func newBackgroundLoop(component string, logger *slog.Logger) backgroundLoop {
	return backgroundLoop{done: make(chan struct{}), component: component, logger: logger}
}

func (l *backgroundLoop) Start(ctx context.Context, run func(context.Context)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.state {
	case backgroundStarted:
		return ErrBackgroundAlreadyStarted
	case backgroundStopping, backgroundStopped:
		return ErrBackgroundStopped
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	l.state = backgroundStarted
	go func() {
		defer func() {
			if l.logger != nil {
				l.logger.Info("background worker exited",
					"component", l.component,
					"reason", observability.SafeError(context.Cause(runCtx)))
			}
			l.finish()
		}()
		run(runCtx)
	}()
	return nil
}

func (l *backgroundLoop) Stop(ctx context.Context) error {
	l.mu.Lock()
	switch l.state {
	case backgroundConstructed:
		l.state = backgroundStopped
		close(l.done)
		l.mu.Unlock()
		return nil
	case backgroundStarted:
		l.state = backgroundStopping
		l.cancel()
	case backgroundStopping:
	case backgroundStopped:
		l.mu.Unlock()
		return nil
	}
	done := l.done
	l.mu.Unlock()

	select {
	case <-done:
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *backgroundLoop) finish() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == backgroundStopped {
		return
	}
	l.state = backgroundStopped
	l.cancel = nil
	close(l.done)
}
