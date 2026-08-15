package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"webtag/internal/representation"
	"webtag/internal/session"
)

// corsAllowHeaders / corsAllowMethods 必须覆盖客户端**实际会发**的全集。
// 少一项的症状是「跨源部署下某个操作莫名其妙不工作」，而同源部署一切正常
// ——预检失败不会进 handler，服务端日志上什么都看不到。
//
// 逐项来源：
//   - PUT / PATCH：/api/feed-items/{id}/state、/api/subscriptions/{id}、
//     /api/links/{id}/content 是 PUT；/api/sites/{id}、
//     /api/library-classification-rules/{id} 是 PATCH。此前两个方法都不在
//     列表里，跨源改站点 / 标记已读的预检一律失败。
//   - If-Match：站点管理的乐观锁（reader/src/lib/api/client.ts 有 7 处）。
//   - If-None-Match：Reader 条件 GET 的缓存校验器。
//   - X-WebTag-Session：会话鉴权的 CSRF 头。不放行它，跨源部署下会话模式
//     连预检都过不去。
//   - Idempotency-Key：写类请求的重放保护（middleware/idempotency.go）。
const corsAllowHeaders = "Content-Type, Authorization, X-Request-ID, If-Match, If-None-Match, Idempotency-Key, " + session.HeaderName
const corsAllowMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
const corsExposeHeaders = representation.DataNamespaceHeader + ", ETag"

// CORS 返回一个处理跨域请求的 Gin 中间件：根据传入的允许 Origin 列表
// 设置 Access-Control-Allow-* 响应头，支持通过 "*" 放通所有来源；
// 对 OPTIONS 预检请求，未授权 Origin 直接返回 403，授权或同源返回 204。
func CORS(origins []string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(origins))
	allowAll := false

	for _, origin := range origins {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		if origin == "*" {
			allowAll = true
			continue
		}
		allowed[origin] = struct{}{}
	}

	return func(c *gin.Context) {
		// Always advertise that the response varies on Origin so any CDN /
		// shared cache in front of the API does not collapse two requests
		// from different origins into the same cache entry. This applies
		// to both the wildcard and the explicit-allow paths — without it
		// the wildcard response could be cached against an origin that is
		// not actually permitted on a follow-up request.
		c.Header("Vary", "Origin")
		origin := strings.TrimSpace(c.GetHeader("Origin"))
		originAllowed := false
		if origin != "" {
			if allowAll {
				c.Header("Access-Control-Allow-Origin", "*")
				originAllowed = true
			} else if _, ok := allowed[origin]; ok {
				c.Header("Access-Control-Allow-Origin", origin)
				originAllowed = true
				// 只有具名来源才能带凭证。CORS 规范禁止 `*` 与
				// Allow-Credentials 并存（浏览器会直接拒绝整个响应），所以
				// 这个头只挂在白名单分支上。
				//
				// 没有它，跨源部署下 httpOnly 会话根本用不起来：POST
				// /api/session 的 Set-Cookie 会被浏览器丢弃，前端静默回退到
				// 把 api key 存进 localStorage——正是会话模式要消灭的那个面。
				c.Header("Access-Control-Allow-Credentials", "true")
			}
			if originAllowed {
				c.Header("Access-Control-Allow-Methods", corsAllowMethods)
				c.Header("Access-Control-Allow-Headers", corsAllowHeaders)
				c.Header("Access-Control-Expose-Headers", corsExposeHeaders)
			}
		}

		if c.Request.Method == http.MethodOptions {
			// Preflight from an unauthorized origin should be rejected
			// outright. Returning 204 with no Access-Control-Allow-Origin
			// "works" only because browsers refuse to follow up — sending
			// 403 instead makes the rejection explicit so misconfigured
			// frontends fail loudly during development. The request never
			// reaches downstream handlers either way.
			if origin != "" && !originAllowed {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}
