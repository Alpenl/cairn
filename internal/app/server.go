package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

const (
	defaultShutdownTimeout   = 30 * time.Second
	defaultReadHeaderTimeout = 2 * time.Second
	defaultReadTimeout       = 10 * time.Second
	defaultWriteTimeout      = 15 * time.Second
	defaultIdleTimeout       = 60 * time.Second
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

// Shutdown drains HTTP before stopping backgrounds and closing PostgreSQL.
// The caller owns the deadline for the whole shutdown sequence.
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.httpServer.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return s.closeLifecycle(ctx)
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
