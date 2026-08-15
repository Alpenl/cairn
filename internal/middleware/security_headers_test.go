package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestSecurityHeadersSetsAllFourBaselineHeaders 锁定 SKILL 文档承诺的
// 四个安全头都会被注入到每个响应上，并防止后续重构时悄悄漏掉一项。
func TestSecurityHeadersSetsAllFourBaselineHeaders(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(SecurityHeaders())
	router.GET("/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	cases := map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"X-Frame-Options":            "DENY",
		"Referrer-Policy":            "no-referrer",
		"Cross-Origin-Opener-Policy": "same-origin",
	}
	for header, want := range cases {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// TestSecurityHeadersDoesNotMutateBody 验证中间件只动响应头、不改响应
// 体；如果未来误把 c.Writer 包裹成会改写 body 的形态，这里立刻报错。
func TestSecurityHeadersDoesNotMutateBody(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	const payload = `{"status":"ok","nested":{"a":1}}`
	router := gin.New()
	router.Use(SecurityHeaders())
	router.GET("/payload", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", []byte(payload))
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/payload", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body = %q, want %q", rec.Body.String(), payload)
	}
}

// TestSecurityHeadersPreservesUpstreamOverride 锁定 "已存在的响应头不
// 会被覆盖" 这个契约：某个上游中间件（比如未来某条针对 /admin 的专用
// CSP）先把 X-Frame-Options 设成 SAMEORIGIN 时，SecurityHeaders 不应当
// 再改回 DENY。这保留了"个别页面有特殊需求"的覆盖入口。
func TestSecurityHeadersPreservesUpstreamOverride(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	// 上游中间件：在 SecurityHeaders 跑之前显式设置 X-Frame-Options。
	router.Use(func(c *gin.Context) {
		c.Header("X-Frame-Options", "SAMEORIGIN")
		c.Next()
	}, SecurityHeaders())
	router.GET("/embed", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/embed", nil))

	if got := rec.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Fatalf("X-Frame-Options = %q, want SAMEORIGIN (override should be preserved)", got)
	}
	// 其他三项仍应被 SecurityHeaders 兜底设置
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
	if got := rec.Header().Get("Cross-Origin-Opener-Policy"); got != "same-origin" {
		t.Fatalf("Cross-Origin-Opener-Policy = %q, want same-origin", got)
	}
}
