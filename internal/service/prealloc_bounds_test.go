package service

import (
	"testing"

	"webtag/internal/alloc"
)

// TestServicePageLimitsFitPreallocBound 守住**下面这张表里列出的**几个 service
// 层上界 ≤ alloc.MaxPrealloc。
//
// ⚠ 它守不住的东西比守住的多，必须说清楚，否则又成了「注释声称的比测试做到的
// 多」：
//
//   - 它是一张手写清单，**无法发现自己漏了什么**。全仓 alloc.Hint 有 11 个
//     调用点，这张表真正管住的只有 feed_items.go 那一个。
//   - library search 的 site 腿真正决定 cap 的是 site_read_repository.go 里
//     **又内联 clamp 一次**的 `> 50`；reading 腿的 ListDone 根本不按 limit
//     预分配。所以 maxLibrarySearchLimit 列在这里更多是「顺手钉住这个常量」，
//     不代表它保护了某个分配点。
//   - service 里还有不喂任何分配点的上界（如 treeListVisibleSoftCap = 5000），
//     它们**不在**本测试范围内——早先的注释把范围写成「service 层的分页/搜索
//     上界」，那句话当时就已经是假的（5000 > 1024 而测试全绿）。
//
// 真正覆盖全部调用点的是 internal/alloc 的 TestHintCallSitesAreRegistered：
// AST 扫出每一个 alloc.Hint 调用点并要求逐个登记，漏登即失败。
//
// 为什么这条测试在 service 包而不是 alloc 包：
//
// 它必须读 service 的**真常量**才有意义。初版把这条断言写在 alloc 包里，用
// 字面量复刻了 50 / 100 两个值——那等于把待测事实抄了一份，两边各自漂移谁也
// 不知道。实测把 maxFeedPageLimit 从 100 改成 100000，那个版本照样绿，而它的
// 注释还写着「改动它们时本测试会提醒同步复核」。
//
// alloc 是零依赖叶子包，不能反向 import service；断言只能落在这一侧。
//
// 初版还留了一条 `const librarySearchInlineCap = 50`，因为当时 library_search.go
// 用的是内联 `> 50`。那条同样是抄件——实测把内联值改成 100000，断言照样绿。
// 已把它提成 maxLibrarySearchLimit 并在下面读本体。
//
// 上界一旦超过 alloc.MaxPrealloc，夹取就会在**正常请求**上生效，把「一次分配
// 到位」退化成反复扩容——功能不受影响（append 会扩容），但预分配的意义没了，
// 属于该被察觉的静默劣化。
func TestServicePageLimitsFitPreallocBound(t *testing.T) {
	t.Parallel()

	limits := map[string]int{
		"maxFeedPageLimit":          maxFeedPageLimit,
		"defaultFeedPageLimit":      defaultFeedPageLimit,
		"maxLibrarySearchLimit":     maxLibrarySearchLimit,
		"defaultLibrarySearchLimit": defaultLibrarySearchLimit,
	}
	for name, v := range limits {
		if v > alloc.MaxPrealloc {
			t.Errorf("%s = %d 超过 alloc.MaxPrealloc = %d——"+
				"预分配会在正常请求上被夹住，退化成反复扩容。"+
				"要么调低该上界，要么连带复核 alloc.MaxPrealloc。",
				name, v, alloc.MaxPrealloc)
		}
	}
}
