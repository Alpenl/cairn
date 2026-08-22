package middleware

import (
	"log/slog"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/observability"
)

// RequestIDHeader 是 RequestID 中间件读取与回写的 HTTP 头名称。
const (
	RequestIDHeader = "X-Request-ID"
	// RequestIDContextKey 是 RequestID 写入 gin.Context 的键名，便于业务侧通过 c.GetString 取出。
	RequestIDContextKey = "request_id"

	maxRequestIDLength = 128
)

// RequestID 返回一个 Gin 中间件：校验或生成请求 ID，写入 gin.Context、回填响应头，
// 并把带 request_id 字段的 logger 注入到请求 context 中，供下游 handler 通过
// observability.FromContext 取出做关联日志。
func RequestID(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if !validRequestID(id) {
			id = uuid.NewString()
		}

		c.Set(RequestIDContextKey, id)
		c.Header(RequestIDHeader, id)

		reqLogger := observability.WithRequestID(logger, id)
		c.Request = c.Request.WithContext(observability.ContextWithLogger(c.Request.Context(), reqLogger))
		c.Next()
	}
}

// validRequestID returns true when the supplied request id can be safely
// echoed back into headers and structured logs. It rejects empty strings,
// values longer than maxRequestIDLength, and anything containing characters
// outside the [A-Za-z0-9_-] alphabet (which keeps newlines, ANSI escapes,
// and other control bytes out of the log pipeline).
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}
