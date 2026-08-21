// Package middleware 汇总了 WebTag HTTP 层使用的 Gin 中间件，
// 包括请求 ID 注入与日志染色（RequestID）、访问日志（AccessLog）、
// CORS 处理（CORS）、Panic 恢复与统一 JSON 错误响应（Recovery / JSONError）、
// 请求 ID、访问日志与 panic 恢复等 HTTP 基础中间件。
package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"webtag/internal/observability"
)

// AccessLog 返回一个 Gin 中间件，在请求结束后输出结构化访问日志，
// 包含方法、路径、路由、状态码与耗时。当 logger 为 nil 时退化为无操作中间件。
func AccessLog(logger *slog.Logger) gin.HandlerFunc {
	if logger == nil {
		return func(c *gin.Context) {
			c.Next()
		}
	}

	return func(c *gin.Context) {
		startedAt := time.Now()
		c.Next()

		// 优先使用 RequestID 中间件注入到 context 中的 logger（带 request_id 字段），
		// 这样访问日志与业务日志能通过同一 request_id 串起来；缺失时退回到原始 logger。
		requestLogger := observability.FromContext(c.Request.Context())
		if requestLogger == nil {
			requestLogger = logger
		}

		requestLogger.Info("http request completed",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"route", c.FullPath(),
			"status", c.Writer.Status(),
			"latency_ms", time.Since(startedAt).Milliseconds(),
		)
	}
}
