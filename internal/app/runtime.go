package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"webtag/internal/lifecycle"
)

const defaultRuntimeCleanupTimeout = 5 * time.Second

var (
	// ErrRuntimeAlreadyStarted reports a repeated Runtime.Start call.
	ErrRuntimeAlreadyStarted = errors.New("runtime already started")
	// ErrRuntimeClosed reports Start after Close or after a failed Start.
	ErrRuntimeClosed = errors.New("runtime closed")
)

type runtimeState uint8

const (
	runtimeConstructed runtimeState = iota
	runtimeStarting
	runtimeStarted
	runtimeStopping
	runtimeStopped
)

// Runtime is the assembled application runtime. The main entrypoint only
// needs Router plus the lifecycle hooks to start and stop background work.
type Runtime struct {
	Router *gin.Engine
	close  func(context.Context) error
	start  func(context.Context) error

	mu         sync.Mutex
	state      runtimeState
	transition chan struct{}
	closeErr   error
}

// Start 启动 Runtime 所辖的后台组件（解析队列、各类 poller）。对 nil
// 接收者或未装配 start 的实例都是安全 no-op，方便测试构造空 Runtime。
func (r *Runtime) Start(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	switch r.state {
	case runtimeStarting, runtimeStarted:
		r.mu.Unlock()
		return ErrRuntimeAlreadyStarted
	case runtimeStopping, runtimeStopped:
		r.mu.Unlock()
		return ErrRuntimeClosed
	}
	r.state = runtimeStarting
	done := make(chan struct{})
	r.transition = done
	r.mu.Unlock()
	startCtx, cleanupBudget := lifecycle.WithCleanupBudget(
		context.WithoutCancel(ctx),
		defaultRuntimeCleanupTimeout,
	)
	defer cleanupBudget.Cancel()

	var startErr error
	if err := ctx.Err(); err != nil {
		startErr = err
	} else if r.start != nil {
		startErr = r.start(startCtx)
		if startErr == nil {
			startErr = ctx.Err()
		}
	}

	var startCleanupErr error
	if startErr != nil && r.close != nil {
		cleanupCtx, cancelCleanup := lifecycle.CleanupContext(startCtx, defaultRuntimeCleanupTimeout)
		startCleanupErr = r.close(cleanupCtx)
		cancelCleanup()
		if startCleanupErr != nil {
			startErr = errors.Join(startErr, fmt.Errorf("runtime start cleanup: %w", startCleanupErr))
		}
	}

	r.mu.Lock()
	r.closeErr = startCleanupErr
	if startErr != nil {
		r.state = runtimeStopped
	} else {
		r.state = runtimeStarted
	}
	r.transition = nil
	close(done)
	r.mu.Unlock()
	return startErr
}

// Close 触发优雅停机：按顺序停掉 poller、排空队列、关闭连接池并
// flush tracer。同 Start，对 nil 接收者或未装配 close 的实例 no-op。
func (r *Runtime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	for {
		r.mu.Lock()
		switch r.state {
		case runtimeStarting, runtimeStopping:
			done := r.transition
			r.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		case runtimeStopped:
			err := r.closeErr
			r.mu.Unlock()
			return err
		case runtimeConstructed, runtimeStarted:
			r.state = runtimeStopping
			done := make(chan struct{})
			r.transition = done
			r.mu.Unlock()

			var closeErr error
			if r.close != nil {
				closeErr = r.close(ctx)
			}

			r.mu.Lock()
			r.closeErr = closeErr
			r.state = runtimeStopped
			r.transition = nil
			close(done)
			r.mu.Unlock()
			return closeErr
		}
	}
}
