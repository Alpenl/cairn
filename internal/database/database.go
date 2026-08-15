package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

const defaultDatabasePingTimeout = 5 * time.Second

// Querier 是 repository 层依赖的最小 SQL 接口，*pgxpool.Pool 与 pgx.Tx 都实现了它，
// 因此业务代码既可以直接用连接池，也可以在事务里复用同一份 SQL。
type Querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Pool 在 Querier 之上额外暴露 Ping 与 Close，用于健康检查和优雅停机。
type Pool interface {
	Querier
	Ping(ctx context.Context) error
	Close()
}

// Options bundles the operator-tunable pgxpool sizing and lifecycle knobs.
//
// 关于零值的语义：
//   - MaxConns / MinConns <= 0 时不覆盖 pgxpool 默认值（pgx 在 URL 未指定
//     pool 参数时，MaxConns 取 max(4, runtime.NumCPU())）。
//   - Tracer == nil 时不会安装 QueryTracer。
//   - MaxConnLifetime / MaxConnIdleTime / HealthCheckPeriod 为零值时同样
//     保持 pgx 默认（1h / 30min / 1min）。Wave 2 H6：让 PgBouncer / RDS
//     Proxy 等"中间件强制 reset 空闲连接"的部署可以把 lifetime/idle 调到
//     比中间件更短，避免 client 端拿到一根已被对端 reset 的连接。生产
//     推荐 30min / 10min / 1min。
type Options struct {
	MaxConns int
	MinConns int
	// MaxConnLifetime 是连接的最长存活时间。到期后 pgx 会在空闲时关闭
	// 并新开。0 = 保持 pgx 默认（1 小时）。生产建议设小于 PgBouncer /
	// RDS Proxy 的 server_idle_timeout，避免拿到已被对端 reset 的连接。
	MaxConnLifetime time.Duration
	// MaxConnIdleTime 是连接的最长空闲时间。超过则关闭，下次需要时新建。
	// 0 = 保持 pgx 默认（30 分钟）。生产建议 10 分钟左右。
	MaxConnIdleTime time.Duration
	// HealthCheckPeriod 是 pgx 主动 ping 空闲连接的间隔。0 = 保持 pgx
	// 默认（1 分钟）。降低可以更快发现死连接，代价是更多空载流量。
	HealthCheckPeriod time.Duration
	Tracer            pgx.QueryTracer
	// AdditionalTracers 是除主 Tracer 外要同时挂上的附加 tracer 列表，
	// Wave 2 H4 用来把 otelpgx 与现有的 Prometheus QueryTracer 串到同一
	// 个 pgx connection 上：multitracer.New 会按接口断言自动把每个 tracer
	// 加到对应的 QueryTracers / BatchTracers / ConnectTracers 等子集里，
	// 业务调用一次 Query 时两边各跑一遍，互不打扰。
	//   - 仅 Tracer 非 nil、AdditionalTracers 为空 → 直接挂 Tracer，行为同旧路径。
	//   - 仅 AdditionalTracers 有值、Tracer 为 nil → 用 multitracer 包附加 tracer。
	//   - 两者都有 → 用 multitracer 把 Tracer + AdditionalTracers 串起来。
	AdditionalTracers []pgx.QueryTracer
	// AcquisitionGate is installed only for the application runtime. It
	// rejects new persistence owners during shutdown while allowing contexts
	// carrying an owner admitted before shutdown to finish.
	AcquisitionGate *AcquisitionGate
	// PoolShutdown makes the application Runtime's synchronous pgxpool close
	// consume its cleanup deadline. Other process-lifetime pools may leave it nil.
	PoolShutdown *PoolShutdown
}

// Open builds and pings a *pgxpool.Pool. The Options struct is value-typed
// so callers can pass database.Options{} when the runtime does not need to
// pin sizing (tests / migrations).
func Open(ctx context.Context, databaseURL string, opts Options) (*pgxpool.Pool, error) {
	cfg, err := newPoolConfig(databaseURL, opts)
	if err != nil {
		return nil, err
	}
	return openConfiguredPool(ctx, cfg, opts, defaultDatabasePingTimeout)
}

func newPoolConfig(databaseURL string, opts Options) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse pgx config: %w", err)
	}
	applyPoolOptions(cfg, opts)
	applyTracer(cfg, opts)
	installPgvectorAfterConnect(cfg)
	installAcquisitionGate(cfg, opts.AcquisitionGate)
	if err := installPoolShutdown(cfg, opts.PoolShutdown); err != nil {
		return nil, fmt.Errorf("install pool shutdown: %w", err)
	}
	return cfg, nil
}

func openConfiguredPool(
	ctx context.Context,
	cfg *pgxpool.Config,
	opts Options,
	pingTimeout time.Duration,
) (*pgxpool.Pool, error) {
	if pingTimeout <= 0 {
		pingTimeout = defaultDatabasePingTimeout
	}
	// NewWithConfig uses its context for the asynchronous initial MinConns
	// constructors. Own that work independently so Ping failure can cancel it
	// before synchronous pool destruction waits on puddle's destructor barrier.
	initialConstructionCtx := ctx
	if opts.PoolShutdown != nil {
		var err error
		initialConstructionCtx, err = opts.PoolShutdown.bindInitialConstructionContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("bind initial pool construction: %w", err)
		}
	}

	pool, err := pgxpool.NewWithConfig(initialConstructionCtx, cfg)
	if err != nil {
		if opts.PoolShutdown != nil {
			opts.PoolShutdown.BeginShutdown()
		}
		return nil, fmt.Errorf("open pgx pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		if opts.PoolShutdown != nil {
			opts.PoolShutdown.BeginShutdown()
		}
		var closeErr error
		if opts.PoolShutdown != nil {
			closeErr = opts.PoolShutdown.Close(pingCtx, pool)
		} else {
			pool.Close()
		}
		return nil, errors.Join(fmt.Errorf("ping pgx pool: %w", err), closeErr)
	}

	// Successful Open transfers the cancel function to PoolShutdown so initial
	// MinConns warmup can finish. Runtime shutdown cancels any remaining work
	// before persistence Drain. Pools without that capability retain pgx's
	// process-lifetime construction behavior.
	return pool, nil
}

// applyPoolOptions 把 Options 的连接数与生命周期旋钮按"显式给值才覆盖"
// 的规则写进 pgxpool 配置，测试 / migrations 这类不关心生命周期的入口
// 不会被强制塞默认值。
func applyPoolOptions(cfg *pgxpool.Config, opts Options) {
	if opts.MaxConns > 0 {
		// gosec G115：MaxConns / MinConns 来自 config，已被 validateConfig
		// 约束在 [1, 1000]，int 截到 int32 不会溢出；保留 //nolint 而不是
		// 加 runtime 检查，避免双重校验。
		cfg.MaxConns = int32(opts.MaxConns) //nolint:gosec // reason: 配置层已约束在 [1,1000]
	}
	if opts.MinConns > 0 {
		cfg.MinConns = int32(opts.MinConns) //nolint:gosec // reason: 配置层已约束在 [1,1000]
	}
	if opts.MaxConnLifetime > 0 {
		cfg.MaxConnLifetime = opts.MaxConnLifetime
	}
	if opts.MaxConnIdleTime > 0 {
		cfg.MaxConnIdleTime = opts.MaxConnIdleTime
	}
	if opts.HealthCheckPeriod > 0 {
		cfg.HealthCheckPeriod = opts.HealthCheckPeriod
	}
}

// applyTracer 装配查询 tracer：单 tracer 直接挂；多 tracer 用 pgx 自带
// multitracer 组合成一个，cfg.ConnConfig.Tracer 字段只允许单一实例。
func applyTracer(cfg *pgxpool.Config, opts Options) {
	tracers := make([]pgx.QueryTracer, 0, 1+len(opts.AdditionalTracers))
	if opts.Tracer != nil {
		tracers = append(tracers, opts.Tracer)
	}
	for _, t := range opts.AdditionalTracers {
		if t != nil {
			tracers = append(tracers, t)
		}
	}
	switch len(tracers) {
	case 0:
		// no-op：不覆盖 pgx 默认（nil tracer）。
	case 1:
		cfg.ConnConfig.Tracer = tracers[0]
	default:
		cfg.ConnConfig.Tracer = multitracer.New(tracers...)
	}
}

// installPgvectorAfterConnect 挂接 pgvector 类型注册（Phase 5 v3.0 检索式
// 打标）：每根新连接建立后调 pgxvec.RegisterTypes，把 vector / halfvec /
// sparsevec 的 OID 绑定到 pgx 的类型映射上，concept.embedding 列才能用
// pgvector.Vector 直接读写（否则 pgx 把 vector 当未知类型按文本处理）。
// 注册在每根连接上做（而非全局），因为 pgx 的类型映射是 per-connection
// 的，且 vector 的 OID 是扩展安装后动态分配、连接级查询得到。
//
// ⚠️ 为什么先探测扩展再注册：pgxvec.RegisterTypes 在库里没有 vector
// 类型时返回 "vector type not found" 错误。如果在 AfterConnect 里无条件
// 调用，会让 *扩展尚未安装* 的连接全部获取失败 —— 而 cmd/migrate 自己
// 就是在扩展安装之前打开连接池（database.Open → Ping），随后才用 DDL
// 启用扩展；无条件注册会直接卡死迁移工具本身，也会破坏从 pre-pgvector
// 状态升级的部署。因此这里先用 to_regtype('vector') 探一下：扩展存在
// 才注册类型，不存在则跳过（migrate 工具按文本下发 DDL，根本不需要
// vector 类型；运行时服务总是在迁移之后才连库，那时扩展已就位）。
// 链式保留调用方既有的 AfterConnect（当前无人设置，防御性保留组合点）。
func installPgvectorAfterConnect(cfg *pgxpool.Config) {
	priorAfterConnect := cfg.AfterConnect
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if priorAfterConnect != nil {
			if err := priorAfterConnect(ctx, conn); err != nil {
				return err
			}
		}
		var vectorOID *uint32
		if err := conn.QueryRow(ctx, "SELECT to_regtype('vector')::oid").Scan(&vectorOID); err != nil {
			return fmt.Errorf("probe pgvector type: %w", err)
		}
		if vectorOID == nil {
			// 扩展未安装：跳过注册，不视为错误（见上方说明）。
			return nil
		}
		if err := pgxvec.RegisterTypes(ctx, conn); err != nil {
			return fmt.Errorf("register pgvector types: %w", err)
		}
		return nil
	}
}
