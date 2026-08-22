package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"webtag/internal/handler"
	"webtag/internal/middleware"
)

// RouterOptions 把"挂路由用到、但与业务依赖无关"的安全/可观测开关
// 收拢成单一参数，避免 NewRouterWithDependencies 的参数列表越长越随
// 意。所有字段都允许零值。
type RouterOptions struct {
	// TrustedProxyCIDRs is the explicit ingress trust boundary used by
	// gin.Context.ClientIP. Empty means direct deployment: forwarded client IP
	// headers are ignored. Values come from validated config CIDRs.
	TrustedProxyCIDRs []string

	// ExtensionAPIToken is the installation's only public API credential.
	ExtensionAPIToken string

	// SessionSigningKey 是浏览器会话 cookie 的签名密钥（config.SessionSigningKey）。
	// 为空表示会话签发功能未启用：POST/DELETE /api/session 不挂载，鉴权链
	// 忽略 cookie；受公开 API guard 保护的 GET identity 仍然挂载。
	SessionSigningKey []byte

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

	// InstallationIdentity provides the stable namespace used to partition
	// Reader data. nil means authenticated API access cannot establish identity.
	InstallationIdentity middleware.IdentityReader

	// RequestDeadlinePercent 是 deadline 占 RequestDeadlineTimeout 的比例。
	// 0 → 用默认 0.9（留 10% 给 handler ctx.Done 后的清理路径，避免
	// server 写超时关闭连接时 handler 还没来得及落 error response）。
	RequestDeadlinePercent float64

	// IdempotencyCache 是 PG 支撑的幂等缓存实例（Phase 13 / v4.0 M2）。由
	// BuildRuntime 注入：它持有 idempotency_keys 表的 store 适配器与后台 TTL
	// 清理 goroutine，Stop 挂进 Runtime.Close。nil 只用于不装配数据库的窄测试；
	// 生产启动必须构造 PG cache。
	IdempotencyCache *middleware.PGIdempotencyCache
}

// ReadinessChecker 是 /ready 端点的就绪探针契约：返回 nil 视为
// healthy，返回错误时端点回 503 + "degraded"。
type ReadinessChecker interface {
	Ready(context.Context) error
}

// NewRouterWithDependencies 是生产路由的真正装配点：注入业务依赖、
// readiness 与可选 middleware，统一挂载 Reader、/health、/ready 与 RegisterRoutes
// 暴露的 /api/* 路由。
//
// opts 是安全与运行时行为开关，与业务依赖严格分开。详见
// RouterOptions。extraMiddleware
// 仍保持 variadic 在尾巴，向后兼容现有 BuildRuntime 调用的拼装风格。
func NewRouterWithDependencies(deps handler.Dependencies, readiness ReadinessChecker, logger *slog.Logger, opts RouterOptions, extraMiddleware ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	if err := router.SetTrustedProxies(opts.TrustedProxyCIDRs); err != nil {
		panic("configure trusted proxies: " + err.Error())
	}
	installRouterMiddleware(router, logger, opts, extraMiddleware...)
	registerOperationalRoutes(router, readiness, logger)
	// 公开 API 路由（/api/ingest、/api/links*、/api/tags、
	// /api/tree）
	// PublicAuth accepts the static installation token or a valid browser session.
	registerPublicAPIRoutes(router, deps, opts)
	return router
}
