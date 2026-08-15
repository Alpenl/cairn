package alloc_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// compositeLitShapes 覆盖 `CompositeLit.Type` 在合法 Go 里的**全部**可能 AST
// 类型，逐个断言 walkHintRefs 对该形状下的 `Hint` key 是走还是跳。
//
// ⚠ 这句话曾经是假的：`wantHit` 字段建出来之后**一行读取都没有**，整个文件也
// 没有任何一处调用 walkHintRefs——import 里连一个 WebTag 包都没有。也就是说
// 它只测了 go/parser，没测本仓的剪枝。实测：把 walkHintRefs 的极性回退成第
// 十四轮那个 HIGH 缺陷（`case *ast.MapType, *ast.ArrayType, nil:` 走、
// `default:` 跳），这两个测试照样绿。
//
// 这和 callsites_test.go 里刚被处决的 `bound int` 字段是同一样东西——同一个
// 包、同一个目录、三十行之外，「长得像断言的装饰品」被重新造了一遍。已接上：
// 见下面的 TestWalkHintRefsHonorsShapes。
//
// 为什么要有这张表：
//
// 这件事此前只以**散文**存在（架构审查日志里那句「八种形状逐个跑了一遍」）。
// 散文的问题是数字会错，而且没有任何东西会因此变红——实测发生过两次：先写
// 「九种」（实为 9 个用例、8 种形状），改完之后修正文本自己又写了「五种形状」
// （实为 5 个构造、4 种形状），两次双计的都是同一个 `*ast.Ident`。
//
// 更要紧的是：散文说「跑了一遍」，但那一遍是手工的，不在 CI 里，下次改剪枝
// 没有任何东西会提醒你重跑。这张表把那句话变成断言——数字要么对，要么红。
//
// 枚举的依据是 Go spec 的产生式，不是「想不出第九种」：
//
//	CompositeLit = LiteralType LiteralValue
//	LiteralType  = StructType | ArrayType | "[" "..." "]" ElementType
//	             | SliceType | MapType | TypeName [ TypeArgs ]
//	TypeName     = identifier | QualifiedIdent
//
// 映射到 go/ast：StructType、ArrayType（数组/`[...]E`/切片三种语法共用它）、
// MapType、Ident（identifier）、SelectorExpr（QualifiedIdent）、IndexExpr
// （TypeName + 1 个类型实参）、IndexListExpr（+≥2 个）、nil（省略类型的内层
// 字面量）——**八种**。
//
// `*ast.ParenExpr` 不是第九种：parser 直接拒绝，`(map[any]string){1:"x"}` 报
// `syntax error: cannot parenthesize type in composite literal`。它与
// `*ast.BadExpr` 只在**带错源码**里作为节点出现，而 walkHintRefs 见不到它们
// ——扫描侧拿到 parseErr 就 t.Fatalf，是响亮的红。
var compositeLitShapes = []struct {
	name string
	kind string // 期望的 CompositeLit.Type 动态类型，"" 表示 nil（省略类型）
	src  string
	// wantHit：该形状下裸 Ident 的 key 是否应被当作引用走到。
	// 只有内联匿名结构体是 false——它是唯一能**语法确定** key 必是字段名的形状。
	// 具名类型 `T{Field: v}` 看着也像，但 T 完全可能是个 map 别名，语法层分不
	// 出来；分得清需要 go/types。取红（误报）而非绿（漏扫）。
	wantHit bool
}{
	{
		name: "Ident（具名 map 类型）", kind: "*ast.Ident", wantHit: true,
		src: "type NamedMap map[any]string\n\nfunc f() { _ = NamedMap{Hint: \"x\"} }",
	},
	{
		name: "Ident（map 类型别名）", kind: "*ast.Ident", wantHit: true,
		src: "type AliasMap = map[any]string\n\nfunc f() { _ = AliasMap{Hint: \"x\"} }",
	},
	{
		name: "SelectorExpr（跨包具名 map）", kind: "*ast.SelectorExpr", wantHit: true,
		src: "import \"webtag/internal/zzshapes\"\n\nfunc f() { _ = zzshapes.M{Hint: \"x\"} }",
	},
	{
		name: "IndexExpr（泛型单参）", kind: "*ast.IndexExpr", wantHit: true,
		src: "type GenMap[V any] map[any]V\n\nfunc f() { _ = GenMap[string]{Hint: \"x\"} }",
	},
	{
		name: "IndexListExpr（泛型多参）", kind: "*ast.IndexListExpr", wantHit: true,
		src: "type Gen2[K comparable, V any] map[K]V\n\nfunc f() { _ = Gen2[any, string]{Hint: \"x\"} }",
	},
	{
		name: "MapType（内联 map）", kind: "*ast.MapType", wantHit: true,
		src: "func f() { _ = map[any]string{Hint: \"x\"} }",
	},
	{
		name: "ArrayType（数组索引 key）", kind: "*ast.ArrayType", wantHit: true,
		src: "func f() { _ = [8]string{Hint: \"x\"} }",
	},
	{
		name: "StructType（内联匿名结构体）", kind: "*ast.StructType", wantHit: false,
		src: "func f() { _ = struct{ Hint string }{Hint: \"x\"} }",
	},
	{
		name: "nil（省略类型的内层字面量）", kind: "", wantHit: true,
		src: "type S struct{ Hint string }\n\nfunc f() { _ = []S{{Hint: \"x\"}} }",
	},
}

// TestCompositeLitShapesAreExhaustive 断言上表**恰好**覆盖八种形状，一个不多、
// 一个不少。它守的是「八」这个数字本身——此前那个数字只活在散文里，错了没人知道。
func TestCompositeLitShapesAreExhaustive(t *testing.T) {
	t.Parallel()

	// 合法 CompositeLit.Type 的全集，依据见 compositeLitShapes 的文档注释。
	want := map[string]bool{
		"*ast.Ident": true, "*ast.SelectorExpr": true,
		"*ast.IndexExpr": true, "*ast.IndexListExpr": true,
		"*ast.ArrayType": true, "*ast.MapType": true,
		"*ast.StructType": true, "": true, // "" = nil，省略类型
	}
	got := map[string]bool{}
	for _, tc := range compositeLitShapes {
		got[tc.kind] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("形状 %q 没有对应用例——枚举不完整", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("用例覆盖了 %q，但它不在合法 CompositeLit.Type 全集里——"+
				"要么全集写漏了，要么这个用例的期望类型写错了", k)
		}
	}
	if len(want) != 8 {
		t.Fatalf("全集有 %d 项而不是 8——改动它时请连同 compositeLitShapes 的"+
			"文档注释一起复核（那里写着依据）", len(want))
	}
}

// TestCompositeLitShapeIsWhatWeThink 断言每个用例产生的 AST 类型**确实**是它
// 声称的那种。没有这条，上面那张表可能整体建立在误解上——比如以为
// `GenMap[string]{}` 的 Type 是 Ident，其实是 IndexExpr。
func TestCompositeLitShapeIsWhatWeThink(t *testing.T) {
	t.Parallel()

	for _, tc := range compositeLitShapes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file := parseShape(t, tc.src)
			var kinds []string
			ast.Inspect(file, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				// 只看带 KeyValueExpr 的那个字面量——外层 `[]S{…}` 之类不算。
				for _, e := range cl.Elts {
					if _, isKV := e.(*ast.KeyValueExpr); isKV {
						kinds = append(kinds, fmt.Sprintf("%T", cl.Type))
						break
					}
				}
				return true
			})
			if len(kinds) != 1 {
				t.Fatalf("源码里带 key 的复合字面量有 %d 个（期望 1）：%v", len(kinds), kinds)
			}
			want := tc.kind
			if want == "" {
				want = "<nil>"
			}
			if kinds[0] != want {
				t.Errorf("CompositeLit.Type 实为 %s，用例声称 %s", kinds[0], want)
			}
		})
	}
}

// TestWalkHintRefsHonorsShapes 是这张表**唯一测到本仓代码**的地方：把每个用例
// 的源码喂给 walkHintRefs，断言它对该形状下的裸 Ident key 走还是跳。
//
// 它是第十四轮那个 HIGH 缺陷（`switch` 的 default 落在「跳过」一侧，导致具名
// map 类型、类型别名、跨包、泛型实例化四类静默漏扫）的回归防线。上面两个测试
// 都拦不住它——实测回退极性后它们全绿。
func TestWalkHintRefsHonorsShapes(t *testing.T) {
	t.Parallel()

	for _, tc := range compositeLitShapes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file := parseShape(t, tc.src)

			// 只数**落在该复合字面量花括号内**的命中，不是全文件计数。
			//
			// 只数次数有盲点，实测构造过：`nil` 型也跳过 key **且**删掉 Field
			// 跳过时，那条用例的命中改由**字面量之外**的字段声明
			// `Hint string` 提供，总数仍是 1，钉死次数也照样绿。位置才是要
			// 断言的东西。
			lbrace, rbrace := keyedLiteralRange(t, file)
			hits, total := 0, 0
			// dot-import（locals = ["."]）下裸 Ident 才参与匹配，那正是剪枝
			// 存在的场景。
			walkHintRefs(file, []string{"."}, func(pos token.Pos) {
				total++
				if pos >= lbrace && pos <= rbrace {
					hits++
				}
			})

			// **只做范围内计数不够，还要断言范围外为零。**
			//
			// 位置过滤修好了「次数对了但位置错了」，却顺手关掉了另一条防线：
			// `struct{ Hint string }{Hint: "x"}` 的字段声明位于 Lbrace
			// **之前**，被过滤器筛掉，于是「删掉 *ast.Field 跳过」这个变异
			// 从红变绿——而 callsites_test.go 里记着那个事故真的发生过
			// （删除锚点取到下一个 case，连带删掉 Field 分支），当场抓住它的
			// 就是 StructType 这一行。基线各行 total 与 hits 全部相等，
			// 所以这条零成本。
			if total != hits {
				t.Errorf("字面量之外还有 %d 次命中——剪枝命中了它不该命中的位置"+
					"（比如字段声明名）", total-hits)
			}

			want := 0
			if tc.wantHit {
				want = 1
			}
			if hits != want {
				if want == 1 {
					t.Errorf("该形状下的 `Hint` key 应被当作引用走到且恰好一次，"+
						"实际在字面量内命中 %d 次——0 次是**静默漏扫**"+
						"（绿，致命）", hits)
				} else {
					t.Errorf("该形状下的 `Hint` key 是字段名、不该命中，"+
						"实际在字面量内命中 %d 次——误报（红，安全），"+
						"但与本表登记的取舍不符", hits)
				}
			}
		})
	}
}

// bindingForms 覆盖 `webtag/internal/alloc` 的各种 import 绑定形式，断言
// allocLocalNames + walkHintRefs 对每种都认得出调用点。
//
// **不说「全部」**：ImportSpec 的四种原始形式（默认 / 具名 / `.` / `_`）确实都
// 在，但表里还收了组合（`_`+具名、具名+具名、dot+具名），而组合是无界的，
// 「全部」在字面上不可能成立。收这三种是因为它们同属**顺序依赖族**——
// allocLocalNames 里任何形式的提前 return 都会让正确性取决于书写顺序，那正是
// 这张表存在的理由。
//
// （`import alloc "webtag/internal/alloc"` 显式写默认名不是第八种：它走的是
// 与「具名别名」完全相同的分支，实测 locals=[alloc]、hits=1。）
//
// 为什么需要它：callsites_test.go 里那两处修复（解析 import 找本地绑定名而非
// 硬认包名；收集**全部**绑定名而非首个匹配即返回）的注释都用「实测三种绕过」
// 「实测两行对调就红」背书，但**没有任何一条会红的路径**——实测把
// `matchesLocal(pkg.Name)` 回退成 `pkg.Name == "alloc"`，整包测试照样绿。
//
// 那是与 `bound int`、`wantHit` 同一样东西的第三次现身，只是躲在函数的另一半。
// 仓内 10 处生产 import 全用默认包名，没有一个别名或 dot-import 撑住这条路径。
var bindingForms = []struct {
	name string
	want int // 期望命中次数
	src  string
}{
	{
		name: "默认包名", want: 1,
		src: "import \"webtag/internal/alloc\"\n\nfunc f(n int) int { return alloc.Hint(n) }",
	},
	{
		name: "具名别名", want: 1,
		src: "import al \"webtag/internal/alloc\"\n\nfunc f(n int) int { return al.Hint(n) }",
	},
	{
		name: "dot-import", want: 1,
		src: "import . \"webtag/internal/alloc\"\n\nfunc f(n int) int { return Hint(n) }",
	},
	{
		name: "_ 在前 + 具名在后", want: 1,
		src: "import (\n\t_ \"webtag/internal/alloc\"\n\tal \"webtag/internal/alloc\"\n)\n\nfunc f(n int) int { return al.Hint(n) }",
	},
	{
		name: "具名 + 具名（首个匹配即返回会漏后者）", want: 1,
		src: "import (\n\ta1 \"webtag/internal/alloc\"\n\ta2 \"webtag/internal/alloc\"\n)\n\nfunc f(n int) int { _ = a1.MaxPrealloc; return a2.Hint(n) }",
	},
	{
		// selector 接收者位置：`newBuf(alloc.Hint(n)).Len()`。
		//
		// 钉的是 walkHintRefs 里 SelectorExpr 那一支的 `walk(node.X)`——
		// **补这一行之前**删掉它整包绿（实测），补上之后该变异红在本行（实测）。
		// 那一支对文件里**每一个** selector 都触发、末尾
		// return，`walk(node.X)` 是它唯一的下钻语句。它一坏，凡是嵌在任意
		// selector 接收者里的 alloc.Hint 全部漏扫。登记表现有 11 处没有一处
		// 落在这个位置，真仓扫描也兜不住。
		name: "selector 接收者里的调用", want: 1,
		src: "import \"webtag/internal/alloc\"\n\ntype buf struct{ n int }\n\nfunc newBuf(n int) buf { return buf{n: n} }\n\nfunc (b buf) Len() int { return b.n }\n\nfunc f(n int) int { return newBuf(alloc.Hint(n)).Len() }",
	},
	{
		// 结构体字面量的**字段值**位置——生产里最常见的形状之一。
		//
		// 钉的是 CompositeLit 那一支的 `walk(kv.Value)`——**补这一行之前**删掉它
		// 整包绿（实测），补上之后该变异红在本行（实测）。
		name: "复合字面量字段值里的调用", want: 1,
		src: "import \"webtag/internal/alloc\"\n\ntype box struct{ Items []int }\n\nfunc f(n int) box { return box{Items: make([]int, 0, alloc.Hint(n))} }",
	},
	{
		// 具名在前、dot 在后——顺序是刻意的。写成 dot 在前时，
		// 「dotImported 只看 locals[0]」这个缺陷测不出来（locals[0] 恰好就是
		// "."）。与「具名+具名」同属顺序依赖族。
		name: "具名 + dot 共存（dot 在后）", want: 2,
		src: "import (\n\tal \"webtag/internal/alloc\"\n\t. \"webtag/internal/alloc\"\n)\n\nfunc f(n int) int { return Hint(n) + al.Hint(n) }",
	},
	{
		// ⚠ 源码里**必须真的出现 alloc.Hint**，否则 hits 恒为 0、这一行没有
		// 失败方向。上一版写的是 `func f() {}`——一个建在「专门为消灭装饰品
		// 而写的表」里的装饰品。
		//
		// 为它背书的实测**曾经写错过**：写的是「把 `_` 也当成绑定名会红」，
		// 实测那个变异整包绿——`locals = ["_"]` 无害，因为 `_` 在 Go 里不能
		// 作表达式操作数，`_.Hint` 不是合法源码，永远不会匹配。
		// 真正红在这一行的是**把 `_` 当成默认包名 `alloc`**（实测红）。
		name: "只有 _（副作用引入，绑定名不该产生匹配）", want: 0,
		src: "import _ \"webtag/internal/alloc\"\n\nfunc f(n int) int { return alloc.Hint(n) }",
	},
	{
		name: "根本没 import（局部变量恰好叫 alloc）", want: 0,
		src: "type T struct{}\n\nfunc (T) Hint(n int) int { return n }\n\nfunc f() int { alloc := T{}; return alloc.Hint(5) }",
	},
}

// TestBindingFormsAreRecognized 是 allocLocalNames / matchesLocal 的回归防线。
func TestBindingFormsAreRecognized(t *testing.T) {
	t.Parallel()

	for _, tc := range bindingForms {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file := parseShape(t, tc.src)
			locals := allocLocalNames(file)
			hits := 0
			walkHintRefs(file, locals, func(token.Pos) { hits++ })
			if hits != tc.want {
				t.Errorf("命中 %d 次，期望 %d（解析出的绑定名：%v）——"+
					"少了是**静默漏扫**（绿，致命），多了是误报（红，安全）",
					hits, tc.want, locals)
			}
		})
	}
}

// keyedLiteralRange 返回源码里那个带 KeyValueExpr 的复合字面量的花括号范围。
// 用它把命中判定限定在字面量内部——见 TestWalkHintRefsHonorsShapes 的说明。
func keyedLiteralRange(t *testing.T, file *ast.File) (token.Pos, token.Pos) {
	t.Helper()
	var lb, rb token.Pos
	found := 0
	ast.Inspect(file, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, e := range cl.Elts {
			if _, isKV := e.(*ast.KeyValueExpr); isKV {
				lb, rb = cl.Lbrace, cl.Rbrace
				found++
				break
			}
		}
		return true
	})
	if found != 1 {
		t.Fatalf("源码里带 key 的复合字面量有 %d 个（期望 1）——用例本身写错了", found)
	}
	return lb, rb
}

func parseShape(t *testing.T, body string) *ast.File {
	t.Helper()
	src := "package zzshape\n\n" + body + "\n"
	file, err := parser.ParseFile(token.NewFileSet(), "shape.go", src, 0)
	if err != nil {
		t.Fatalf("解析用例源码失败（用例本身写错了）：%v\n%s", err, src)
	}
	return file
}
