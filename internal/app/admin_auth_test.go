package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/handler"
)

// stubAdminConceptMerges 是 handler.ConceptMergeService 的极简实现，
// 测试 admin 鉴权时只需要确认 ListPending 是否被调用即可，不关心返回
// 的业务字段。
type stubAdminConceptMerges struct {
	called bool
}

func (s *stubAdminConceptMerges) ListPending(_ context.Context, _, _ int) ([]handler.ConceptMergeView, error) {
	s.called = true
	return nil, nil
}
func (s *stubAdminConceptMerges) Approve(_ context.Context, _ uuid.UUID, _ string) error { return nil }
func (s *stubAdminConceptMerges) Reject(_ context.Context, _ uuid.UUID, _ string) error  { return nil }

// adminTestDeps 在 smokeDeps 之上挂一个非空的 ConceptMerges 实现 ——
// registerAdminRoutes 对 nil 直接跳过，没有路由就谈不上鉴权测试。
func adminTestDeps() (handler.Dependencies, *stubAdminConceptMerges) {
	stub := &stubAdminConceptMerges{}
	deps := smokeDeps()
	deps.ConceptMerges = stub
	return deps, stub
}

// TestAdminRouteRejectsMissingToken 验证 admin 路由的 fail-closed 行为：
// 非 dev 环境 + 配置了 AdminAuthToken 时，无 Authorization 头一律 401。
// 这是 H2 改造的核心承诺，丢了就等于 /api/admin/* 裸奔。
func TestAdminRouteRejectsMissingToken(t *testing.T) {
	deps, stub := adminTestDeps()
	router := NewRouterWithDependencies(deps, nil, nil, nil, nil, RouterOptions{
		AppEnv:         "prod",
		AdminAuthToken: "right-token",
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/concept-merges", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if stub.called {
		t.Fatal("ListPending should NOT have been called without bearer token")
	}
}

// TestAdminRouteRejectsWrongToken 锁定常量时间比较：错的 token 也得 401。
func TestAdminRouteRejectsWrongToken(t *testing.T) {
	deps, stub := adminTestDeps()
	router := NewRouterWithDependencies(deps, nil, nil, nil, nil, RouterOptions{
		AppEnv:         "prod",
		AdminAuthToken: "right-token",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/concept-merges", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if stub.called {
		t.Fatal("ListPending should NOT have been called with wrong token")
	}
}

// TestAdminRouteAcceptsValidToken 闭环：对的 token 走通到 handler。
func TestAdminRouteAcceptsValidToken(t *testing.T) {
	deps, stub := adminTestDeps()
	router := NewRouterWithDependencies(deps, nil, nil, nil, nil, RouterOptions{
		AppEnv:         "prod",
		AdminAuthToken: "right-token",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/concept-merges", nil)
	req.Header.Set("Authorization", "Bearer right-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !stub.called {
		t.Fatal("ListPending was not invoked despite valid bearer token")
	}
}

// TestAdminRouteSealedWhenTokenMissingInProd 验证 "token 空 + 非 dev"
// 的特殊场景：路由仍然挂载，但 BearerAuthGin("") 永远 401 —— 这是
// "避免破坏现有 prod 部署" 与 "fail-closed" 折中之后的目标行为，运维
// 拿到的是 401 而不是 404，便于排查。
func TestAdminRouteSealedWhenTokenMissingInProd(t *testing.T) {
	deps, stub := adminTestDeps()
	router := NewRouterWithDependencies(deps, nil, nil, nil, nil, RouterOptions{
		AppEnv: "prod",
		// AdminAuthToken 故意留空
	})

	// 即使带了 "看起来对" 的 token，也应当被拒（因为 expected 是空字符串）
	req := httptest.NewRequest(http.MethodGet, "/api/admin/concept-merges", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (admin must seal when token missing in prod)", rec.Code)
	}
	if stub.called {
		t.Fatal("ListPending should never run when admin token is empty in prod")
	}
}

// TestAdminRouteOpenInDevWithoutToken 验证开发态豁免：AppEnv=dev 且
// AdminAuthToken 为空时，admin 路由对外开放，方便本地调试 admin UI。
func TestAdminRouteOpenInDevWithoutToken(t *testing.T) {
	deps, stub := adminTestDeps()
	router := NewRouterWithDependencies(deps, nil, nil, nil, nil, RouterOptions{
		AppEnv: "dev",
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/concept-merges", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (dev env open access)", rec.Code)
	}
	if !stub.called {
		t.Fatal("ListPending should have been invoked under dev open access")
	}
}
