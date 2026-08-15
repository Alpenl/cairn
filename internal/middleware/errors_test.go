package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestRecoveryLogsStackTrace 验证 Recovery 中间件在 panic 发生时把当前
// goroutine 的 stack 写进结构化日志。事故排查时如果只有 panic 字面量
// 没有栈，多半只能盲猜，这条测试保证回归不会悄悄丢掉 stack 字段。
func TestRecoveryLogsStackTrace(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	router := gin.New()
	router.Use(Recovery(logger))
	router.GET("/boom", func(c *gin.Context) {
		panic("deliberate panic for recovery test")
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "http panic recovered") {
		t.Fatalf("log does not contain panic message: %s", logOutput)
	}
	if !strings.Contains(logOutput, "stack=") {
		t.Fatalf("log missing stack= field: %s", logOutput)
	}
	// stack trace 至少要包含 debug.Stack() 的 goroutine header — 这是
	// 进入 runtime/debug 的硬保证，丢了就说明 stack 字段是空串。
	if !strings.Contains(logOutput, "goroutine ") {
		t.Fatalf("log stack field looks empty / non-goroutine: %s", logOutput)
	}
	if !strings.Contains(logOutput, "panic=") {
		t.Fatalf("log missing original panic= field: %s", logOutput)
	}
}

// TestRecoveryStackTruncatedToLimit 锁定 stack 字段被截断到
// recoveryStackLimit 字节这个上限，防止后续把 limit 调到无限大、把
// 日志后端写炸。
func TestRecoveryStackTruncatedToLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	router := gin.New()
	router.Use(Recovery(logger))
	router.GET("/boom", func(c *gin.Context) {
		// 递归触发深栈，让 debug.Stack() 一定会撑超 4KB。
		var deep func(int)
		deep = func(n int) {
			if n == 0 {
				panic("deep panic")
			}
			deep(n - 1)
		}
		deep(200)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	// 检查日志行里 stack=... 字段的长度。slog TextHandler 把含空格的
	// 值用引号包起来，找到第一个 stack=" 后到匹配引号之间的长度即
	// stack 字段的可见字符数；只要它不大于 limit 就说明截断生效。
	logOutput := buf.String()
	idx := strings.Index(logOutput, `stack="`)
	if idx == -1 {
		t.Fatalf("log missing quoted stack field: %s", logOutput)
	}
	rest := logOutput[idx+len(`stack="`):]
	end := strings.Index(rest, `"`)
	if end == -1 {
		t.Fatalf("stack field missing closing quote: %s", logOutput)
	}
	// stack 字段里的反斜杠转义会让可见长度大于原始字节数，所以这里
	// 检查一个宽松上限（limit 的 4 倍），只要量级对得上就说明截断
	// 在工作；若未截断，深栈 200 层至少 30KB+，会显著超出。
	if got := end; got > recoveryStackLimit*4 {
		t.Fatalf("stack field length = %d, expected <= %d (truncation likely broken)", got, recoveryStackLimit*4)
	}
}
