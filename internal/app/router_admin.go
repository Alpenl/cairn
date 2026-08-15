package app

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/handler"
	"webtag/internal/middleware"
)

// registerPprofRoutes 把 net/http/pprof 的标准 handler 挂在 /debug/pprof/*
// 之下。Wave 12.5 H3 之后，pprof 默认关闭 —— heap/goroutine dump 中
// 包含进程内存里的字面量（含 token、API key 残片），开放就是数据泄露。
//
// 挂载条件（两个都满足才挂）：
//  1. opts.PProfEnabled = true（显式开启）
//  2. opts.MetricsAuthToken 非空（必须有 Bearer 守门）
//
// 任一不满足都跳过挂载并打 Info（PProfEnabled=true 但 token 缺失会打
// Warn，提醒运维"你以为打开了其实没"）。
func registerPprofRoutes(router *gin.Engine, opts RouterOptions) {
	if !opts.PProfEnabled {
		if opts.Logger != nil {
			opts.Logger.Info("pprof endpoints disabled (PPROF_ENABLED=false)")
		}
		return
	}
	if opts.MetricsAuthToken == "" {
		if opts.Logger != nil {
			opts.Logger.Warn("pprof endpoints disabled: PPROF_ENABLED=true but METRICS_AUTH_TOKEN is empty; refusing to expose unauthenticated profiler")
		}
		return
	}

	pprofHandler := middleware.BearerAuth(http.DefaultServeMux, opts.MetricsAuthToken)
	pprofGroup := router.Group("/debug/pprof")
	pprofGroup.Any("", gin.WrapH(pprofHandler))
	pprofGroup.Any("/*any", gin.WrapH(pprofHandler))
}

// registerPublicAPIRoutes 挂载 handler.RegisterRoutes 暴露的公开 API
// 路由（/api/ingest、/api/links*、/api/jobs*、/api/tags、/api/tree 及
// /api/v1/* 别名）。
func registerPublicAPIRoutes(router *gin.Engine, deps handler.Dependencies, opts RouterOptions) {
	authenticator := middleware.NewInstallationAuthenticator(opts.ExtensionAPIToken)
	guard := middleware.PublicAuth(middleware.PublicAuthOptions{
		Authenticator:   authenticator,
		AllowOpenAccess: opts.AllowOpenAccess,
		SessionKey:      opts.SessionSigningKey,
		Representations: opts.ConditionalGetRevisions,
	})
	groupMiddleware := []gin.HandlerFunc{guard}
	// 条件请求（If-None-Match → 304）挂在 guard 之后：guard 已解析 identity
	// namespace，ConditionalGet 再按 route policy 读取完整 component version。
	if opts.ConditionalGetRevisions != nil {
		policy := conditionalGetRoutes()
		if opts.ConditionalGetPolicyOverride != nil {
			policy = opts.ConditionalGetPolicyOverride
		}
		groupMiddleware = append(groupMiddleware,
			middleware.ConditionalGet(opts.ConditionalGetRevisions, policy))
	}
	// 聚合缓存失效必须排在 guard 之后。
	// 它是所有 HTTP 写路径的统一失效点——逐个 service 注入失效器意味着每加
	// 一个新写端点都要记得再接一次，而漏接不会有任何测试变红。
	if opts.AggregateInvalidator != nil {
		groupMiddleware = append(groupMiddleware, middleware.InvalidateAggregatesOnWrite(opts.AggregateInvalidator))
	}
	// Response replay must run after authentication: otherwise a caller that
	// knows another request's Idempotency-Key can receive its cached 2xx without
	// credentials.
	if opts.IdempotencyEnabled && opts.IdempotencyCache != nil {
		groupMiddleware = append(groupMiddleware, middleware.Idempotency(opts.IdempotencyCache))
	}
	apiGroup := router.Group("/", groupMiddleware...)
	handler.RegisterRoutes(apiGroup, deps)
	handler.RegisterSessionIdentityRoute(apiGroup)
	// 会话签发端点必须挂在鉴权 group 之外：它是登录入口，要求它先通过鉴权
	// 是循环依赖。SessionSigningKey 为空时 exchange registrar no-op（端点 404，
	// 客户端据此回退到 Bearer token 模式）。
	handler.RegisterSessionExchangeRoutes(router, handler.SessionOptions{
		SigningKey:      opts.SessionSigningKey,
		Authenticator:   authenticator,
		AllowOpenAccess: opts.AllowOpenAccess,
		Representations: opts.ConditionalGetRevisions,
	})
}

// registerAdminRoutes 把 /api/admin/* 路由挂在带 Bearer 鉴权的 group
// 下。fail-closed 策略：
//
//   - AdminAuthToken 非空 → 用 BearerAuthGin(token) 守门。
//   - AdminAuthToken 为空 + AppEnv == "dev" → 路由开放，方便本地调试。
//   - AdminAuthToken 为空 + AppEnv != "dev" → 仍然挂载，但中间件硬编
//     码到 401（BearerAuthGin("") 永远返回 401）。挂载本身不被跳过，
//     这样客户端拿到的是 401（"配错了 token"）而不是 404（"路由不
//     存在"），更便于运维排查。
//
// deps.ConceptMerges == nil 时直接跳过（与原来 handler.RegisterRoutes
// 内部对该字段的 nil 保护语义保持一致 —— 是可选依赖）。
func registerAdminRoutes(router *gin.Engine, deps handler.Dependencies, opts RouterOptions) {
	// 没有任何 admin service 可挂时直接返回（全是可选依赖）。
	if deps.ConceptMerges == nil {
		return
	}
	if opts.AdminAuthToken == "" && opts.isDevEnv() {
		// dev 豁免：直接挂在主 engine 上，等同于历史行为。
		handler.RegisterConceptMergeRoutes(router, deps.ConceptMerges)
		return
	}
	adminGroup := router.Group("/", middleware.BearerAuthGin(opts.AdminAuthToken))
	handler.RegisterConceptMergeRoutes(adminGroup, deps.ConceptMerges)
}
