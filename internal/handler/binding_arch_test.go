package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestProductionHandlersUseJSONBindingHelper prevents new handlers from
// bypassing the centralized 413/422 classification in binding.go.
func TestProductionHandlersUseJSONBindingHelper(t *testing.T) {
	forbidden := map[string]struct{}{
		"BindJSON":       {},
		"ShouldBindJSON": {},
	}
	var violations []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "binding.go" {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, blocked := forbidden[selector.Sel.Name]; blocked {
				position := fset.Position(selector.Pos())
				violations = append(violations, position.String()+": direct JSON binder bypasses bindJSONWithLimit")
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan production handlers: %v", err)
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("production handlers bypass the centralized JSON binder:\n  %s", strings.Join(violations, "\n  "))
	}
}
