package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newOKHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestBearerAuth_AcceptsMatchingToken(t *testing.T) {
	t.Parallel()

	wrapped := BearerAuth(newOKHandler(), "scrape-secret")
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer scrape-secret")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want ok", rec.Body.String())
	}
}

func TestBearerAuth_RejectsMissingHeader(t *testing.T) {
	t.Parallel()

	wrapped := BearerAuth(newOKHandler(), "scrape-secret")
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("WWW-Authenticate header not set on 401 response")
	}
}

func TestBearerAuth_RejectsWrongScheme(t *testing.T) {
	t.Parallel()

	wrapped := BearerAuth(newOKHandler(), "scrape-secret")
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Basic c2NyYXBlOnNlY3JldA==")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestBearerAuth_RejectsBadToken(t *testing.T) {
	t.Parallel()

	wrapped := BearerAuth(newOKHandler(), "scrape-secret")
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestBearerAuth_TrimsTokenWhitespace pins the strings.TrimSpace
// behaviour so trailing CRLFs from misbehaving clients still match.
func TestBearerAuth_TrimsTokenWhitespace(t *testing.T) {
	t.Parallel()

	wrapped := BearerAuth(newOKHandler(), "scrape-secret")
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer scrape-secret  ")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after whitespace trim", rec.Code)
	}
}

// runBearerGin 起一个仅挂 BearerAuthGin + 一个返回 200 ok 的 handler
// 的最小 gin engine，方便 table-test 调用方在请求级断言 401/200/headers
// 时不每次重复 5 行模板。
//
// 如果 rec.Code == 401，会自动断言 WWW-Authenticate: Bearer realm="admin"
// 头部存在 —— 这是 RFC 7235 对 401 的契约，BearerAuthGin 源码里
// 缺 header 分支和错 token 分支各有一个 c.Header(...) 调用；helper
// 内统一断言能确保任意 401 case 漏掉 challenge header 都会被测试拦下，
// 不只是 RejectsMissingHeader 那一条。
func runBearerGin(t *testing.T, expected string, header string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(BearerAuthGin(expected))
	r.GET("/api/admin/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/ping", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// 任何 401 都必须带 WWW-Authenticate challenge —— 不在调用方
	// 重复，统一在 helper 兜底。
	if rec.Code == http.StatusUnauthorized {
		const want = `Bearer realm="admin"`
		if got := rec.Header().Get("WWW-Authenticate"); got != want {
			t.Errorf("401 response missing WWW-Authenticate header: got %q, want %q (header=%q)", got, want, header)
		}
	}
	return rec
}

// TestBearerAuthGin_AcceptsMatchingToken 锁定 BearerAuthGin 的 happy
// path：token 与 expected 匹配时 c.Next 放行，下游 handler 返回 200。
// 这条路径之前 0% 覆盖，回归会让 /api/admin/* 全部 401。
func TestBearerAuthGin_AcceptsMatchingToken(t *testing.T) {
	t.Parallel()
	rec := runBearerGin(t, "admin-secret", "Bearer admin-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "ok")
	}
}

// TestBearerAuthGin_RejectsMissingHeader 锁定无 Authorization header
// 的 401 路径。WWW-Authenticate realm 由 runBearerGin helper 统一
// 在所有 401 case 上断言（realm="admin"，区别于 BearerAuth 给 metrics
// 用的 realm="metrics"），这里只关心 status。
func TestBearerAuthGin_RejectsMissingHeader(t *testing.T) {
	t.Parallel()
	rec := runBearerGin(t, "admin-secret", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestBearerAuthGin_RejectsWrongScheme 锁定非 "Bearer " 开头的 header
// 走 401，确保 Basic / Token 等其它 scheme 不会蒙混过关。
func TestBearerAuthGin_RejectsWrongScheme(t *testing.T) {
	t.Parallel()
	rec := runBearerGin(t, "admin-secret", "Basic YWRtaW46c2VjcmV0")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestBearerAuthGin_RejectsWrongToken 锁定 token 不匹配走 401（subtle
// 比较）。
func TestBearerAuthGin_RejectsWrongToken(t *testing.T) {
	t.Parallel()
	rec := runBearerGin(t, "admin-secret", "Bearer wrong-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestBearerAuthGin_TrimsTokenWhitespace 锁定 strings.TrimSpace 兜底，
// 让客户端误带尾部空格/CRLF 仍能匹配。
func TestBearerAuthGin_TrimsTokenWhitespace(t *testing.T) {
	t.Parallel()
	rec := runBearerGin(t, "admin-secret", "Bearer admin-secret  ")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d after trim", rec.Code, http.StatusOK)
	}
}

// TestBearerAuthGin_FailClosedWhenExpectedEmpty 锁定关键安全契约：
// expected 为空字符串时 BearerAuthGin 必须拒掉所有请求（含"看似匹配"
// 的 "Bearer "）。这是 admin 接口未配置 token 时的 fail-closed 保险，
// 防止配置遗漏直接把管理面暴露成开放端口。
//
// 实现层面是 `if expectedLen == 0 || subtle...` 的 || 短路求值，
// 三条 subtest 走的是同一条 `expectedLen == 0` 分支（||左侧恒 true
// 直接 short-circuit）。但保留多个 input 不是为了覆盖不同 branch，
// 而是把"无论 header 长什么样，expected 为空都必须 401"这条契约
// 钉死成文档级断言——防止未来"优化"误把空 expected 的判断挪掉。
func TestBearerAuthGin_FailClosedWhenExpectedEmpty(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		header string
	}{
		{"bearer_with_empty_token", "Bearer "},
		{"bearer_with_some_token", "Bearer anything-the-attacker-tries"},
		{"bearer_with_only_whitespace", "Bearer    "},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := runBearerGin(t, "", tc.header)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("expected=\"\" header=%q got status=%d, want 401 (fail-closed)",
					tc.header, rec.Code)
			}
		})
	}
}

// TestBearerAuthGin_ResponseBodyShape 验证 401 响应 body 是 JSON
// 错误对象（含 ErrCodeUnauthorized slug），不是裸 text。前端按 slug
// 分支处理 admin endpoint 错误依赖这个 shape。
func TestBearerAuthGin_ResponseBodyShape(t *testing.T) {
	t.Parallel()
	rec := runBearerGin(t, "admin-secret", "")
	body := rec.Body.String()
	if !strings.Contains(body, ErrCodeUnauthorized) {
		t.Errorf("body %q missing ErrCodeUnauthorized slug", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}
