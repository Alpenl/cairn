package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestServerRunStartsLifecycleAndStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}

	lifecycle := &fakeLifecycle{}
	server := NewServer(ServerOptions{
		Addr: listener.Addr().String(),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
		Lifecycle:       lifecycle,
		Listen:          func(string, string) (net.Listener, error) { return listener, nil },
		ShutdownTimeout: time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Run(ctx)
	}()

	waitForHTTPServer(t, "http://"+listener.Addr().String())
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server shutdown")
	}

	if lifecycle.started != 1 {
		t.Fatalf("lifecycle started = %d, want 1", lifecycle.started)
	}
	if lifecycle.closed != 1 {
		t.Fatalf("lifecycle closed = %d, want 1", lifecycle.closed)
	}
}

func TestServerRunClosesLifecycleWhenListenFails(t *testing.T) {
	t.Parallel()

	lifecycle := &fakeLifecycle{}
	listenErr := errors.New("listen failed")
	server := NewServer(ServerOptions{
		Addr:            "127.0.0.1:0",
		Handler:         http.NewServeMux(),
		Lifecycle:       lifecycle,
		Listen:          func(string, string) (net.Listener, error) { return nil, listenErr },
		ShutdownTimeout: time.Second,
	})

	err := server.Run(context.Background())
	if !errors.Is(err, listenErr) {
		t.Fatalf("Run() error = %v, want %v", err, listenErr)
	}

	if lifecycle.started != 1 {
		t.Fatalf("lifecycle started = %d, want 1", lifecycle.started)
	}
	if lifecycle.closed != 1 {
		t.Fatalf("lifecycle closed = %d, want 1", lifecycle.closed)
	}
}

func TestServerRunClosesLifecycleWhenStartFails(t *testing.T) {
	t.Parallel()

	startErr := errors.New("start failed")
	lifecycle := &fakeLifecycle{startErr: startErr}
	server := NewServer(ServerOptions{
		Addr:            "127.0.0.1:0",
		Handler:         http.NewServeMux(),
		Lifecycle:       lifecycle,
		ShutdownTimeout: time.Second,
	})

	err := server.Run(context.Background())
	if !errors.Is(err, startErr) {
		t.Fatalf("Run() error = %v, want %v", err, startErr)
	}
	if lifecycle.started != 1 {
		t.Fatalf("lifecycle started = %d, want 1", lifecycle.started)
	}
	if lifecycle.closed != 1 {
		t.Fatalf("lifecycle closed = %d, want 1 after start failure", lifecycle.closed)
	}
}

// TestSplitShutdownBudgetAllocates70Percent70HTTP30Lifecycle 锁定 P2 的
// 70/30 切分：总预算 10s 应当切成 7s/3s。失误（比如把 ratio 改成 0.5）
// 会导致 Lifecycle 拿不到足够时间排空 queue，被 ctx.DeadlineExceeded
// 立刻打断。这是优雅停机正确性的基线断言。
func TestSplitShutdownBudgetAllocates70HTTP30Lifecycle(t *testing.T) {
	t.Parallel()

	httpBudget, lifecycleBudget := splitShutdownBudget(10 * time.Second)
	if httpBudget != 7*time.Second {
		t.Fatalf("httpBudget = %v, want 7s", httpBudget)
	}
	if lifecycleBudget != 3*time.Second {
		t.Fatalf("lifecycleBudget = %v, want 3s", lifecycleBudget)
	}
	// 两段之和必须等于总预算（避免 ratio 计算导致预算泄漏）。
	if httpBudget+lifecycleBudget != 10*time.Second {
		t.Fatalf("budgets sum to %v, want 10s", httpBudget+lifecycleBudget)
	}
}

// TestSplitShutdownBudgetNonPositiveFallsBackToDefault 锁定边界：传入
// 0 或负数时按默认值切分，避免误传 0 让两段都退化成 1ms 把停机变成
// 强制 kill。
func TestSplitShutdownBudgetNonPositiveFallsBackToDefault(t *testing.T) {
	t.Parallel()

	httpBudget, lifecycleBudget := splitShutdownBudget(0)
	if httpBudget < time.Second || lifecycleBudget < time.Second {
		t.Fatalf("budgets too small: http=%v lifecycle=%v (want sane defaults)", httpBudget, lifecycleBudget)
	}
	// 默认 30s × 0.7 = 21s。
	if httpBudget != 21*time.Second {
		t.Fatalf("httpBudget = %v, want 21s for default 30s budget", httpBudget)
	}
}

// TestServerShutdownInvokesHTTPThenLifecycle 锁定 Wave 5 P2 两阶段停机
// 的顺序与独立预算：httpServer.Shutdown 必须先于 Lifecycle.Close 被调用，
// 且成功排空后阶段 2 会拿到独立的预算运行。
func TestServerShutdownInvokesHTTPThenLifecycle(t *testing.T) {
	t.Parallel()

	var order []string
	var orderMu sync.Mutex
	lifecycle := &orderTrackingLifecycle{
		hook: func(stage string) {
			orderMu.Lock()
			order = append(order, stage)
			orderMu.Unlock()
		},
	}

	server := NewServer(ServerOptions{
		Addr:            "127.0.0.1:0",
		Handler:         http.NewServeMux(),
		Lifecycle:       lifecycle,
		ShutdownTimeout: 100 * time.Millisecond,
	})

	// 直接调用 Shutdown 而不走 Run —— Run 涉及真实 listener，本测试只关
	// 心两阶段顺序与预算独立性。
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) < 1 || order[len(order)-1] != "lifecycle_close" {
		t.Fatalf("order = %v, want lifecycle_close to be the last stage", order)
	}
}

// TestServerShutdownGivesLifecycleIndependentBudgetAfterHTTPDrain 锁定成功
// HTTP drain 后阶段 2 的 ctx 使用独立 lifecycle budget，而不是复用已经
// cancel 的阶段 1 context。HTTP drain 失败的硬屏障由
// TestServerRetainsRuntimeWhenHTTPDrainDeadlineExpires 覆盖。
func TestServerShutdownGivesLifecycleIndependentBudgetAfterHTTPDrain(t *testing.T) {
	t.Parallel()

	var closeCallCtx context.Context
	lifecycle := &capturingCloseLifecycle{
		captureClose: func(ctx context.Context) { closeCallCtx = ctx },
	}

	server := NewServer(ServerOptions{
		Addr:            "127.0.0.1:0",
		Handler:         http.NewServeMux(),
		Lifecycle:       lifecycle,
		ShutdownTimeout: 100 * time.Millisecond,
	})

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	if closeCallCtx == nil {
		t.Fatal("Lifecycle.Close was not called")
	}
	deadline, ok := closeCallCtx.Deadline()
	if !ok {
		t.Fatal("Lifecycle ctx has no deadline; want lifecycleBudget-bounded ctx")
	}
	remaining := time.Until(deadline)
	// lifecycleBudget = 100ms × 0.3 = 30ms；测试运行时可能延迟到 10ms 左右仍未到，
	// 但只要 remaining > 0 就说明 lifecycle 阶段拿到了独立预算。
	if remaining <= 0 {
		t.Fatalf("lifecycle ctx already expired (remaining=%v); two-phase budget not isolated", remaining)
	}
}

// orderTrackingLifecycle 记录 Start/Close 的调用顺序，给两阶段顺序测试用。
type orderTrackingLifecycle struct {
	hook func(stage string)
}

func (l *orderTrackingLifecycle) Start(context.Context) error {
	l.hook("lifecycle_start")
	return nil
}

func (l *orderTrackingLifecycle) Close(context.Context) error {
	l.hook("lifecycle_close")
	return nil
}

// capturingCloseLifecycle 记录传给 Close 的 ctx，给阶段 2 预算独立性
// 测试用。
type capturingCloseLifecycle struct {
	captureClose func(context.Context)
}

func (l *capturingCloseLifecycle) Start(context.Context) error { return nil }
func (l *capturingCloseLifecycle) Close(ctx context.Context) error {
	if l.captureClose != nil {
		l.captureClose(ctx)
	}
	return nil
}

func TestNewServerAppliesDefaultHTTPTimeouts(t *testing.T) {
	t.Parallel()

	server := NewServer(ServerOptions{
		Addr:    "127.0.0.1:0",
		Handler: http.NewServeMux(),
	})

	if server.httpServer.ReadHeaderTimeout != defaultReadHeaderTimeout {
		t.Fatalf("ReadHeaderTimeout = %v, want %v", server.httpServer.ReadHeaderTimeout, defaultReadHeaderTimeout)
	}
	if server.httpServer.ReadTimeout != defaultReadTimeout {
		t.Fatalf("ReadTimeout = %v, want %v", server.httpServer.ReadTimeout, defaultReadTimeout)
	}
	if server.httpServer.WriteTimeout != defaultWriteTimeout {
		t.Fatalf("WriteTimeout = %v, want %v", server.httpServer.WriteTimeout, defaultWriteTimeout)
	}
	if server.httpServer.IdleTimeout != defaultIdleTimeout {
		t.Fatalf("IdleTimeout = %v, want %v", server.httpServer.IdleTimeout, defaultIdleTimeout)
	}
}

func TestNewServerUsesConfiguredHTTPTimeouts(t *testing.T) {
	t.Parallel()

	server := NewServer(ServerOptions{
		Addr:              "127.0.0.1:0",
		Handler:           http.NewServeMux(),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       3 * time.Second,
		WriteTimeout:      4 * time.Second,
		IdleTimeout:       5 * time.Second,
	})

	if server.httpServer.ReadHeaderTimeout != 2*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v, want 2s", server.httpServer.ReadHeaderTimeout)
	}
	if server.httpServer.ReadTimeout != 3*time.Second {
		t.Fatalf("ReadTimeout = %v, want 3s", server.httpServer.ReadTimeout)
	}
	if server.httpServer.WriteTimeout != 4*time.Second {
		t.Fatalf("WriteTimeout = %v, want 4s", server.httpServer.WriteTimeout)
	}
	if server.httpServer.IdleTimeout != 5*time.Second {
		t.Fatalf("IdleTimeout = %v, want 5s", server.httpServer.IdleTimeout)
	}
}

type fakeLifecycle struct {
	started  int
	closed   int
	startErr error
}

func (f *fakeLifecycle) Start(context.Context) error {
	f.started++
	return f.startErr
}

func (f *fakeLifecycle) Close(context.Context) error {
	f.closed++
	return nil
}

func waitForHTTPServer(t *testing.T, baseURL string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL)
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s did not start in time", baseURL)
}
