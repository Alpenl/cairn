package middleware

import (
	"log/slog"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

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
//
// Wave 2 H4（脚手架版）：如果请求 context 里已经有一个有效的 OTel
// SpanContext（trace.SpanContextFromContext(ctx).IsValid() == true），
// 把 trace_id / span_id 也 With 到 logger 上。当前 OTel tracer 是
// noop，SpanContext 永远无效，这条代码是空跑。等后续 wave 上
// go.opentelemetry.io/otel/sdk + OTLP exporter 后自动生效，零迁移成本。
//
// 风险点：trace.SpanContextFromContext 即便在 noop tracer 下也保证不
// panic、返回 zero-value SpanContext，IsValid() 返回 false。所以"未来兼容"
// 的代价是零。
func RequestID(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if !validRequestID(id) {
			id = uuid.NewString()
		}

		c.Set(RequestIDContextKey, id)
		c.Header(RequestIDHeader, id)

		// 先附 request_id，再叠 trace_id / span_id。顺序无关，但保持
		// "request_id 永远在 trace 字段前" 的字段顺序方便人肉看日志。
		reqLogger := observability.WithRequestID(logger, id)
		if span := trace.SpanContextFromContext(c.Request.Context()); span.IsValid() && reqLogger != nil {
			reqLogger = reqLogger.With(
				"trace_id", span.TraceID().String(),
				"span_id", span.SpanID().String(),
			)
		}
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
