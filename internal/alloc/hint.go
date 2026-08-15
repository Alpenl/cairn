package alloc

// MaxPrealloc 是任何「按请求参数预分配切片容量」的上限。
//
// 值取 1024。要说清它保证什么、不保证什么：
//
//   - **保证**：任何调用点都不会因为一个巨大的请求参数而分配任意大的内存。
//     这是无条件的——夹取在函数内部，与调用方是否 clamp 无关。
//   - **不保证**：正常请求不被夹到。那取决于每个调用点上游的上界是否 ≤ 1024，
//     而那是逐调用点的事实，由 TestHintCallSitesAreRegistered 的登记表管。
//
// 刻意不在这里写「本仓所有上界都远低于 1024」之类的话——那既是抄件（正本一改
// 就成假话），而且**当时就已经是假的**：service 里存在 5000 的上界
// （treeListVisibleSoftCap），只是它不喂任何 Hint 调用点。
//
// 超过上限也不会出错，只是退化成 append 扩容。
//
// 「service 的上界必须 ≤ 这个值」这条不变量由 internal/service 包内的
// TestServicePageLimitsFitPreallocBound 守着——它读的是 service 的**真常量**，
// 改动任一侧都会变红。放在那边而不是这边，是因为 repository 不能反向 import
// service；早先在 repository 里用字面量复刻那些上界的写法是假的提醒
// （实测把 maxFeedPageLimit 调到 100000，那个测试照样绿）。
//
// 单独成包而不是放在 repository 里：internal/worker 也有同形的分配点，而
// worker -> repository 是跳层依赖，会被 internal/arch 的门禁拦下（实测拦到了）。
// 为了夹一个 int 就开一条架构例外不划算，所以抽成零依赖的叶子包，任何层都能
// import。
const MaxPrealloc = 1024

// Hint 把一个来自请求的数量夹到安全区间，用作 make 的 cap 参数。
//
// 为什么需要它：CodeQL 的 go/uncontrolled-allocation-size 在三处报了 error
// （feed_items.go 与 site_read_repository.go 的两处），因为 `make([]T, 0, n)`
// 的 n 一路来自请求参数。这几处的上界**确实存在**，但写在 service 层
// （FeedService 与 LibrarySearchService 各自的具名上界常量），
// 跨过 repository 边界后静态分析看不到，人也一样看不到。
//
// 前几轮反复出现的教训是「判定与门必须同层」：边界在 service 决定、分配在
// repository 发生，两者迟早分叉——而且 repository 的方法本来就可以被别的调用方
// 直接调用，那道 clamp 只存在于其中一个调用方。
//
// cap 只是性能提示，夹住它不改变任何行为：真实数据多于 cap 时 append 自己会
// 扩容。所以这是零代价的就地加固，不是为了让扫描器闭嘴。
func Hint(n int) int {
	if n < 0 {
		return 0
	}
	if n > MaxPrealloc {
		return MaxPrealloc
	}
	return n
}
