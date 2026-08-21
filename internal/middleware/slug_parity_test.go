package middleware_test

import (
	"testing"

	"webtag/internal/httperr"
	"webtag/internal/middleware"
)

// TestMiddlewareSlugsMatchHTTPErr 把「两份 slug 表必须保持一致」从注释里的口头
// 约定变成会失败的断言。
//
// middleware 与 httperr 各自维护一份错误 slug 常量表，两边的注释都写着「字面值
// 必须保持一致，避免前端按 slug 分支时产生歧义」——但此前没有任何机制保证它。
// 改了一边忘了另一边，前端的分支就会静默失配：请求确实失败了，只是错误码对不上，
// UI 落到兜底分支显示一句通用报错。
//
// middleware 已经依赖 httperr（go list 可证），因此重复定义不是为了避免成环。
// 保留两份是既有事实，本测试不改变它，只保证它们同步。新增共有 slug 时在下表
// 补一行即可。
func TestMiddlewareSlugsMatchHTTPErr(t *testing.T) {
	t.Parallel()

	for _, pair := range []struct {
		name       string
		middleware string
		httperr    string
	}{
		{"LinkNotFound", middleware.ErrCodeLinkNotFound, httperr.CodeLinkNotFound},
		{"CooldownActive", middleware.ErrCodeCooldownActive, httperr.CodeCooldownActive},
		{"InvalidCursor", middleware.ErrCodeInvalidCursor, httperr.CodeInvalidCursor},
	} {
		if pair.middleware != pair.httperr {
			t.Errorf("%s 的两份定义已漂移: middleware=%q httperr=%q——前端按 slug 分支会失配",
				pair.name, pair.middleware, pair.httperr)
		}
	}
}
