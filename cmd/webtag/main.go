// Command webtag 是 WebTag 后端 HTTP 服务的入口：加载配置 → 构建运行时（路由、依赖注入、生命周期）→
// 启动 HTTP server 并监听 SIGINT/SIGTERM 触发优雅关停。所有业务逻辑都在 internal/app 内组装，这里只负责
// 把进程信号、配置、运行时三者串起来。
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/automaxprocs/maxprocs"

	"webtag/internal/app"
	"webtag/internal/buildinfo"
	"webtag/internal/config"
)

func main() {
	if err := execute(os.Args[1:], os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func execute(args []string, stdout io.Writer) error {
	if handled, err := buildinfo.PrintVersion(args, stdout); handled {
		return err
	}

	_, _ = maxprocs.Set(maxprocs.Logger(log.Printf))
	return run()
}

// run 把启动逻辑从 main 抽离，使 defer stop()（signal.NotifyContext 注册的
// 反注册）在任何失败路径上都能跑——main 里 log.Fatal 走 os.Exit，defer 不
// 会触发（gocritic exitAfterDefer）。错误冒泡到 main 由 main 统一 Fatal。
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	runtime, err := app.BuildRuntime(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build runtime: %w", err)
	}

	server := app.NewServer(app.ServerOptions{
		Addr:              cfg.Server.ListenAddr,
		Handler:           runtime.Router,
		Lifecycle:         runtime,
		ReadHeaderTimeout: time.Duration(cfg.Server.ReadHeaderTimeoutMS) * time.Millisecond,
		ReadTimeout:       time.Duration(cfg.Server.ReadTimeoutMS) * time.Millisecond,
		WriteTimeout:      time.Duration(cfg.Server.WriteTimeoutMS) * time.Millisecond,
		IdleTimeout:       time.Duration(cfg.Server.IdleTimeoutMS) * time.Millisecond,
		// 优雅停机总预算（默认 30s，可由 SHUTDOWN_TIMEOUT_MS 调整）。
		// Server.Shutdown 把它按 70/30 拆给 HTTP / Lifecycle 两阶段。
		ShutdownTimeout: time.Duration(cfg.Server.ShutdownTimeoutMS) * time.Millisecond,
	})
	if err := server.Run(ctx); err != nil {
		return fmt.Errorf("run server: %w", err)
	}
	return nil
}
