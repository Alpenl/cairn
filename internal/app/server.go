package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"webtag/internal/database"
)

const (
	defaultShutdownTimeout   = 30 * time.Second
	defaultReadHeaderTimeout = 2 * time.Second
	defaultReadTimeout       = 10 * time.Second
	defaultWriteTimeout      = 15 * time.Second
	defaultIdleTimeout       = 60 * time.Second

	// shutdownHTTPBudgetRatio 是优雅停机预算中分配给 httpServer.Shutdown
	// 的比例。其余分给 Lifecycle.Close（worker 排空 + 连接池关闭）。
	// 70/30 是经验切分：HTTP 阶段的关键路径是等 in-flight 请求
	// 返回 + 关闭 keep-alive 连接（通常 < 1s 完成，但慢请求会拖长尾），
	// Lifecycle 阶段在 HTTP 完全静默后还要排空 queue（worker.Stop 自带
	// cancelRun，无新工作只剩当前 in-flight），30% 一般够用。两端任何
	// 一段都使用独立子预算，但只有 HTTP 成功排空才会进入 Lifecycle；
	// HTTP 超时会保留 Runtime 资源并把错误交给进程监督器。
	shutdownHTTPBudgetRatio = 0.7
)

// Lifecycle 抽象了 Server 启动前的初始化（如打开数据库连接池、启动
// 后台 goroutine）与停机时的清理动作。Runtime 就是它的标准实现。
type Lifecycle interface {
	Start(context.Context) error
	Close(context.Context) error
}

// ServerOptions 控制 NewServer 的所有可调参数。Listen 字段允许测试
// 注入自定义 listener；其他 Timeout 字段未设置时会落到包内 default*
// 常量上。
type ServerOptions struct {
	Addr              string
	Handler           http.Handler
	Lifecycle         Lifecycle
	Listen            func(network, addr string) (net.Listener, error)
	ShutdownTimeout   time.Duration
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

// Server 把 net/http.Server、Lifecycle 钩子和优雅停机超时绑定在一起，
// 是 main 真正调用 Run/Shutdown 的入口。
type Server struct {
	addr            string
	lifecycle       Lifecycle
	listen          func(network, addr string) (net.Listener, error)
	shutdownTimeout time.Duration
	httpServer      *http.Server
}

// NewServer 根据 opts 构造 Server，未设置的超时字段会回退到包内的
// default* 常量，Listen 字段未指定时使用 net.Listen。
func NewServer(opts ServerOptions) *Server {
	listen := opts.Listen
	if listen == nil {
		listen = net.Listen
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = defaultShutdownTimeout
	}
	if opts.ReadHeaderTimeout <= 0 {
		opts.ReadHeaderTimeout = defaultReadHeaderTimeout
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = defaultReadTimeout
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = defaultWriteTimeout
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = defaultIdleTimeout
	}

	return &Server{
		addr:            opts.Addr,
		lifecycle:       opts.Lifecycle,
		listen:          listen,
		shutdownTimeout: opts.ShutdownTimeout,
		httpServer: &http.Server{
			Addr:              opts.Addr,
			Handler:           opts.Handler,
			ReadHeaderTimeout: opts.ReadHeaderTimeout,
			ReadTimeout:       opts.ReadTimeout,
			WriteTimeout:      opts.WriteTimeout,
			IdleTimeout:       opts.IdleTimeout,
		},
	}
}

// Run 依序执行 Lifecycle.Start → listen → http.Server.Serve；ctx 取消或
// Serve 返回时都先通过 Shutdown 排空 in-flight handler，再关闭 Lifecycle。
// Serve 异常与停机错误会通过 errors.Join 一并返回。
func (s *Server) Run(ctx context.Context) error {
	if s.lifecycle != nil {
		if err := s.lifecycle.Start(ctx); err != nil {
			shutdownCtx, cancel := s.newShutdownContext()
			defer cancel()
			return errors.Join(err, s.closeLifecycle(shutdownCtx))
		}
	}

	listener, err := s.listen("tcp", s.addr)
	if err != nil {
		shutdownCtx, cancel := s.newShutdownContext()
		defer cancel()
		return errors.Join(err, s.closeLifecycle(shutdownCtx))
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.httpServer.Serve(listener)
	}()

	select {
	case err := <-errCh:
		shutdownCtx, cancel := s.newShutdownContext()
		defer cancel()
		shutdownErr := s.Shutdown(shutdownCtx)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return errors.Join(err, shutdownErr)
		}
		return shutdownErr
	case <-ctx.Done():
		shutdownCtx, cancel := s.newShutdownContext()
		defer cancel()
		return s.Shutdown(shutdownCtx)
	}
}

// Shutdown 两阶段优雅停机：
//
//  1. 阶段 1：httpServer.Shutdown — 拒收新连接、等已建立连接上的 in-flight
//     请求自然结束。分配总预算的 70%（shutdownHTTPBudgetRatio）。
//  2. 阶段 2：Lifecycle.Close — HTTP 静默后排空 worker queue、关闭 DB
//     连接池。分配剩余的 30%。
//
// HTTP drain 是 Lifecycle.Close 的硬屏障。阶段 1 返回错误时，仍可能有
// handler 在使用 Runtime 所辖的 pool 或 queue，因此必须保留这些
// 资源并把错误交给进程级终止策略；只有阶段 1 成功才进入阶段 2。
//
// 两阶段使用独立子预算，避免成功的 HTTP drain 消耗时间后把一个已经
// cancel 的 ctx 传给 Lifecycle.Close。ctx 仍是两阶段共同的上游取消
// 通道；调用者需要优雅停机时应传入 detached 且有界的 context。
func (s *Server) Shutdown(ctx context.Context) error {
	httpBudget, lifecycleBudget := splitShutdownBudget(s.shutdownTimeout)

	httpCtx, httpCancel := context.WithTimeout(ctx, httpBudget)
	defer httpCancel()
	if err := s.httpServer.Shutdown(httpCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.Join(err, database.ErrShutdownDeadline)
	}

	lifecycleCtx, lifecycleCancel := context.WithTimeout(ctx, lifecycleBudget)
	defer lifecycleCancel()
	return s.closeLifecycle(lifecycleCtx)
}

// splitShutdownBudget 把总预算 total 按 shutdownHTTPBudgetRatio 切成
// HTTP 阶段 / Lifecycle 阶段两段。提取为独立函数以便单测断言切分逻辑
// 而不需启动真实的 server.Run。
func splitShutdownBudget(total time.Duration) (httpBudget, lifecycleBudget time.Duration) {
	if total <= 0 {
		total = defaultShutdownTimeout
	}
	httpBudget = time.Duration(float64(total) * shutdownHTTPBudgetRatio)
	lifecycleBudget = total - httpBudget
	// 双重保险：极小 total（< 10ms）下浮点截断可能让两段都 ~0，给底线
	// 1ms 避免 context.WithTimeout 立即触发 DeadlineExceeded。
	if httpBudget <= 0 {
		httpBudget = time.Millisecond
	}
	if lifecycleBudget <= 0 {
		lifecycleBudget = time.Millisecond
	}
	return httpBudget, lifecycleBudget
}

func (s *Server) closeLifecycle(ctx context.Context) error {
	if s.lifecycle == nil {
		return nil
	}
	return s.lifecycle.Close(ctx)
}

// newShutdownContext 刻意从 context.Background() 派生，而非沿用触发关停的 ctx。
//
// 关停多半由那个 ctx 被取消触发（SIGTERM 或父 ctx 结束）；若在它之上派生，优雅
// 关停会在第一步就因已取消而放弃，连接排空、worker 收尾、连接池关闭全部跳过。
// 这里要的正是一段脱离触发者、只受 shutdownTimeout 约束的生命周期。
func (s *Server) newShutdownContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.shutdownTimeout)
}
