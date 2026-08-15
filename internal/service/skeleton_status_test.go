package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkeletonStatusHasNoProducer 锁住 skeleton 是纯读兼容状态这个事实。
//
// model.LinkStatusSkeleton 注释写着「历史占位行，保留兼容旧数据」：全仓有 4 处
// 消费分支，但**没有任何代码再产生它**。删掉那些分支会让存量 skeleton 行落入
// 未预期路径，所以保留是对的；但「无生产者」此前只是个观察，没有任何机制保证。
//
// 一旦有人重新开始写 skeleton，这个状态就从「只读兼容」变成「活状态」，而那 4
// 处消费分支是按前者写的（例如 submit_batch 把它与 pending/processing 并列当作
// 在途）。本测试让这种转变必须是显式决定，而不是某次改动的副作用。
//
// 扫描的是赋值与复合字面量，不含 case / 比较——消费是允许的，产生不允许。
func TestSkeletonStatusHasNoProducer(t *testing.T) {
	t.Parallel()

	var producers []string
	scanned := 0
	fset := token.NewFileSet()

	// 必须用绝对路径：walk 根若写成 "..", 下面那条
	// `strings.HasPrefix(name, ".")` 会命中根自身（d.Name() == ".."），第一次
	// 回调就 fs.SkipDir，整棵树被跳过、扫描面归零。
	//
	// （这跟 t.Parallel 无关——同一进程内 cwd 不变。最初的注释归因错了，而一条
	// 用来警示后人的注释写错因果，后人就会照着"并行"去理解，把它改回相对路径。）
	//
	// 下方的 scanned 自检正是为此存在，它第一次跑就把这个问题顶了出来。
	root, absErr := filepath.Abs("..")
	if absErr != nil {
		t.Fatalf("resolve internal root: %v", absErr)
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == "testdata" || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		// model 包定义常量本身不算产生。
		if strings.Contains(filepath.ToSlash(path), "/model/") {
			return nil
		}
		scanned++
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				for _, rhs := range node.Rhs {
					if mentionsSkeleton(rhs) {
						producers = append(producers, fset.Position(node.Pos()).String())
					}
				}
			case *ast.KeyValueExpr:
				if mentionsSkeleton(node.Value) {
					producers = append(producers, fset.Position(node.Pos()).String())
				}
			case *ast.CallExpr:
				// 作为实参传出去同样是产生：sink(model.LinkStatusSkeleton)。
				for _, arg := range node.Args {
					if mentionsSkeleton(arg) {
						producers = append(producers, fset.Position(node.Pos()).String())
					}
				}
			case *ast.ReturnStmt:
				for _, res := range node.Results {
					if mentionsSkeleton(res) {
						producers = append(producers, fset.Position(node.Pos()).String())
					}
				}
			case *ast.BasicLit:
				// 本仓的 link status 写入几乎全活在 repository 的原始 SQL 里
				// （convertSiteToReadingSQL 写 status='pending' 正是第 1 轮 P0
				// 的成因）。新生产者最可能就长成 SQL，纯 AST 表达式扫描对此完全
				// 失明，所以这里额外查字符串字面量。
				if node.Kind == token.STRING && sqlWritesSkeleton(node.Value) {
					producers = append(producers, fset.Position(node.Pos()).String())
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// 扫描面必须足够大，否则「没找到生产者」只是因为根本没扫到。
	if scanned < 100 {
		t.Fatalf("只扫描了 %d 个文件，扫描面异常——「无生产者」的结论不可信", scanned)
	}

	if len(producers) > 0 {
		t.Fatalf("检测到 %d 处产生 skeleton 状态：\n  %s\n\n"+
			"skeleton 是只读的历史兼容状态，现有 4 处消费分支按此编写"+
			"（submit_batch 把它与 pending/processing 并列当作在途）。"+
			"若确实要让它重新成为活状态，请先复核那些分支再更新本测试。",
			len(producers), strings.Join(producers, "\n  "))
	}
}

// sqlWritesSkeleton 判断字符串字面量是否是一条写入 skeleton 的 SQL。
//
// 只认「写」的形态（SET status = 'skeleton' / VALUES 里出现），不认比较与过滤
// （status <> 'skeleton'、IN (...) 之类）——消费允许，产生不允许。
func sqlWritesSkeleton(quoted string) bool {
	lowered := strings.ToLower(quoted)
	if !strings.Contains(lowered, "'skeleton'") {
		return false
	}
	// 排除比较/过滤：这些是消费，不是产生。
	for _, consume := range []string{"<> 'skeleton'", "!= 'skeleton'", "= 'skeleton'\" in", "in ('skeleton'"} {
		if strings.Contains(lowered, consume) {
			return false
		}
	}
	// CHECK 约束枚举合法值，属声明而非写入。
	if strings.Contains(lowered, "check") {
		return false
	}
	return strings.Contains(lowered, "set status") || strings.Contains(lowered, "insert into")
}

// mentionsSkeleton 判断表达式是否求值为 LinkStatusSkeleton。
func mentionsSkeleton(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return sel.Sel != nil && sel.Sel.Name == "LinkStatusSkeleton"
}
