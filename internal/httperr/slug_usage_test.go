package httperr_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestErrorSlugsAreReferencedAsConstants 禁止在 handler / middleware 里把错误
// slug 写成字符串字面量。
//
// 本测试反向扫描：任何出现在 httperr 常量表里的 slug 值，都不允许再以字面量
// 形式出现在别处。新增错误码时先加常量再引用，天然满足。
func TestErrorSlugsAreReferencedAsConstants(t *testing.T) {
	t.Parallel()

	slugs := knownSlugValues(t)
	if len(slugs) == 0 {
		t.Fatal("未从 httperr 解析出任何 Code* 常量，测试失去意义")
	}

	type offence struct{ file, slug string }
	var found []offence
	scanned := 0

	for _, dir := range []string{"../handler", "../middleware", "../service", "../app"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				if name == "testdata" || strings.HasPrefix(name, "_") {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			scanned++
			fset := token.NewFileSet()
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				return parseErr
			}
			ast.Inspect(file, func(n ast.Node) bool {
				// 只豁免刻意维护的 slug 常量表（middleware 那份 ErrCode*，
				// 见 TestMiddlewareSlugsMatchHTTPErr），不是所有 ValueSpec。
				//
				// 早先跳过整个 ValueSpec 会剪掉整棵子树，于是
				// `var x = "link_not_found"` 和 `var m = map[string]string{...}`
				// 里的字面量全部隐形——那才是真正该抓的形态。
				if spec, isSpec := n.(*ast.ValueSpec); isSpec && isSlugTableSpec(spec) {
					return false
				}
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				val, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					return true
				}
				if slugs[val] {
					found = append(found, offence{file: path, slug: val})
				}
				return true
			})
			return nil
		})
		if err != nil {
			// 不再吞掉 IsNotExist：目录改名会让扫描面静默归零、门禁全绿。
			// 这正是同仓 TestSkeletonStatusHasNoProducer 亲手踩过的坑。
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	// 扫描面自检：没扫到文件时「没找到违规」毫无意义，而这种失效完全静默。
	if scanned < 50 {
		t.Fatalf("只扫描了 %d 个文件，扫描面异常——「无字面量」的结论不可信", scanned)
	}

	if len(found) > 0 {
		msgs := make([]string, 0, len(found))
		for _, f := range found {
			msgs = append(msgs, f.file+" 写了字面量 "+strconv.Quote(f.slug))
		}
		sort.Strings(msgs)
		t.Fatalf("错误 slug 必须引用 httperr 常量，不得写字面量：\n  %s\n\n"+
			"字面量与常量的值今天一致不代表明天一致——改了常量，字面量不会跟着变，"+
			"而前端按常量做的分支会静默失配。",
			strings.Join(msgs, "\n  "))
	}
}

// isSlugTableSpec 判断一个声明是否属于刻意维护的 slug 常量表：名字以 Code /
// ErrCode 开头。只有这类声明豁免字面量检查。
func isSlugTableSpec(spec *ast.ValueSpec) bool {
	for _, name := range spec.Names {
		if strings.HasPrefix(name.Name, "Code") || strings.HasPrefix(name.Name, "ErrCode") {
			return true
		}
	}
	return false
}

// knownSlugValues 解析 httperr 包里所有 Code* 常量的字面量取值。
func knownSlugValues(t *testing.T) map[string]bool {
	t.Helper()

	// 逐文件 ParseFile 而非 ParseDir：后者自 Go 1.25 起废弃（不考虑 build tag）。
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read httperr dir: %v", err)
	}

	fset := token.NewFileSet()
	out := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, ident := range spec.Names {
				if !strings.HasPrefix(ident.Name, "Code") || i >= len(spec.Values) {
					continue
				}
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if val, uerr := strconv.Unquote(lit.Value); uerr == nil && val != "" {
					out[val] = true
				}
			}
			return true
		})
	}
	return out
}
