package arch

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"webtag/internal/representation"
)

func TestPublicClientsDeclareProductionRepresentationContract(t *testing.T) {
	t.Parallel()

	repoRoot := filepath.Join("..", "..")
	for _, client := range []struct {
		path        string
		declaration string
	}{
		{
			path:        "reader/src/lib/api/client.ts",
			declaration: "const REPRESENTATION_CONTRACT = '" + representation.Contract + "'",
		},
		{
			path:        "extension/src/api/webtag-client.ts",
			declaration: "const REPRESENTATION_CONTRACT = '" + representation.Contract + "'",
		},
		{
			path:        "mobile/android/app/src/main/java/com/alpenl/webtag/share/contract/MobileContract.kt",
			declaration: "const val REPRESENTATION_CONTRACT = \"" + representation.Contract + "\"",
		},
		{
			path:        "mobile/ios/WebTagShare/Shared/WebTagShareCore.swift",
			declaration: "private let supportedRepresentationContract = \"" + representation.Contract + "\"",
		},
	} {
		data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(client.path)))
		if err != nil {
			t.Fatalf("read %s: %v", client.path, err)
		}
		if !strings.Contains(string(data), client.declaration) {
			t.Errorf("%s does not declare production representation contract %q", client.path, representation.Contract)
		}
	}
}

func TestRepositoryContainsNoProjectDocumentationBeyondRequiredAttribution(t *testing.T) {
	t.Parallel()

	repoRoot := filepath.Join("..", "..")
	tracked, err := trackedRepositoryPaths(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	trackedSet := make(map[string]struct{}, len(tracked))
	for _, path := range tracked {
		trackedSet[path] = struct{}{}
	}

	for _, relative := range []string{".agents", "design", "docs", "extension/.claude", "extension/docs"} {
		for _, path := range tracked {
			if path == relative || strings.HasPrefix(path, relative+"/") {
				t.Errorf("public repository must not track %s", relative)
				break
			}
		}
	}

	const markdownAttribution = "internal/service/translator/dictionary/README.md"
	for _, required := range []string{
		"LICENSE",
		"extension/LICENSE",
		"extension/NOTICE",
		"internal/service/translator/dictionary/LICENSE",
		markdownAttribution,
	} {
		if _, ok := trackedSet[required]; !ok {
			t.Errorf("required license or attribution file %s is not tracked", required)
			continue
		}
		if _, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(required))); err != nil {
			t.Errorf("required license or attribution file %s is missing: %v", required, err)
		}
	}

	var documents []string
	for _, path := range tracked {
		if strings.HasPrefix(path, "vendor/") || !strings.EqualFold(filepath.Ext(path), ".md") {
			continue
		}
		if path != markdownAttribution {
			documents = append(documents, path)
		}
	}
	if len(documents) > 0 {
		sort.Strings(documents)
		t.Fatalf("public repository contains project documentation:\n  %s", strings.Join(documents, "\n  "))
	}
}

// trackedRepositoryPaths reads the index, not the checkout. Local ignored
// documentation is intentionally invisible, while staged paths remain visible
// even if their working-tree files have since been removed. -z is required:
// Git paths may legally contain whitespace, quotes, and newlines.
func trackedRepositoryPaths(repoRoot string) ([]string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "ls-files", "-z", "--")
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("read tracked repository paths with git ls-files -z: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if len(output) == 0 {
		return nil, nil
	}
	if output[len(output)-1] != 0 {
		return nil, fmt.Errorf("git ls-files -z returned a non-NUL-terminated path list")
	}

	fields := bytes.Split(output[:len(output)-1], []byte{0})
	paths := make([]string, 0, len(fields))
	for _, field := range fields {
		if len(field) == 0 {
			return nil, fmt.Errorf("git ls-files -z returned an empty path")
		}
		paths = append(paths, string(field))
	}
	return paths, nil
}

func TestAndroidSessionFixturesDoNotPinPreviousRepresentationContract(t *testing.T) {
	t.Parallel()

	match := regexp.MustCompile(`^v([1-9][0-9]*)$`).FindStringSubmatch(representation.Contract)
	if match == nil {
		t.Fatalf("production representation contract %q is not versioned", representation.Contract)
	}
	version, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("parse production representation contract %q: %v", representation.Contract, err)
	}
	if version == 1 {
		return
	}
	previous := `"v` + strconv.Itoa(version-1) + `"`

	repoRoot := filepath.Join("..", "..")
	for _, relative := range []string{
		"mobile/android/app/src/test",
		"mobile/android/app/src/androidTest",
	} {
		err := filepath.WalkDir(filepath.Join(repoRoot, filepath.FromSlash(relative)), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".kt" {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if strings.Contains(string(data), previous) {
				t.Errorf("%s pins previous representation contract %s instead of using REPRESENTATION_CONTRACT", path, previous)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", relative, err)
		}
	}
}
