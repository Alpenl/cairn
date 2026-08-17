package alloc_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// hintCallSites 是 alloc.Hint 全部调用点的登记表，key 为 "文件:行号"。
//
// **表里只有 note，没有数字上界。** 上一版有个 `bound int` 字段，看起来像断言，
// 实际是抄件——门禁从不核对它与源码里真实的 clamp 是否一致。实测把
// site_read_repository.go 的两处 `limit > 50` 改成 `> 5000`，登记值仍写着 50，
// 测试照绿。那正是本串反复栽的同一跤（`librarySearchInlineCap = 50` 死断言的
// 第三次搬家），所以直接删掉，不留「长得像断言的装饰品」。
//
// 于是这张表守的只有一件事，但这件事是实的：**新增调用点不能悄悄溜进来**。
// 漏登即失败，表里留着已消失的条目也失败。至于每个上界本身对不对，靠 note 供
// 人复查——这是登记表的固有边界，写在这里而不是假装没有。
//
// （「上界 ≤ MaxPrealloc」这条关系目前只有 feed_items.go 一处被机器核对，在
// internal/service 的 TestServicePageLimitsFitPreallocBound 里。其余靠 Hint
// 自身兜底：安全性无条件成立，被夹到只会退化成 append 扩容。）
var hintCallSites = map[string]string{
	"internal/repository/feed_items.go:42":                   "service/feed.go 的 maxFeedPageLimit=100",
	"internal/repository/site_read_repository.go:223":        "本文件 SearchSites 内联 clamp `limit > 50 → 50`",
	"internal/repository/site_read_repository.go:306":        "本文件 SearchSitesSemantic 内联 clamp `limit > 50 → 50`",
	"internal/repository/historical_migration.go:166":        "本文件内联默认 1000 + service/historical_migration.go 同值",
	"internal/repository/feed_refresh.go:91":                 "无上界：ClaimDue 的 limit 由 worker 的 BatchSize 决定",
	"internal/repository/link_repo_backfill.go:91":           "无上界：回填批大小由调用方决定",
	"internal/repository/concept_repo_backfill.go:57":        "无上界：同 backfill",
	"internal/repository/concept_repo_core.go:234":           "无上界：ListNearestConcepts 的 topK",
	"internal/repository/concept_repo_core.go:327":           "上界 neighbourConceptLimit=15（service/pipeline_retrieve.go 常量，唯一调用方）",
	"internal/repository/site_embedding_repository.go:39":    "无上界：同 backfill",
	"internal/repository/reader_host_lifecycle.go:474":       "同一分配点调用 normalizeReaderTrashLimit，夹到 readerTrashMaxLimit=200（非法值回退 50）",
	"internal/repository/reader_vnext.go:1544":               "本函数内联 clamp `limit > 200 → 50`（ListThoughts）",
	"internal/repository/reader_vnext.go:1629":               "本函数内联 clamp `limit > 100 → 20`（SearchThoughts）",
	"internal/repository/reader_vnext.go:1850":               "本函数内联 clamp `limit > 200 → 100`（ListThoughtsSince）",
	"internal/repository/reader_vnext.go:1897":               "本函数内联 clamp `limit > 200 → 100`（ListThoughtConflicts）",
	"internal/repository/reader_vnext.go:1946":               "本函数内联 clamp `limit > 100 → 30`（ListThoughtHistory）",
	"internal/repository/reader_vnext.go:2475":               "本函数内联 clamp `limit > 100 → 30`（ListNotes）",
	"internal/repository/reader_vnext.go:2518":               "本函数内联 clamp `limit > 100 → 20`（SearchPublishedNotes）",
	"internal/repository/reader_vnext.go:3241":               "本函数内联 clamp `limit > 100 → 50`（ListNoteHistory）",
	"internal/repository/reader_vnext.go:3396":               "本函数内联 clamp `limit > 100 → 30`（ListInbox）",
	"internal/repository/reader_vnext.go:3458":               "本函数内联 clamp `limit > 500 → 100`（ClaimExpiredInbox，由 worker 的 batch size 驱动）",
	"internal/repository/reader_vnext.go:5054":               "本函数内联 clamp `limit > 10 → 3`（ListContinueReading；当前无调用方，接线前先看这条）",
	"internal/repository/reader_vnext.go:6526":               "无本函数 clamp：limit 由唯一调用方 RelatedTags 夹到 `> 50 → 12`（semanticRelatedTags）",
	"internal/repository/reader_vnext.go:6575":               "无本函数 clamp：limit 由唯一调用方 RelatedTags 夹到 `> 50 → 12`（cooccurrenceRelatedTags）",
	"internal/repository/reader_vnext.go:6732":               "本函数内联 clamp `query.Limit > 1000 → 100`（ListActivity 的 limit+1 页）",
	"internal/repository/reader_vnext.go:6886":               "本函数内联 clamp `limit > 100 → 50`（ListContentHistory）",
	"internal/worker/parse_terminal_reconciler.go:143":       "无上界：exported constructor 可直接接收 BatchSize；production config 另行限制为 [1,1000]（terminal mismatch page）",
	"internal/worker/parse_terminal_reconciler.go:189":       "无上界：exported constructor 可直接接收 BatchSize；production config 另行限制为 [1,1000]（missing attempt page）",
	"internal/worker/site_payload_cleaner.go:83":             "无上界：同上",
	"internal/worker/translation_terminal_reconciler.go:273": "无上界：exported constructor 可直接接收 BatchSize；production config 另行限制为 [1,1000]（terminal page）",
	"internal/worker/translation_terminal_reconciler.go:315": "无上界：missing-attempt keyset page 与 terminal page 共用 BatchSize；alloc.Hint 自身负责安全 clamp",
}

// TestHintCallSitesAreRegistered 扫出全部 alloc.Hint 调用点，要求逐个登记。
//
// 扫描面覆盖整个仓库（不止 internal/）——alloc 是零依赖叶子包，cmd/ 与
// test/ 同样能 import 它。上一版把根定在 internal/，实测在 cmd/ 里加一个
// alloc.Hint 调用点会静默通过。
//
// **解析 import 而不是硬认包名 `alloc`**：上一版只匹配 `sel.X` 是 Ident
// "alloc" 的形式，实测三种绕过——`al "webtag/internal/alloc"` 漏扫、
// dot-import 漏扫、局部变量名叫 alloc 的自定义 Hint 方法误红。最糟的是
// 已登记文件改成别名时，门禁会红着说「这里已经没有调用点了，请删掉登记」
// ——照做就把真实存在的调用点永久隐形了。
func TestHintCallSitesAreRegistered(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	found := map[string]bool{}
	topDirs := map[string]int{}
	fset := token.NewFileSet()

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			switch {
			case name == "vendor", name == "node_modules", name == "testdata",
				strings.HasPrefix(name, "_"), strings.HasPrefix(name, "."):
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		if rel, relErr := filepath.Rel(root, path); relErr == nil {
			topDirs[strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]]++
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		// 先看这个文件到底有没有 import alloc，以及绑定成了什么名字。
		locals := allocLocalNames(file)
		if len(locals) == 0 {
			return nil
		}
		full, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		walkHintRefs(full, locals, func(pos token.Pos) {
			found[fmt.Sprintf("%s:%d", rel, fset.Position(pos).Line)] = true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}

	// ⚠ 作用域：`_test.go` **不在扫描范围内**（见上面 walk 里的后缀判断）。
	// 也就是说测试文件里新增的 alloc.Hint 调用点永远不需要登记，门禁绿——
	// 实测确认。这是有意的（登记表守的是生产路径的分配点），但此前一行注释
	// 都没有，等于一个没人知道的静默绿。写在这里，不是假装它不存在。
	//
	// 扫描面自证。**不用「扫到 N 个文件」这种阈值**——那个 N 是从当前文件数
	// 反推的抄件，仓库长大后它只会越来越松，而且没有任何机制提醒。
	//
	// 改为断言三个顶层目录各自都被扫到。据实说清它是什么：**今天仓库布局的
	// 一份快照**，仍然是手维护的清单——但破坏方向从「静默变松」变成了「响亮
	// 变红」，这是实质区别。
	//
	// 它不覆盖新增的顶层目录，那由下面的主检查兜底（实测：在新建的 pkg/ 下放
	// 一个 alloc.Hint 调用点会被报未登记）。
	//
	// 注意 test/ 目前只由 test/dbintegration/postgres.go 一个非测试文件撑着
	// （其余都是被跳过的 _test.go），它一旦移走这条会红，而诊断信息会说
	// 「扫描根有问题」——那时请先确认是不是这个。
	//
	// （真正拦「含登记条目的目录被漏扫」的是下面那条反向核对——那些条目会
	// 立刻报「已经没有调用点了」。本条守的是**尚无登记条目**的目录，比如
	// 今天的 cmd/，那正是反向核对够不着的地方。）
	for _, dir := range []string{"internal", "cmd", "test"} {
		if topDirs[dir] == 0 {
			t.Fatalf("顶层目录 %s/ 一个 .go 文件都没扫到——扫描根或跳过规则有问题，"+
				"「没有未登记调用点」这个结论不可信（各目录计数：%v）", dir, topDirs)
		}
	}
	if len(found) == 0 {
		t.Fatal("一个 alloc.Hint 调用点都没扫到——扫描逻辑失效，而不是真的没有")
	}

	for site := range found {
		if _, ok := hintCallSites[site]; !ok {
			t.Errorf("%s 有未登记的 alloc.Hint 调用点。"+
				"请在 hintCallSites 登记并写明上游上界在哪；"+
				"确实没有上界就写「无上界：<原因>」——那本身是个应当被看见的事实。", site)
		}
	}
	for site := range hintCallSites {
		if !found[site] {
			t.Errorf("%s 登记在 hintCallSites 里，但那里已经没有 alloc.Hint 调用点了。"+
				"三种可能，按概率排：(a) **同一文件另有「未登记」报错 → 多半只是"+
				"上方增删行导致行号漂移，把登记里的数字改成新行号即可**；"+
				"(b) 调用点确实被删了 → 删登记；"+
				"(c) 换了写法而扫描没认出来 → 修扫描，别删登记。", site)
		}
	}
}

// allocLocalNames 返回本文件里 webtag/internal/alloc 的**全部**绑定名。
// 空切片表示本文件没有以可调用的形式 import 它。
//
// 必须收集全部、不能首个匹配即返回。同一路径导入多次是合法 Go，任何形式的
// 提前 return 都会让正确性取决于**书写顺序**：
//
//	import (
//		a1 "webtag/internal/alloc"   // 先命中这条就返回
//		a2 "webtag/internal/alloc"   // a2.Hint(n) 静默漏扫
//	)
//
// 上一版只把 `_` 改成 continue、遇到具名仍然 return，于是「_ + 具名」修好了、
// 「具名 + 具名」照漏——而注释写的是「必须遍历完再判定」。实测两行对调门禁
// 就红，也就是说那句话被两行代码之外的输入直接证伪。
func allocLocalNames(file *ast.File) []string {
	const target = `"webtag/internal/alloc"`
	var names []string
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != target {
			continue
		}
		switch {
		case imp.Name == nil:
			names = append(names, "alloc") // 默认包名
		case imp.Name.Name == "_":
			// 只为副作用引入，不会产生调用；但同路径可能还有别的绑定。
		default:
			names = append(names, imp.Name.Name) // 具名别名，或 "." 的 dot-import
		}
	}
	return names
}

// walkHintRefs 遍历 file，对每一处引用了 alloc.Hint 的位置回调一次。
//
// 匹配的是**引用**而非调用：`h := alloc.Hint; h(n)` 这种方法值形式同样算数
// （只匹配 CallExpr 时它会静默漏过，实测）。
//
// 剪枝**无条件生效**（不只在 dot-import 下）。它存在的动机来自 dot-import：
// 那时匹配的是裸 Ident，「任何叫 Hint 的标识符」都会命中——实测会把结构体
// 字段声明 `Hint string`、复合字面量的字段名 key `{Hint: "x"}`、别的类型的
// `c.Hint` 误报。规则：
//
//	SelectorExpr   只递归 X，不递归 Sel
//	Field          跳过 Names
//	CompositeLit   只在内联匿名结构体 `struct{…}{…}` 下跳过裸 Ident 的 Key
//	               （见下面那一支的说明）
//
// 前两条零损失（Sel 与 Names 恒为裸 Ident，且在别的作用域解析，永不指向
// dot-import 的 Hint）；第三条的取舍在那一支里写明了。
//
// 曾经还有一条独立的 `*ast.KeyValueExpr` 分支。CompositeLit 那一支自己消费
// Elts 之后它已不可达，已删除。
//
// 删它时踩了一脚，值得记：判定「不可达」的依据是「往里插 panic，跑全仓一次都
// 没触发」——那只证明了**在本仓当前内容上**没被触达，不是一般不可达。真正的
// 依据应该是语法：KeyValueExpr 在 Go 里只能作为 CompositeLit.Elts 的元素出现，
// 而那一支自己消费 Elts 并 return，不把它交给 walk。
//
// 而且删除时的锚点取到了下一个 case，**连带删掉了 `*ast.Field` 分支**，
// 于是内联匿名结构体的字段名开始误报。是删完立刻跑的形状复验当场抓到的。
//
// ── 残留的误报（红，安全）──
// dot-import 下这些仍会命中，**至少**五种（下面五种均自己实测复现，不是穷举
// ——审查另补了局部 `const Hint = 1` 与 `goto Hint` 两种）：标签 `Hint:`、
// 局部遮蔽 `Hint := 1` / `var Hint int`、方法名 `func (T) Hint()`、局部类型名
// `type Hint struct{}`。另外省略类型的内层字面量 `[]S{{Hint: "x"}}` 也会命中
// ——那是上面 CompositeLit 那一支刻意选的红。
//
// 没有继续收窄，是因为失败方向是响亮的红、且全仓无一处 alloc 的 dot-import。
//
// ── 残留的漏扫（绿，致命）──
//
// 就本剪枝而言为空，依据是**把 CompositeLit.Type 的可能形状枚举完**而不是
// 「想不出反例」：合法的复合字面量类型只有 `*ast.Ident`（具名）、
// `*ast.SelectorExpr`（跨包）、`*ast.IndexExpr` / `*ast.IndexListExpr`（泛型
// 实例化）、`*ast.ArrayType`、`*ast.MapType`、`*ast.StructType`、以及 nil
// （省略类型的内层）。现在只有 `*ast.StructType` 跳过 Key，而内联匿名结构体
// 的 Key 按语言定义必是字段名——不存在「本该走却被跳过」的形状。
//
// 这句话之前写错过**两次**，且两次都在同一行、同一个方向（绿）：先写
// 「只剩一种，且不可达」，被一个从未调用的函数体证伪；再写「目前为零」，
// 被具名 map 类型证伪。两次的共同点是**用「想不出反例」代替「枚举完」**。
//
// 下面保留第一次的那条记录，因为它示范了这类断言怎么错：
// `map[any]string{Hint: "x"}` 这类接口键 map，当时的剪枝按「Key 是不是裸
// Ident」判，把它整片跳过。理由写的是「能编译，但包 init 阶段必
// `panic: hash of unhashable type`，不可能存在于任何跑得起来的程序里」。
//
// 前半句只对**包级**字面量成立。同一个构造写进一个**从未被调用的函数体**：
//
//	func never() { m := map[any]string{Hint: "x"}; _ = m }
//
// 实测编译通过、程序跑到底、退出码 0、门禁绿——一次 `go run` 就证伪了。
//
// 现在改为看 `CompositeLit.Type`（那个信息本来就在 AST 里），不再靠猜。
//
// 记这一笔是因为它踩的正是上一条纪律：**误报是红（吵但安全），漏扫是绿（安静
// 且致命），两个方向不对称，不能用同一个力度换。** 而我当时用一个没跑过的
// 「不可达」给绿的那一侧背书。
func walkHintRefs(file *ast.File, locals []string, hit func(token.Pos)) {
	matchesLocal := func(name string) bool {
		for _, l := range locals {
			if l == name {
				return true
			}
		}
		return false
	}
	dotImported := matchesLocal(".")

	var walk func(ast.Node)
	walk = func(n ast.Node) {
		if n == nil {
			return
		}
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if node.Sel != nil && node.Sel.Name == "Hint" {
				if pkg, ok := node.X.(*ast.Ident); ok && matchesLocal(pkg.Name) {
					hit(node.Pos())
				}
			}
			walk(node.X) // 不递归 Sel：它是字段/方法名，不是独立标识符
			return
		case *ast.CompositeLit:
			// 复合字面量的 Key 是「字段名」还是「值引用」，取决于字面量自身的
			// 类型。**只在语法上能确定是字段名的那一种跳过，其余一律走。**
			//
			// 极性必须是这个方向，不能反过来枚举「哪些是 map」。上一版按
			//	case *ast.MapType, *ast.ArrayType, nil:  → 走
			//	default:                                 → 跳
			// 写，于是凡是 map 类型**不以字面形式拼写**的全部掉进 default：
			//
			//	type NamedMap map[any]string   → Type 是 *ast.Ident
			//	type AliasMap = map[any]string → *ast.Ident
			//	pkgm.M{…}                      → *ast.SelectorExpr
			//	GenMap[string]{…}              → *ast.IndexExpr
			//	Gen2[any,string]{…}            → *ast.IndexListExpr
			//
			// 五种都是合法 Go，实测三种在真仓库上编译通过、程序跑到底、
			// 退出码 0、**门禁绿**。而具名 map 类型在真实代码里比内联
			// `map[any]string` 更常见，不是更罕见。
			//
			// **`switch` 的 default 分支是漏扫的默认藏身处**：只要它落在
			// 「跳过」一侧，任何没枚举到的形状都会静默绿。所以把 default 放在
			// 「走」一侧——没枚举到的形状最坏是误报（红，吵但安全）。
			//
			// 唯一能语法确定 Key 必是字段名的，只有内联匿名结构体
			// `struct{…}{Field: v}`。具名类型 `T{Field: v}` 看着也像，但
			// `T` 完全可能是个 map 类型别名——语法层分不出来，要分得清需要
			// go/types 解析，那是另一个量级。取红。
			walk(node.Type)
			keyIsValue := true
			if _, isStruct := node.Type.(*ast.StructType); isStruct {
				keyIsValue = false
			}
			for _, elt := range node.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					walk(elt)
					continue
				}
				if keyIsValue {
					walk(kv.Key)
				} else if _, isIdent := kv.Key.(*ast.Ident); !isIdent {
					walk(kv.Key)
				}
				walk(kv.Value)
			}
			return
		case *ast.Field:
			// 跳过 Names（字段/形参的声明名），只走类型。
			walk(node.Type)
			return
		case *ast.Ident:
			if dotImported && node.Name == "Hint" {
				hit(node.Pos())
			}
			return
		}
		ast.Inspect(n, func(c ast.Node) bool {
			if c == nil || c == n {
				return c == n
			}
			walk(c)
			return false
		})
	}
	walk(file)
}
