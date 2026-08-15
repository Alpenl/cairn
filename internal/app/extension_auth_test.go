package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"webtag/internal/representation"
)

// extensionTestPaths 列出 handler.RegisterRoutes 暴露、由 Task 1B 的
// opt-in 鉴权守门的代表性公开 API 路径。覆盖 spec 明确点名的
// /api/tree、/api/links、/api/tags、/api/ingest、/api/jobs/{id}，外加
// /api/v1/* 别名各取一例，确保别名前缀同样受门。
var extensionTestPaths = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/tree"},
	{http.MethodGet, "/api/links"},
	{http.MethodGet, "/api/tags"},
	{http.MethodPost, "/api/ingest"},
	{http.MethodGet, "/api/jobs/some-job-id"},
	{http.MethodGet, "/api/export"},
	{http.MethodGet, "/api/v1/tree"},
	{http.MethodGet, "/api/v1/links"},
	{http.MethodGet, "/api/v1/export"},
}

// TestExtensionAPIRejectsMissingToken 验证 opt-in 鉴权开启后
// （ExtensionAPIToken 非空）公开 API 在缺少 Authorization 头时一律 401。
func TestExtensionAPIRejectsMissingToken(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv:                  "prod",
		ExtensionAPIToken:       "ext-token",
		ConditionalGetRevisions: sessionSecurityVersions{},
	})

	for _, tc := range extensionTestPaths {
		tc := tc
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (no bearer token); body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestExtensionAPIRejectsWrongToken 锁定常量时间比较：错误 token 也 401。
func TestExtensionAPIRejectsWrongToken(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv:            "prod",
		ExtensionAPIToken: "ext-token",
	})

	for _, tc := range extensionTestPaths {
		tc := tc
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", "Bearer wrong-token")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (wrong token); body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestExtensionAPIAcceptsValidToken 闭环：正确 token 走通到 handler。
// smoke* stub handler 对个别零值依赖可能返回业务 4xx；鉴权成功的共同证据是
// 不出现 5xx 且响应带权威 namespace marker。
func TestExtensionAPIAcceptsValidToken(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv:                  "prod",
		ExtensionAPIToken:       "ext-token",
		ConditionalGetRevisions: sessionSecurityVersions{},
	})

	for _, tc := range extensionTestPaths {
		tc := tc
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", "Bearer ext-token")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code >= http.StatusInternalServerError {
				t.Fatalf("status = %d with valid token, want handler response; body=%q", rec.Code, rec.Body.String())
			}
			if marker := rec.Header().Get(representation.DataNamespaceHeader); marker == "" {
				t.Fatal("valid authenticated response omitted data namespace marker")
			}
		})
	}
}

// TestPublicAPIOpenRequiresExplicitFlag locks the installation-safe default:
// an empty token does not silently expose the library. Anonymous access is
// enabled only through PUBLIC_API_OPEN=true.
func TestPublicAPIOpenRequiresExplicitFlag(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv:                  "prod",
		ConditionalGetRevisions: sessionSecurityVersions{},
	})

	for _, tc := range extensionTestPaths {
		tc := tc
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d with empty token and closed default, want 401; body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPublicAPIExplicitOpenAccess(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv:                  "prod",
		AllowOpenAccess:         true,
		ConditionalGetRevisions: sessionSecurityVersions{},
	})

	for _, tc := range extensionTestPaths {
		tc := tc
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code == http.StatusUnauthorized || rec.Code >= http.StatusInternalServerError {
				t.Fatalf("status = %d in explicitly open mode, want handler response; body=%q", rec.Code, rec.Body.String())
			}
			if marker := rec.Header().Get(representation.DataNamespaceHeader); marker == "" {
				t.Fatal("open installation response omitted data namespace marker")
			}
		})
	}
}
