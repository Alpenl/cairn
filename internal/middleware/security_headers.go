package middleware

import "github.com/gin-gonic/gin"

// SecurityHeaders 返回一个 Gin 中间件，为所有响应注入一组与具体业务无关
// 的基础安全响应头。这里只覆盖"应用层一定该设、出错代价最小"的四项：
//
//   - X-Content-Type-Options: nosniff          —— 阻止浏览器把响应猜成
//     和 Content-Type 不一致的可执行类型（脚本/样式表的 MIME sniff）。
//   - X-Frame-Options: DENY                    —— 拒绝任何来源把页面
//     iframe 嵌入，防 clickjacking。前端走的是同源 SPA，没有 iframe 需求。
//   - Referrer-Policy: no-referrer             —— 跳出本站时不带 Referer，
//     避免把内部 URL（含 query 参数里的搜索词、token 等）泄露给第三方。
//   - Cross-Origin-Opener-Policy: same-origin  —— 把 BroadcastChannel /
//     window.opener 等跨源能力隔离到同源 browsing context，防侧信道。
//
// 故意不设的两项：
//
//   - HSTS（Strict-Transport-Security）：HSTS 必须在 HTTPS 已经稳定下发
//     的边界（反向代理 / CDN）上启用；从应用进程发，开发环境的 http://
//     一旦命中浏览器就会被永久升级到 https://，调试会很难受。生产环境
//     该由反代统一下发。
//   - CSP（Content-Security-Policy）：Reader 的构建产物需要单独审查资源
//     策略；在没有覆盖前不由通用 API 中间件猜测一套规则。
//
// 中间件无状态、不读取响应体，可安全挂在最前面（RequestID 之后、CORS
// 之前），不会与下游 handler 写入的内容冲突。已存在的响应头不会被覆盖。
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		// 仅在未设置时写入，给单点 handler 留出覆盖的余地（极少需要，
		// 但保留显式入口比硬覆盖更友好）。
		if h.Get("X-Content-Type-Options") == "" {
			h.Set("X-Content-Type-Options", "nosniff")
		}
		if h.Get("X-Frame-Options") == "" {
			h.Set("X-Frame-Options", "DENY")
		}
		if h.Get("Referrer-Policy") == "" {
			h.Set("Referrer-Policy", "no-referrer")
		}
		if h.Get("Cross-Origin-Opener-Policy") == "" {
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
		}
		c.Next()
	}
}
