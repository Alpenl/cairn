package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	"webtag/internal/observability"
)

func TestValidRequestIDAccepted(t *testing.T) {
	cases := []string{
		"req-123",
		"abc_DEF-789",
		"0123456789",
		strings.Repeat("a", maxRequestIDLength),
	}
	for _, c := range cases {
		if !validRequestID(c) {
			t.Fatalf("validRequestID(%q) = false, want true", c)
		}
	}
}

func TestValidRequestIDRejected(t *testing.T) {
	cases := []string{
		"", // empty
		strings.Repeat("a", maxRequestIDLength+1), // too long
		"req\nid",            // newline
		"req\rid",            // carriage return
		"req id",             // space
		"\x1b[31mred\x1b[0m", // ANSI escapes
		"req\x00id",          // NUL
		"req/id",             // slash
		"req:id",             // colon
		"中文",                 // non-ASCII
	}
	for _, c := range cases {
		if validRequestID(c) {
			t.Fatalf("validRequestID(%q) = true, want false", c)
		}
	}
}

// TestRequestID_AttachesTraceIDFromContext 验证 Wave 2 H4 脚手架：当
// 请求 ctx 里已经携带有效 SpanContext 时，logger 注入了 trace_id / span_id
// 字段。当前 tracer 是 noop，但 trace.ContextWithSpanContext 可以直接构造
// 一个 valid SpanContext 喂给中间件，绕过对真实 SDK 的依赖。这条测试也
// 顺带保证：未来 OTLP SDK 上线后这个字段是真的会被串起来。
func TestRequestID_AttachesTraceIDFromContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 构造一个固定的 trace/span ID，便于断言。SpanContext 必须带 Sampled
	// flag 才会被 IsValid() 认为有效。
	traceID, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	spanID, _ := trace.SpanIDFromHex("1112131415161718")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	if !sc.IsValid() {
		t.Fatalf("constructed SpanContext is not valid; check trace/span IDs")
	}

	// 把 SpanContext 注入到请求 ctx，并用 JSON handler 捕获 logger 输出。
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	router := gin.New()
	router.Use(RequestID(logger))
	router.GET("/ping", func(c *gin.Context) {
		// 从 ctx 取出 logger 写一行；要求 trace_id / span_id 都已 With 上。
		l := observability.FromContext(c.Request.Context())
		if l == nil {
			t.Fatal("logger not present in request context")
		}
		l.Info("hit")
		c.String(http.StatusOK, "ok")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req = req.WithContext(trace.ContextWithSpanContext(req.Context(), sc))
	router.ServeHTTP(rec, req)

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("logger output not JSON: %q (%v)", buf.String(), err)
	}
	if got := out["trace_id"]; got != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("trace_id = %v, want fixed hex", got)
	}
	if got := out["span_id"]; got != "1112131415161718" {
		t.Errorf("span_id = %v, want fixed hex", got)
	}
}

// TestRequestID_OmitsTraceIDWhenSpanContextInvalid 边界用例：没有 OTel
// SpanContext 时 logger 不应当多出 trace_id 字段——避免日志里凭空出现
// "00000000000000000000000000000000" 之类的空 trace id 干扰排查。
func TestRequestID_OmitsTraceIDWhenSpanContextInvalid(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	router := gin.New()
	router.Use(RequestID(logger))
	router.GET("/ping", func(c *gin.Context) {
		l := observability.FromContext(c.Request.Context())
		if l == nil {
			t.Fatal("logger not present in request context")
		}
		l.Info("hit")
		c.String(http.StatusOK, "ok")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	router.ServeHTTP(rec, req)

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("logger output not JSON: %q (%v)", buf.String(), err)
	}
	if _, ok := out["trace_id"]; ok {
		t.Errorf("trace_id should be absent when SpanContext is invalid; got %v", out["trace_id"])
	}
	if _, ok := out["span_id"]; ok {
		t.Errorf("span_id should be absent when SpanContext is invalid; got %v", out["span_id"])
	}
}

func TestRequestIDDropsInvalidIncomingHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestID(nil))
	router.GET("/ping", func(c *gin.Context) {
		c.String(http.StatusOK, c.GetString(RequestIDContextKey))
	})

	for _, malicious := range []string{"req\nid", strings.Repeat("a", maxRequestIDLength+1), "req id"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/ping", nil)
		req.Header.Set(RequestIDHeader, malicious)
		router.ServeHTTP(rec, req)

		if rec.Header().Get(RequestIDHeader) == malicious {
			t.Fatalf("X-Request-ID = %q, expected fallback to UUID", malicious)
		}
		if rec.Header().Get(RequestIDHeader) == "" {
			t.Fatal("expected fallback X-Request-ID header to be set")
		}
		if rec.Body.String() == malicious {
			t.Fatalf("body propagated malicious id %q", malicious)
		}
	}
}
