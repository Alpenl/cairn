package app

import (
	"github.com/gin-gonic/gin"

	"webtag/internal/handler"
	"webtag/internal/middleware"
)

// registerPublicAPIRoutes 挂载 handler.RegisterRoutes 暴露的公开 API
// 路由（/api/ingest、/api/links*、/api/tags、/api/tree）。
func registerPublicAPIRoutes(router *gin.Engine, deps handler.Dependencies, opts RouterOptions) {
	authenticator := middleware.NewInstallationAuthenticator(opts.ExtensionAPIToken)
	guard := middleware.PublicAuth(middleware.PublicAuthOptions{
		Authenticator:  authenticator,
		SessionKey:     opts.SessionSigningKey,
		IdentityReader: opts.InstallationIdentity,
	})
	groupMiddleware := []gin.HandlerFunc{guard}
	// Response replay must run after authentication: otherwise a caller that
	// knows another request's Idempotency-Key can receive its cached 2xx without
	// credentials.
	if opts.IdempotencyCache != nil {
		groupMiddleware = append(groupMiddleware, middleware.Idempotency(opts.IdempotencyCache))
	}
	apiGroup := router.Group("/", groupMiddleware...)
	handler.RegisterRoutes(apiGroup, deps)
	handler.RegisterSessionIdentityRoute(apiGroup)
	// 会话签发端点必须挂在鉴权 group 之外：它是登录入口，要求它先通过鉴权
	// 是循环依赖。SessionSigningKey 为空时 exchange registrar no-op（端点 404，
	// 客户端据此回退到 Bearer token 模式）。
	handler.RegisterSessionExchangeRoutes(router, handler.SessionOptions{
		SigningKey:     opts.SessionSigningKey,
		Authenticator:  authenticator,
		IdentityReader: opts.InstallationIdentity,
	})
}
