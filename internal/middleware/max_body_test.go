package middleware

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestMaxRequestBodyTriggersMaxBytesErrorAboveLimit 锁定 Wave 5 M4：
// 请求体超过 maxBytes 时，handler 读取触发 *http.MaxBytesError；
// IsMaxBytesError 帮上层 handler 把它转成 413。
func TestMaxRequestBodyTriggersMaxBytesErrorAboveLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(MaxRequestBody(16))

	var readErr error
	router.POST("/echo", func(c *gin.Context) {
		_, readErr = io.ReadAll(c.Request.Body)
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	body := bytes.Repeat([]byte("x"), 64)
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(body)))

	if readErr == nil {
		t.Fatal("io.ReadAll error = nil, want *http.MaxBytesError")
	}
	if !IsMaxBytesError(readErr) {
		t.Fatalf("IsMaxBytesError(%v) = false, want true", readErr)
	}
	var maxErr *http.MaxBytesError
	if !errors.As(readErr, &maxErr) {
		t.Fatalf("errors.As to *http.MaxBytesError failed; err = %v", readErr)
	}
}

// TestMaxRequestBodyPassesThroughWhenUnderLimit 锁定 happy path：
// body 小于上限时，handler 读到完整内容，无 error。
func TestMaxRequestBodyPassesThroughWhenUnderLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(MaxRequestBody(1 << 20))

	var got []byte
	router.POST("/echo", func(c *gin.Context) {
		b, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Errorf("ReadAll error = %v", err)
		}
		got = b
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader("hello")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if string(got) != "hello" {
		t.Fatalf("body = %q, want %q", got, "hello")
	}
}

// TestMaxRequestBodyNoOpWhenMaxBytesZero 锁定退化路径：maxBytes<=0 时不
// 包装 Body，handler 读完任意大 body。
func TestMaxRequestBodyNoOpWhenMaxBytesZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(MaxRequestBody(0))

	var size int
	router.POST("/echo", func(c *gin.Context) {
		b, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Errorf("ReadAll error = %v", err)
		}
		size = len(b)
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	body := bytes.Repeat([]byte("x"), 4096)
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(body)))

	if size != 4096 {
		t.Fatalf("read size = %d, want 4096 (no-op should leave Body unwrapped)", size)
	}
}

// TestIsMaxBytesErrorReturnsFalseForUnrelatedErrors 边界：nil 与不相干
// error 都返回 false，避免被 handler 错认成 413。
func TestIsMaxBytesErrorReturnsFalseForUnrelatedErrors(t *testing.T) {
	if IsMaxBytesError(nil) {
		t.Error("IsMaxBytesError(nil) = true, want false")
	}
	if IsMaxBytesError(errors.New("boom")) {
		t.Error("IsMaxBytesError(unrelated) = true, want false")
	}
}
