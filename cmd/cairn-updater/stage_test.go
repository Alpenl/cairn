package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"webtag/internal/releasetrust"
)

// Unpacking runs as root, so the tests here are about what must never end up on
// disk rather than about what should.

func TestUnpackingRefusesAMemberVerificationNeverSaw(t *testing.T) {
	honest := buildTarGz(t, []archiveFile{
		{path: "root", mode: 0o755, dir: true},
		{path: "root/index.html", data: []byte("hello"), mode: 0o644},
	})
	contents := mustInspect(t, honest)

	// A second archive with the same declared contents plus one extra member.
	// Replaying it blindly would write a file nothing vouched for.
	smuggled := buildTarGz(t, []archiveFile{
		{path: "root", mode: 0o755, dir: true},
		{path: "root/index.html", data: []byte("hello"), mode: 0o644},
		{path: "root/backdoor.sh", data: []byte("#!/bin/sh\n"), mode: 0o755},
	})

	destination := filepath.Join(t.TempDir(), "out")
	err := extractArchive(smuggled, contents, destination)
	if err == nil {
		t.Fatal("an unverified member was unpacked")
	}
	if !strings.Contains(err.Error(), "not present when the archive was verified") {
		t.Fatalf("expected a verification-mismatch message, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(destination, "root", "backdoor.sh")); statErr == nil {
		t.Fatal("the unverified member reached the filesystem")
	}
}

func TestUnpackingRefusesAMemberWhoseBytesChanged(t *testing.T) {
	original := buildTarGz(t, []archiveFile{
		{path: "root", mode: 0o755, dir: true},
		{path: "root/index.html", data: []byte("original"), mode: 0o644},
	})
	contents := mustInspect(t, original)
	// Same path and same length, different bytes. Only the per-member digest
	// catches this.
	swapped := buildTarGz(t, []archiveFile{
		{path: "root", mode: 0o755, dir: true},
		{path: "root/index.html", data: []byte("replaced"), mode: 0o644},
	})

	err := extractArchive(swapped, contents, filepath.Join(t.TempDir(), "out"))
	if err == nil {
		t.Fatal("a member whose bytes changed was unpacked")
	}
	if !strings.Contains(err.Error(), "hashed to") {
		t.Fatalf("expected a digest mismatch, got %v", err)
	}
}

func TestUnpackingProducesTheDeclaredModesRegardlessOfTheHeader(t *testing.T) {
	archive := buildTarGz(t, []archiveFile{
		{path: "pkg", mode: 0o755, dir: true},
		// A setuid header on a shipped binary. The manifest verification would
		// already have refused an undeclared executable, and unpacking must not
		// reproduce the header mode either way.
		{path: "pkg/webtag", data: []byte("#!/bin/sh\n"), mode: 0o4755},
		{path: "pkg/notes.txt", data: []byte("plain"), mode: 0o600},
	})
	contents := mustInspect(t, archive)
	destination := filepath.Join(t.TempDir(), "out")
	if err := extractArchive(archive, contents, destination); err != nil {
		t.Fatalf("unpack: %v", err)
	}

	binary, err := os.Stat(filepath.Join(destination, "pkg", "webtag"))
	if err != nil {
		t.Fatalf("stat binary: %v", err)
	}
	if binary.Mode()&os.ModeSetuid != 0 {
		t.Fatal("a setuid bit from the archive header reached the filesystem")
	}
	if binary.Mode().Perm() != 0o755 {
		t.Fatalf("expected 0755 for a declared executable, got %04o", binary.Mode().Perm())
	}
	plain, err := os.Stat(filepath.Join(destination, "pkg", "notes.txt"))
	if err != nil {
		t.Fatalf("stat plain file: %v", err)
	}
	if plain.Mode().Perm() != 0o644 {
		t.Fatalf("expected 0644 for a non-executable, got %04o", plain.Mode().Perm())
	}
}

func TestSafeJoinKeepsMembersInsideTheRoot(t *testing.T) {
	root := "/opt/webtag/releases/.incoming-v1.2.3"
	for _, name := range []string{"../escape", "../../etc/passwd", "a/../../b"} {
		if _, err := safeJoin(root, name); err == nil {
			t.Fatalf("member %q was allowed to resolve outside the root", name)
		}
	}
	joined, err := safeJoin(root, "pkg/webtag")
	if err != nil {
		t.Fatalf("a legitimate member was refused: %v", err)
	}
	if joined != root+"/pkg/webtag" {
		t.Fatalf("expected %s, got %s", root+"/pkg/webtag", joined)
	}
}

// TestTheCurrentLinkIsNeverAbsent is why the switch is a rename rather than a
// remove followed by a create.
func TestTheCurrentLinkIsNeverAbsent(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "current")
	first := filepath.Join(dir, "v1.2.2")
	second := filepath.Join(dir, "v1.2.3")
	for _, path := range []string{first, second} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
	}
	if err := switchSymlink(link, first); err != nil {
		t.Fatalf("first switch: %v", err)
	}
	if got := readSymlink(link); got != first {
		t.Fatalf("expected %s, got %s", first, got)
	}
	// Switching over an existing link must succeed rather than fail on "file
	// exists", and must land on the new target.
	if err := switchSymlink(link, second); err != nil {
		t.Fatalf("second switch: %v", err)
	}
	if got := readSymlink(link); got != second {
		t.Fatalf("expected %s, got %s", second, got)
	}
	// No switch artefacts left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".switch-") {
			t.Fatalf("a switch artefact was left behind: %s", entry.Name())
		}
	}
}

func TestReinstallingTheSameTagIsIdempotent(t *testing.T) {
	host := newHost(t)

	first := host.runUpdate(fixtureTag)
	if first.State != JobSucceeded {
		t.Fatalf("first update: %s %+v", first.State, first.Hold)
	}
	if err := host.store.Release(first.ID); err != nil {
		t.Fatalf("release: %v", err)
	}

	second := host.runUpdate(fixtureTag)
	if second.State != JobSucceeded {
		t.Fatalf("re-installing the same tag failed: %s %+v", second.State, second.Hold)
	}
	if second.ID == first.ID {
		t.Fatal("a second run after completion must be a new job")
	}
	// The installed tree is still exactly one tree, not a nested duplicate.
	release := filepath.Join(host.config.ReleasesDir(), fixtureTag)
	if _, err := os.Stat(filepath.Join(release, host.fixture.PackageRoot, "webtag")); err != nil {
		t.Fatalf("the installed tree is not intact: %v", err)
	}
}

func TestTheReaderRootBuildIsTheOneServedFromDisk(t *testing.T) {
	// The embedded build is served by the binary itself; only the root build is
	// unpacked to the directory Caddy serves.
	reader := releasetrust.ReaderArtifact{Builds: []releasetrust.ReaderBuild{
		{Name: "embedded", Directory: "embedded"},
		{Name: "root", Directory: "root"},
	}}
	if got := readerRootBuildDirectory(reader); got != "root" {
		t.Fatalf("expected the root build, got %q", got)
	}
}

// safeJoin compares strings, and a string comparison cannot see a symlink that
// was created after it ran. os.Root is what actually confines the write: the
// kernel refuses to follow a link out of the root. This is the property that
// matters for a process unpacking a downloaded archive as root, so it is
// asserted directly rather than inferred from safeJoin's return value.
func TestExtractionIsConfinedByTheKernelNotByStringComparison(t *testing.T) {
	t.Parallel()

	outside := t.TempDir()
	staging := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatalf("seed victim: %v", err)
	}

	// A link inside the staging root pointing out of it — exactly what an
	// archive would plant if it could, and what a path-prefix check waves
	// through because "staging/escape" is textually inside the root.
	if err := os.Symlink(outside, filepath.Join(staging, "escape")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	confined, err := os.OpenRoot(staging)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = confined.Close() }()

	// safeJoin is happy with this name: it never leaves the root textually.
	if _, err := safeJoin(staging, "escape/victim"); err != nil {
		t.Fatalf("safeJoin rejected a textually-contained name: %v", err)
	}

	// The kernel is not.
	if _, err := confined.OpenFile("escape/victim", os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		t.Fatal("writing through a symlink out of the root succeeded; extraction is not confined")
	}

	if contents, err := os.ReadFile(victim); err != nil || string(contents) != "original" {
		t.Fatalf("victim file was modified: contents=%q err=%v", contents, err)
	}
}
