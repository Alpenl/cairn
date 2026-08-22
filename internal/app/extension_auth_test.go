package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"webtag/internal/representation"
)

// extensionTestPaths lists representative API routes protected by the
// installation credential or browser session.
var extensionTestPaths = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/tree"},
	{http.MethodGet, "/api/links"},
	{http.MethodGet, "/api/tags"},
	{http.MethodPost, "/api/ingest"},
}

// TestExtensionAPIRejectsMissingToken verifies that API routes reject a
// request that has neither an installation token nor a browser session.
func TestExtensionAPIRejectsMissingToken(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, RouterOptions{
		ExtensionAPIToken:    "ext-token",
		InstallationIdentity: sessionSecurityVersions{},
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
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, RouterOptions{
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
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, RouterOptions{
		ExtensionAPIToken:    "ext-token",
		InstallationIdentity: sessionSecurityVersions{},
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

func TestPublicAPIWithEmptyTokenRemainsClosed(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, RouterOptions{
		InstallationIdentity: sessionSecurityVersions{},
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
