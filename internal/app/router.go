package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"
	// gosec G108：net/http/pprof 注册的 handler 挂在 http.DefaultServeMux 上，
	// 而本服务用 Gin 自己的 Mux，因此 _ 导入本身并不暴露 /debug/pprof。
	// 实际暴露由 internal/app/admin_routes.go 在 admin token 鉴权之后显式
	// re-mount，详见 Wave 1 安全审计 H-7。
	_ "net/http/pprof" //nolint:gosec // reason: 仅副作用导入，路由 mount 由 admin token 守门

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"webtag/internal/handler"
	"webtag/internal/middleware"
	"webtag/internal/observability"
)

// RouterOptions 把"挂路由用到、但与业务依赖无关"的安全/可观测开关
// 收拢成单一参数，避免 NewRouterWithDependencies 的参数列表越长越随
// 意。所有字段都允许零值：零值代表"按 fail-closed 默认（pprof 关闭、
// admin 路由必须带 token，未配置 token 时一律 401）"。
type RouterOptions struct {
	// TrustedProxyCIDRs is the explicit ingress trust boundary used by
	// gin.Context.ClientIP. Empty means direct deployment: forwarded client IP
	// headers are ignored. Values come from validated config CIDRs.
	TrustedProxyCIDRs []string

	// AdminAuthToken 是 /api/admin/* 路由的 Bearer token。
	// 空字符串 + AppEnv != "dev" 时所有 admin 路由 100% 返回 401；
	// 空字符串 + AppEnv == "dev" 时 admin 路由对外开放（仅用于本地
	// 调试）。详见 config.Config.AdminAuthToken。
	AdminAuthToken string

	// MetricsAuthToken 是 /debug/pprof/* 共用的 Bearer token；同时也
	// 是把 pprof 限制到"只在有 token 时才允许暴露"的硬约束（即使
	// PProfEnabled=true，token 为空也会被拒挂载并打 Warn）。
	MetricsAuthToken string

	// PProfEnabled 是 pprof 路由的总开关，默认 false。允许临时打开
	// 排查问题，但必须搭配 MetricsAuthToken。详见
	// config.Config.PProfEnabled。
	PProfEnabled bool

	// AppEnv 同 config.Config.AppEnv，仅用于决定 dev 环境是否允许
	// admin 路由"无 token 开放"这个开发态豁免。
	AppEnv string

	// ExtensionAPIToken is the installation's only public API credential.
	ExtensionAPIToken string

	// AllowOpenAccess 透传 config.PublicAPIOpen：是否允许
	// 「零凭证即开放」。默认 false（fail-closed）。
	AllowOpenAccess bool

	// SessionSigningKey 是浏览器会话 cookie 的签名密钥（config.SessionSigningKey）。
	// 为空表示会话签发功能未启用：POST/DELETE /api/session 不挂载，鉴权链
	// 忽略 cookie；受公开 API guard 保护的 GET identity 仍然挂载。
	SessionSigningKey []byte

	// Logger 用来在路由装配时打 Warn（pprof 被强制关闭、admin token
	// 缺失等启动期就该被运维看到的信息）。nil 代表静默。
	Logger *slog.Logger

	// MaxRequestBodyBytes 控制 MaxRequestBody 中间件挂载的全局 body
	// 上限。0 或负数 → 不挂载中间件（保留旧行为）；> 0 → 挂在
	// SecurityHeaders 之后，所有路由生效。生产装配走 BuildRuntime 时
	// 取 middleware.DefaultMaxRequestBodyBytes（4 MiB，见 max_body.go——曾经用
	// 1 MiB 导致 ingest 回归，这里的注释一度还停在那个旧值）。
	MaxRequestBodyBytes int64

	// RequestDeadlineTimeout 是 RequestDeadline 中间件用来推导 per-request
	// ctx deadline 的基础（一般等于 cfg.Server.WriteTimeoutMS）。0 或负数
	// → 不挂载（旧行为）；> 0 → 与 RequestDeadlinePercent 联合派生
	// deadline。
	RequestDeadlineTimeout time.Duration

	// ConditionalGetRevisions 提供 authenticated representation 的 installation
	// namespace/component version base。空 component set 驱动 identity marker；
	// eligible GET 再按 route policy 读取完整版本。nil 时 marker/version 不可用。
	ConditionalGetRevisions middleware.VersionReader

	// ConditionalGetPolicyOverride is used by RF3 acceptance tests to exercise
	// candidate routes before RF3B enables any production policy. Nil always
	// selects conditionalGetRoutes(), which remains empty throughout RF3A.
	ConditionalGetPolicyOverride middleware.ConditionalGetPolicy

	// AggregateInvalidator 在成功的写请求之后失效安装级聚合缓存。
	// nil（测试装配常见）时中间件退化为 no-op。
	AggregateInvalidator middleware.AggregateInvalidator

	// GzipEnabled 控制响应压缩中间件的挂载（config.Config.GzipEnabled，
	// 默认 true）。false 时整层不挂载，行为与引入压缩之前逐字节一致。
	GzipEnabled bool

	// GzipMinLength 是启用压缩的响应体字节下限（config.Config.GzipMinLength）。
	// 0 或负数时 Gzip 中间件回退到 middleware.DefaultGzipMinLength。
	GzipMinLength int

	// RequestDeadlinePercent 是 deadline 占 RequestDeadlineTimeout 的比例。
	// 0 → 用默认 0.9（留 10% 给 handler ctx.Done 后的清理路径，避免
	// server 写超时关闭连接时 handler 还没来得及落 error response）。
	RequestDeadlinePercent float64

	// IdempotencyEnabled 控制是否在公开 API 鉴权之后挂载 Idempotency
	// 中间件。仅对带 Idempotency-Key 头的 POST/PUT/PATCH/DELETE 请求生效；
	// 无该头时透传。鉴权后的顺序保证 replay 不能绕过凭证检查。
	IdempotencyEnabled bool

	// IdempotencyCache 是 PG 支撑的幂等缓存实例（Phase 13 / v4.0 M2）。由
	// BuildRuntime 注入：它持有 idempotency_keys 表的 store 适配器与后台 TTL
	// 清理 goroutine，Stop 挂进 Runtime.Close。nil 且 IdempotencyEnabled=true
	// 时中间件退化为 no-op（无 store 可用），不再像旧 LRU 那样在路由层兜底
	// 自建——PG store 必须由装配层提供（路由层拿不到 pool）。
	IdempotencyCache *middleware.PGIdempotencyCache
}

// isDevEnv 判断当前是否运行在 dev 环境。"dev" 是唯一允许 admin 无
// token 开放的环境标记；staging / prod / 任何未识别值一律走 fail-closed
// 路径。
func (o RouterOptions) isDevEnv() bool {
	return o.AppEnv == "dev"
}

// ReadinessChecker 是 /ready 端点的就绪探针契约：返回 nil 视为
// healthy，返回错误时端点回 503 + "degraded"。
type ReadinessChecker interface {
	Ready(context.Context) error
}

// WithHealthExtraFields returns a gin middleware that attaches an additional
// field map onto the request context. The /health handler merges those fields
// into its JSON body, letting the runtime surface flags such as
// `unsafe_targets_allowed=true` for operator visibility without changing the
// NewRouterWithDependencies signature.
func WithHealthExtraFields(extra map[string]any) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("health_extra_fields", extra)
		c.Next()
	}
}

// NewRouterWithDependencies 是生产路由的真正装配点：注入业务依赖、
// metrics、readiness 与可选 middleware，统一挂载静态资源、/health、
// /ready、/metrics、/openapi.json、/docs、pprof 与 RegisterRoutes
// 暴露的 /api/* 路由。
//
// opts 是后加进来的安全/可观测开关包（admin token、pprof 总开关、
// 环境标识等），与业务依赖严格分开。详见 RouterOptions。extraMiddleware
// 仍保持 variadic 在尾巴，向后兼容现有 BuildRuntime 调用的拼装风格。
func NewRouterWithDependencies(deps handler.Dependencies, metricsHandler http.Handler, readiness ReadinessChecker, logger *slog.Logger, metrics *observability.Metrics, opts RouterOptions, extraMiddleware ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	if err := router.SetTrustedProxies(opts.TrustedProxyCIDRs); err != nil {
		panic("configure trusted proxies: " + err.Error())
	}
	// otelgin.Middleware 必须最先挂：它负责从 incoming HTTP 头里
	// (W3C TraceContext) 提取上游的 trace_id 并 start 一个 server span。
	// 放在 RequestID 之前的好处是后者的 trace.SpanContextFromContext
	// 拿到的就是真实 SpanContext，可以把 trace_id/span_id 注入 logger，
	// 让 access log / handler 日志与 trace 自然关联。
	//
	// service 字符串 "webtag" 同时是 resource.service.name，trace 后端
	// 会按这个分组（与 observability.InitTracer 内的默认值保持一致）。
	router.Use(otelgin.Middleware("webtag"))
	installRouterMiddleware(router, logger, metrics, opts, extraMiddleware...)
	registerStaticAndHealthRoutes(router, readiness, logger, metricsHandler)
	// /debug/pprof/* – profiling endpoints. Wave 12.5 H3 把 pprof 默认
	// 关闭：必须 opts.PProfEnabled=true 且 opts.MetricsAuthToken 非空才
	// 挂载，否则只打 Info 跳过。
	registerPprofRoutes(router, opts)
	// 公开 API 路由（/api/ingest、/api/links*、/api/jobs*、/api/tags、
	// /api/tree 及 /api/v1/* 别名）
	// 默认 fail-closed。这里曾经是 opt-in（留空即开放，对齐 /metrics 的策略），
	// 而那个默认值意味着：一个 docker run 起来的自托管实例，端口一暴露，任何人
	// 都能读走全部链接、导出整库、删数据。
	// PublicAuth accepts the static installation token, a valid browser session,
	// or explicitly enabled anonymous access. Empty configuration stays closed.
	registerPublicAPIRoutes(router, deps, opts)
	// admin 路由单独走带鉴权的 group：dev 环境 + 无 token 时退回开放，
	// 其余情况一律要求 Bearer token（无 token 即 401 fail-closed）。
	registerAdminRoutes(router, deps, opts)
	return router
}
