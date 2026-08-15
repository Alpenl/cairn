package middleware

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"webtag/internal/session"
)

// SetSessionCookie 把会话 token 写成 httpOnly cookie。
//
// 属性逐条都有理由：
//   - HttpOnly：页面脚本读不到。这是整个方案的意义所在——Reader 会渲染来自
//     第三方网页的标题 / 摘要 / 保存的正文，一次 XSS 若能读到凭证就等于把它
//     永久交出去。读不到就带不走。
//   - SameSite=Strict：跨站请求不携带本 cookie，堵死 CSRF 的主路径。Reader 由
//     后端同源 serve，Strict 不影响正常使用。
//   - Secure：仅在 https 请求上设置。自托管常见 http://localhost，硬加 Secure
//     会让 cookie 根本存不下，登录静默失败。
//   - Path=/：/api/* 与 /reader/* 都要用到。
func SetSessionCookie(c *gin.Context, token string, expiresAt time.Time) {
	//nolint:gosec // reason: G124 要求 Secure 是常量 true 才认。这里 Secure 由
	// isTLSRequest 按请求是否经 https 到达决定——自托管常见 http://localhost，
	// 硬编码 true 会让 cookie 根本存不下、登录静默失败。HttpOnly 与
	// SameSite=Strict 均为常量，三个属性由 TestSessionCookieSecurityAttributes
	// 逐条断言（含「裸 http 不加 Secure」「反代 https 加 Secure」两个方向）。
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     session.CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
		HttpOnly: true,
		Secure:   isTLSRequest(c),
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearSessionCookie 让浏览器立刻丢弃会话 cookie（登出，或收到一张坏票时）。
// 属性必须与写入时一致，否则浏览器会认为是另一个 cookie 而不覆盖。
func ClearSessionCookie(c *gin.Context) {
	//nolint:gosec // reason: 同 SetSessionCookie —— Secure 条件化，其余属性必须
	// 与写入时逐字一致，否则浏览器会认为是另一个 cookie 而不覆盖。
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     session.CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isTLSRequest(c),
		SameSite: http.SameSiteStrictMode,
	})
}

// isTLSRequest 判断请求是否经 https 到达。反向代理终止 TLS 时 r.TLS 为 nil，
// 因此同时看 X-Forwarded-Proto。
func isTLSRequest(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	return c.GetHeader("X-Forwarded-Proto") == "https"
}
