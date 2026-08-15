package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// BearerAuth wraps next so that requests must carry a matching
// `Authorization: Bearer <token>` header. It is purposefully an
// http.Handler middleware (not a gin.HandlerFunc) so it can sit in
// front of the prometheus exposition handler without dragging in the
// full request/response middleware chain.
//
// expected MUST be non-empty; the caller is responsible for skipping
// the wrap entirely when no token is configured (the /metrics endpoint
// stays open by default to preserve back-compat with single-host
// deployments). The crypto/subtle.ConstantTimeCompare guard prevents
// timing-side-channel disclosure of a longer-lived scrape token.
func BearerAuth(next http.Handler, expected string) http.Handler {
	expectedBytes := []byte(expected)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got := []byte(strings.TrimSpace(header[len(prefix):]))
		if subtle.ConstantTimeCompare(got, expectedBytes) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// BearerAuthGin 是 BearerAuth 的 Gin 版本，用于保护 /api/admin/* 这类
// 走完整 gin 中间件链（CORS、AccessLog、RequestID）的路由：直接复用
// 上面的 http.Handler 版本会绕过 Gin 的 context，request_id 与 access
// log 都拿不到。
//
// expected 为空字符串时返回的中间件会拒掉所有请求 —— 这是有意为之的
// fail-closed 行为：admin 接口若未配置 token，不允许"裸奔上线"。配置
// 层的 warnUnsafeBootDefaults 会在启动时打印 Warn 提示运维补齐 token，
// 但路由本身就是 401，不会因为遗忘配置变成开放端口。
//
// expected 非空时使用 crypto/subtle.ConstantTimeCompare 防止 token 通过
// 时序侧信道泄露。
func BearerAuthGin(expected string) gin.HandlerFunc {
	expectedBytes := []byte(expected)
	expectedLen := len(expectedBytes)
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			c.Header("WWW-Authenticate", `Bearer realm="admin"`)
			JSONErrorWithSlug(c, http.StatusUnauthorized, ErrCodeUnauthorized, "unauthorized")
			return
		}
		got := []byte(strings.TrimSpace(header[len(prefix):]))
		// expectedLen==0（未配置 token）时 ConstantTimeCompare 永远返回
		// 0，无需特判即 fail-closed；显式比较长度只是为了让阅读者一眼
		// 看出 expected 为空时分支为何注定走到 401。
		if expectedLen == 0 || subtle.ConstantTimeCompare(got, expectedBytes) != 1 {
			c.Header("WWW-Authenticate", `Bearer realm="admin"`)
			JSONErrorWithSlug(c, http.StatusUnauthorized, ErrCodeUnauthorized, "unauthorized")
			return
		}
		c.Next()
	}
}
