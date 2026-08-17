package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"webtag/internal/releasetrust"
)

// Unpacking a verified archive.
//
// releasetrust.InspectArchive has already read the whole archive once and
// rejected everything that is not a plain file or directory, everything that
// escapes the archive root, and everything over the size ceilings; and
// VerifyCoreArchive has already matched the exact bytes against a signed
// digest. So the archive reaching this file is known-good.
//
// It is still unpacked against the inspected contents rather than replayed
// blindly. The reason is narrow and worth stating: the verified thing is the
// byte string, and the thing being written is a second traversal of that byte
// string by a different decoder path. Cross-checking each member against the
// entry list turns any disagreement between the two traversals — a tar reader
// quirk, a future refactor of either side — into a refusal instead of a file on
// disk that nobody verified. Extraction runs as root; the cost of the check is
// a map lookup.

// extractArchive writes a verified archive into a fresh directory.
func extractArchive(data []byte, contents *releasetrust.ArchiveContents, destination string) error {
	expected := make(map[string]releasetrust.ArchiveEntry, len(contents.Entries))
	for _, entry := range contents.Entries {
		expected[entry.Path] = entry
	}
	// 0o755 rather than the 0o750 gosec prefers: Caddy serves the Reader tree as
	// its own user and systemd resolves ExecStart through the Core tree, so both
	// need traverse access. Nothing here is group- or world-writable, which is
	// the property the permission model actually depends on.
	if err := os.MkdirAll(destination, 0o755); err != nil { //nolint:gosec // Deployment trees must be traversable by caddy and systemd; they are never writable by anyone but root.
		return fmt.Errorf("create %s: %w", destination, err)
	}
	root, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", destination, err)
	}

	decompressor, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("archive is not gzip: %w", err)
	}
	defer func() { _ = decompressor.Close() }()

	written := 0
	reader := tar.NewReader(decompressor)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive entry: %w", err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(header.Name, "./"), "/")
		entry, known := expected[name]
		if !known {
			return fmt.Errorf("archive member %q was not present when the archive was verified", header.Name)
		}
		target, err := safeJoin(root, name)
		if err != nil {
			return err
		}
		if err := extractEntry(reader, header, entry, target); err != nil {
			return err
		}
		written++
	}
	if written != len(contents.Entries) {
		return fmt.Errorf("unpacked %d members but the verified archive holds %d", written, len(contents.Entries))
	}
	return nil
}

func extractEntry(reader io.Reader, header *tar.Header, entry releasetrust.ArchiveEntry, target string) error {
	if entry.Directory {
		if header.Typeflag != tar.TypeDir {
			return fmt.Errorf("archive member %q changed type between verification and unpacking", entry.Path)
		}
		if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // See extractArchive: deployment trees are read-and-traverse for other users, never writable.
			return fmt.Errorf("create %s: %w", target, err)
		}
		return os.Chmod(target, 0o755) //nolint:gosec // See extractArchive: deployment trees are read-and-traverse for other users, never writable.
	}
	if header.Typeflag != tar.TypeReg {
		return fmt.Errorf("archive member %q changed type between verification and unpacking", entry.Path)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // See extractArchive: deployment trees are read-and-traverse for other users, never writable.
		return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
	}
	// The mode is decided here rather than copied from the header. The signed
	// manifest says which members are executables and the archive verification
	// already refused any executable bit that was not declared, so reproducing
	// an arbitrary header mode would only add ways to write a setuid file.
	mode := os.FileMode(0o644)
	if entry.Executable {
		mode = 0o755
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // target is confined to the staging root by safeJoin.
	if err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	defer func() { _ = file.Close() }()

	digest := sha256.New()
	copied, err := io.Copy(io.MultiWriter(file, digest), io.LimitReader(reader, entry.Size))
	if err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	if copied != entry.Size {
		return fmt.Errorf("archive member %q holds %d bytes but verification recorded %d", entry.Path, copied, entry.Size)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != entry.SHA256 {
		return fmt.Errorf("archive member %q hashed to %s on unpacking but %s on verification", entry.Path, got, entry.SHA256)
	}
	// The mode is set again because OpenFile applies the process umask, and a
	// binary that came out 0o644 would fail to start with a confusing
	// permission error long after this step.
	return os.Chmod(target, mode)
}

// safeJoin resolves an archive-relative path inside root and refuses anything
// that would land outside it.
func safeJoin(root, name string) (string, error) {
	joined := filepath.Join(root, name)
	if joined != root && !strings.HasPrefix(joined, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive member %q resolves outside the staging root", name)
	}
	return joined, nil
}

// switchSymlink atomically repoints a `current` symlink.
//
// A symlink cannot be rewritten in place, and remove-then-create leaves a
// window where `current` does not exist — which for the Reader means Caddy
// serving 404 to every request and for the Core means systemd failing to
// resolve ExecStart on a restart that happens to land in the gap. Creating a
// second symlink and renaming it over the first is a single atomic directory
// operation with no such window.
func switchSymlink(link, target string) error {
	dir := filepath.Dir(link)
	temp, err := os.CreateTemp(dir, ".switch-*")
	if err != nil {
		return fmt.Errorf("create temporary link in %s: %w", dir, err)
	}
	tempName := temp.Name()
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tempName, err)
	}
	// CreateTemp made a regular file to reserve the name; the symlink has to
	// replace it because os.Symlink refuses an existing path.
	if err := os.Remove(tempName); err != nil {
		return fmt.Errorf("clear %s: %w", tempName, err)
	}
	if err := os.Symlink(target, tempName); err != nil {
		return fmt.Errorf("link %s to %s: %w", tempName, target, err)
	}
	if err := os.Rename(tempName, link); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("move %s into place as %s: %w", tempName, link, err)
	}
	return syncDir(dir)
}

// readSymlink returns the current target of a `current` link, or "" when it
// does not exist yet. A missing link is the first-install case, not an error.
func readSymlink(link string) string {
	target, err := os.Readlink(link)
	if err != nil {
		return ""
	}
	return target
}

// freeBytes reports the space available to the helper on the filesystem holding
// path. It is checked in preflight so a dump that cannot fit fails before the
// service is stopped rather than halfway through writing a truncated backup.
func freeBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("stat filesystem at %s: %w", path, err)
	}
	//nolint:gosec // Bavail and Bsize are non-negative sizes; the product is bounded by the device.
	return int64(stat.Bavail) * stat.Bsize, nil
}

// removeAllIfPresent clears a staging path that a previous attempt left behind.
func removeAllIfPresent(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("clear %s: %w", path, err)
	}
	return nil
}
