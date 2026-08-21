package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"webtag/internal/representation"
	"webtag/internal/session"
)

func corsRouter(origins []string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CORS(origins))
	handler := func(c *gin.Context) { c.Status(http.StatusNoContent) }
	r.OPTIONS("/api/links", handler)
	r.GET("/api/links", handler)
	return r
}

func preflight(r *gin.Engine, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodOptions, "/api/links", nil)
	req.Header.Set("Origin", origin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// 允许的方法必须覆盖 API 实际用到的全集。
//
// 少一项的症状很难查：跨源部署下某个操作莫名其妙不工作，而同源部署一切正常
// ——预检失败不会进 handler，服务端日志上什么都看不到。
//
// PUT / PATCH 此前就不在列表里：/api/feed-items/{id}/state、
// /api/subscriptions/{id}、/api/links/{id}/content 是 PUT；/api/sites/{id}、
// 跨源改站点、标记已读的接口包含 PATCH。该方法必须出现在 CORS 允许列表中。
// 预检一律失败。
func TestCORSAllowsEveryMethodTheAPIUses(t *testing.T) {
	rec := preflight(corsRouter([]string{"https://reader.example"}), "https://reader.example")
	got := rec.Header().Get("Access-Control-Allow-Methods")

	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		if !strings.Contains(got, m) {
			t.Errorf("Allow-Methods = %q, 缺 %s（该方法在 openapi.json 里有实际端点）", got, m)
		}
	}
}

// 允许的请求头同理，逐项都有真实来源。
func TestCORSAllowsEveryHeaderClientsSend(t *testing.T) {
	rec := preflight(corsRouter([]string{"https://reader.example"}), "https://reader.example")
	got := rec.Header().Get("Access-Control-Allow-Headers")

	for _, h := range []struct{ name, why string }{
		{"Content-Type", "JSON 请求体"},
		{"Authorization", "Bearer 鉴权"},
		{"If-Match", "站点管理的乐观锁"},
		{"Idempotency-Key", "写类请求的重放保护"},
		{"X-Request-ID", "请求追踪"},
		// 刻意写字面量而不是 session.HeaderName：后者会让断言变成「同一个常量
		// 等于它自己」——改掉 session.go 里的头名，这条测试照样绿，而
		// reader/src/lib/api/client.ts 硬编码的是字符串，会静默拆断真实客户端。
		{"X-WebTag-Session", "会话鉴权的 CSRF 头"},
	} {
		if !strings.Contains(got, h.name) {
			t.Errorf("Allow-Headers = %q, 缺 %s（%s）", got, h.name, h.why)
		}
	}
}

// 没有 Allow-Credentials，跨源部署下 httpOnly 会话根本用不起来：
// POST /api/session 的 Set-Cookie 会被浏览器丢弃，前端静默回退到把 api key
// 存进 localStorage——正是会话模式要消灭的那个面。
func TestCORSSendsAllowCredentialsForNamedOrigin(t *testing.T) {
	rec := preflight(corsRouter([]string{"https://reader.example"}), "https://reader.example")

	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("Access-Control-Allow-Credentials = %q, want \"true\"", got)
	}
}

func TestCORSExposesReaderResponseHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/links", nil)
	req.Header.Set("Origin", "https://reader.example")
	rec := httptest.NewRecorder()
	corsRouter([]string{"https://reader.example"}).ServeHTTP(rec, req)

	const clientHeaderLiteral = "X-WebTag-Data-Namespace"
	if representation.DataNamespaceHeader != clientHeaderLiteral {
		t.Fatalf("backend marker header = %q, client literal = %q", representation.DataNamespaceHeader, clientHeaderLiteral)
	}
	if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, clientHeaderLiteral) {
		t.Fatalf("Access-Control-Expose-Headers = %q, missing %s", got, clientHeaderLiteral)
	}
	if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(strings.ToLower(got), "etag") {
		t.Fatalf("Access-Control-Expose-Headers = %q, missing ETag", got)
	}
}

func TestCORSPreflightHeaderTokensAreCaseInsensitive(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/api/links", nil)
	req.Header.Set("Origin", "https://reader.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	req.Header.Set("Access-Control-Request-Headers", "if-match, x-webtag-session")
	rec := httptest.NewRecorder()
	corsRouter([]string{"https://reader.example"}).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	allowed := strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers"))
	for _, token := range []string{"if-match", "x-webtag-session"} {
		if !strings.Contains(allowed, token) {
			t.Fatalf("Access-Control-Allow-Headers = %q, missing %s", allowed, token)
		}
	}
}

// CORS 规范禁止 `*` 与 Allow-Credentials 并存——浏览器会拒绝整个响应。
// 通配符分支下必须不发这个头，否则 CORS_ORIGINS=* 的部署会比不发更糟：
// 所有跨源请求直接失败，而不是退化成不带凭证。
func TestCORSOmitsAllowCredentialsForWildcard(t *testing.T) {
	rec := preflight(corsRouter([]string{"*"}), "https://anywhere.example")

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin = %q, want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("通配符来源不得携带 Allow-Credentials（浏览器会拒绝整个响应），实际 = %q", got)
	}
}

// 未授权来源仍然什么都不给。
func TestCORSGivesNothingToUnknownOrigin(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/links", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	corsRouter([]string{"https://reader.example"}).ServeHTTP(rec, req)

	for _, h := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Credentials",
	} {
		if got := rec.Header().Get(h); got != "" {
			t.Errorf("未授权来源拿到了 %s = %q", h, got)
		}
	}
}

// 前端硬编码的头名与后端常量必须一致。跨语言的这条缝没有编译器守，
// 只能靠一条断言把两边钉在同一个字面量上。
func TestSessionHeaderNameMatchesFrontendLiteral(t *testing.T) {
	const frontendLiteral = "X-WebTag-Session" // reader/src/lib/api/client.ts SESSION_HEADER

	if session.HeaderName != frontendLiteral {
		t.Fatalf("session.HeaderName = %q，而 Reader 客户端硬编码的是 %q——"+
			"改名必须两边一起改，否则会话鉴权静默失效（后端忽略 cookie → 401）",
			session.HeaderName, frontendLiteral)
	}
}

// Vary: Origin 挡的是缓存投毒：没有它，前置 CDN 可能把某个来源拿到的
// Access-Control-Allow-Origin 响应，复用给另一个本不被允许的来源。
// cors.go 为它写了 5 行注释，却没有任何测试守——删掉全绿。
func TestCORSAlwaysVariesOnOrigin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		origins []string
		origin  string
	}{
		{"具名来源", []string{"https://reader.example"}, "https://reader.example"},
		{"通配来源", []string{"*"}, "https://anywhere.example"},
		{"未授权来源", []string{"https://reader.example"}, "https://evil.example"},
		{"无 Origin 头（同源）", []string{"https://reader.example"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/links", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			corsRouter(tc.origins).ServeHTTP(rec, req)

			if got := rec.Header().Get("Vary"); got != "Origin" {
				t.Fatalf("Vary = %q, want \"Origin\"（缺失会让共享缓存把一个来源的 CORS 响应喂给另一个）", got)
			}
		})
	}
}
