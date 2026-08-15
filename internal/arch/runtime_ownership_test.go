package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Runtime drain relies on pgxpool ownership statistics. Production code may
// not detach raw connections from the pool; River's vendored listener is the
// sole documented exception and is fenced by queue.Stop before owner revoke.
func TestProductionCodeDoesNotHijackPgxpoolConnections(t *testing.T) {
	t.Parallel()

	var offenders []string
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if ok && selector.Sel.Name == "Hijack" {
				offenders = append(offenders, path)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan internal Go files: %v", err)
	}
	slices.Sort(offenders)
	if len(offenders) != 0 {
		t.Fatalf("production pgxpool Hijack calls bypass Runtime drain: %v", offenders)
	}
}

// Every hardened HTTP client created by the composition root owns an idle
// connection pool. Construction must go through the Runtime HTTP client owner
// so Build rollback and Runtime.Close can release those transports.
func TestProductionHTTPClientsAreConstructedByRuntimeOwner(t *testing.T) {
	t.Parallel()

	const ownerFile = "../app/deps_http_clients.go"
	constructors := map[string]struct{}{
		"NewHTTPClient":            {},
		"NewHardenedHTTPClient":    {},
		"NewHTTPClientWithOptions": {},
	}
	var offenders []string
	err := filepath.WalkDir("../app", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || path == ownerFile {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, constructor := constructors[selector.Sel.Name]; constructor {
				offenders = append(offenders, path)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan app Go files: %v", err)
	}
	slices.Sort(offenders)
	if len(offenders) != 0 {
		t.Fatalf("production HTTP clients bypass Runtime owner: %v", offenders)
	}
}

// The helper intentionally permits local Docker-less runs to skip. The
// dedicated database workflow must opt into the fail-closed branch so a green
// CI result always represents a real PostgreSQL run.
func TestDedicatedDBIntegrationWorkflowRequiresPostgres(t *testing.T) {
	t.Parallel()

	workflowData, err := os.ReadFile("../../.github/workflows/dbintegration.yml")
	if err != nil {
		t.Fatalf("read dbintegration workflow: %v", err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflowData, &workflow); err != nil {
		t.Fatalf("parse dbintegration workflow: %v", err)
	}
	postgresJob, ok := workflow.Jobs["postgres"]
	if !ok {
		t.Fatal("dbintegration workflow has no postgres job")
	}
	for _, step := range postgresJob.Steps {
		if strings.Contains(step.Run, "make test-dbintegration-required") {
			return
		}
	}
	t.Fatal("postgres job does not invoke make test-dbintegration-required")
}
