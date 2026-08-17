// Command migrate 是数据库 schema 迁移的独立入口：从 DATABASE_URL 打开连接池，
// 调用 internal/migrate.Up 把自动迁移按序应用到目标库；设置 MIGRATION_TARGET
// 时改用 internal/migrate.UpTo 显式迁移到指定版本。保留值 fresh 是空库安装
// 的显式别名。设计为一次性运行：
// 迁移完成即退出，部署流水线在服务启动前调用，避免 webtag server 在线热迁移。
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/buildinfo"
	"webtag/internal/database"
	"webtag/internal/migrate"
	"webtag/internal/repository"
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
	return run(stdout)
}

// run 把启动逻辑从 main 抽离，使 defer（signal stop / pool.Close）能在
// 任何失败路径上正常执行——main 里 log.Fatal 会直接 os.Exit，defer 不
// 会跑（gocritic exitAfterDefer）。把错误冒泡到 main 由 main 一次性
// Fatal，是 Go 的标准 idiom。
func run(stdout io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	pool, err := database.Open(ctx, databaseURL, database.Options{})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	target := strings.TrimSpace(os.Getenv("MIGRATION_TARGET"))
	switch target {
	case "":
		err = migrate.Up(ctx, pool)
	case migrate.FreshInstallTarget:
		err = migrate.UpFreshInstall(ctx, pool)
	default:
		err = migrate.UpTo(ctx, pool, target)
	}
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return backfillTodoProjections(ctx, stdout, pool)
}

// backfillTodoProjections runs the one-shot TODO projection backfill after the
// schema is in place. It lives in the deploy-time migrate command rather than
// in the server because it must finish before the Reader starts serving TODO
// reads from the projection alone. A repeated deploy is a no-op: the backfill
// records completion in its own ledger and reports the earlier run.
func backfillTodoProjections(ctx context.Context, stdout io.Writer, pool *pgxpool.Pool) error {
	result, err := repository.NewPGXReaderVNextRepository(pool).BackfillTodoProjections(ctx)
	if err != nil {
		return fmt.Errorf("backfill reader TODO projections: %w", err)
	}
	if result.AlreadyComplete {
		fmt.Fprintf(stdout, "reader TODO projection backfill already completed at %s (%d projections)\n",
			result.CompletedAt.UTC().Format(time.RFC3339), result.ProjectedCount)
		return nil
	}
	fmt.Fprintf(stdout, "reader TODO projection backfill completed (%d projections)\n", result.ProjectedCount)
	return nil
}
