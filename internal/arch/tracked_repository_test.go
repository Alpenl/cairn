package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestTrackedRepositoryPathsUsesIndexAndNULDelimiters(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(filepath.Join(root, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init")
	writeFixture(t, root, "tracked.txt", "tracked")
	writeFixture(t, root, "line\nbreak.md", "tracked special path")
	writeFixture(t, root, "docs/local.md", "ignored local documentation")
	writeFixture(t, root, "docs/staged.md", "staged documentation")
	writeFixture(t, root, "LICENSE", "untracked local license")
	writeFixture(t, root, ".git/info/exclude", "/docs/local.md\n/LICENSE\n")
	runGit(t, root, "add", "tracked.txt", "line\nbreak.md", "docs/staged.md")
	if err := os.Remove(filepath.Join(root, "docs", "staged.md")); err != nil {
		t.Fatal(err)
	}

	paths, err := trackedRepositoryPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"docs/staged.md", "line\nbreak.md", "tracked.txt"} {
		if !slices.Contains(paths, want) {
			t.Errorf("tracked paths %q do not contain %q", paths, want)
		}
	}
	for _, unwanted := range []string{"docs/local.md", "LICENSE"} {
		if slices.Contains(paths, unwanted) {
			t.Errorf("tracked paths %q contain ignored/untracked %q", paths, unwanted)
		}
	}
}

func TestTrackedRepositoryPathsResolvesLinkedWorktreeIndex(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	repository := filepath.Join(parent, "repository")
	worktree := filepath.Join(parent, "linked-worktree")
	if err := os.Mkdir(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init")
	runGit(t, repository, "config", "user.email", "arch-test@example.invalid")
	runGit(t, repository, "config", "user.name", "Architecture Test")
	writeFixture(t, repository, "tracked.txt", "base")
	runGit(t, repository, "add", "tracked.txt")
	runGit(t, repository, "commit", "-m", "test: seed repository")
	runGit(t, repository, "worktree", "add", "--detach", worktree, "HEAD")
	writeFixture(t, worktree, "worktree-only.txt", "staged in linked worktree")
	runGit(t, worktree, "add", "worktree-only.txt")

	paths, err := trackedRepositoryPaths(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(paths, "worktree-only.txt") {
		t.Fatalf("linked-worktree index was not used: %q", paths)
	}
	sourcePaths, err := trackedRepositoryPaths(repository)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(sourcePaths, "worktree-only.txt") {
		t.Fatalf("linked-worktree staged path leaked into source index: %q", sourcePaths)
	}
}

func TestTrackedRepositoryPathsFailsWithoutGitMetadata(t *testing.T) {
	t.Parallel()

	if _, err := trackedRepositoryPaths(t.TempDir()); err == nil {
		t.Fatal("trackedRepositoryPaths() succeeded without Git metadata")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func writeFixture(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
