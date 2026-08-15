package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestRequestDeadlinePropagatesDerivedContext 锁定 Wave 5 M1：中间件按
// percent × writeTimeout 派生 ctx deadline；handler 内 c.Request.Context()
// 必须能拿到一个带 deadline 的 ctx，且 deadline 与 percent 一致。
func TestRequestDeadlinePropagatesDerivedContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestDeadline(10*time.Second, 0.9))

	var gotDeadline time.Time
	var hasDeadline bool
	router.GET("/probe", func(c *gin.Context) {
		gotDeadline, hasDeadline = c.Request.Context().Deadline()
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe", nil))

	if !hasDeadline {
		t.Fatal("ctx has no deadline; want middleware-injected deadline")
	}
	// 期望 deadline 在 ~9s 之后（10s × 0.9）；允许 ±500ms 调度漂移。
	remaining := time.Until(gotDeadline)
	expected := 9 * time.Second
	if remaining < expected-500*time.Millisecond || remaining > expected+500*time.Millisecond {
		t.Fatalf("ctx remaining = %v, want around %v", remaining, expected)
	}
}

// TestRequestDeadlineTriggersCtxDoneAfterTimeout 锁定关键行为：当
// deadline 实际到期时 ctx.Done() 触发，handler 能感知超时并放弃。
func TestRequestDeadlineTriggersCtxDoneAfterTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	// 用 30ms 总预算 + 1.0 percent 让 deadline 在测试可观测时间内触发。
	router.Use(RequestDeadline(30*time.Millisecond, 1))

	var ctxErr error
	router.GET("/slow", func(c *gin.Context) {
		select {
		case <-c.Request.Context().Done():
			ctxErr = c.Request.Context().Err()
		case <-time.After(time.Second):
			ctxErr = errors.New("ctx did not cancel within 1s")
		}
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/slow", nil))

	if ctxErr == nil {
		t.Fatal("ctx.Done() never fired; want DeadlineExceeded")
	}
	if !errors.Is(ctxErr, context.DeadlineExceeded) {
		t.Fatalf("ctx error = %v, want context.DeadlineExceeded", ctxErr)
	}
}

// TestRequestDeadlineNoOpWhenWriteTimeoutZero 锁定退化路径：
// writeTimeout=0 / percent=0 时不附加 deadline，避免误传 0 让所有请求
// 立刻 ctx.DeadlineExceeded。
func TestRequestDeadlineNoOpWhenWriteTimeoutZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestDeadline(0, 0.9))

	var hasDeadline bool
	router.GET("/probe", func(c *gin.Context) {
		_, hasDeadline = c.Request.Context().Deadline()
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe", nil))

	if hasDeadline {
		t.Fatal("writeTimeout=0 should be no-op; ctx must not have deadline")
	}
}

// TestRequestDeadlineClampsPercentAbove1 锁定边界：percent > 1 时 clamp
// 到 1，deadline 等于 writeTimeout——永远不应该比 server 的真实 socket
// 写超时更宽松（那样 handler 拿到的 ctx 比 server 真正能等的时间还长，
// 反过来无法保护 server 端 goroutine）。
func TestRequestDeadlineClampsPercentAbove1(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestDeadline(time.Second, 2.0))

	var remaining time.Duration
	router.GET("/probe", func(c *gin.Context) {
		dl, ok := c.Request.Context().Deadline()
		if !ok {
			t.Error("ctx has no deadline")
			return
		}
		remaining = time.Until(dl)
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe", nil))

	if remaining > time.Second+50*time.Millisecond {
		t.Fatalf("remaining = %v, want <= 1s (clamped)", remaining)
	}
}
