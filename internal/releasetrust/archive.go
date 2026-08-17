package releasetrust

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// Archive limits. The helper unpacks release archives as root, so unpacking is
// treated as parsing hostile input: every entry is inspected before anything
// touches a filesystem, and the totals are bounded so a crafted archive cannot
// exhaust the staging disk.
const (
	// MaxArchiveEntries bounds the entry count of a release archive.
	MaxArchiveEntries = 20000
	// MaxArchiveBytes bounds the total uncompressed size of a release archive.
	MaxArchiveBytes = 1 << 30
	// MaxArchiveEntryBytes bounds any single member of a release archive.
	MaxArchiveEntryBytes = 512 << 20
)

// ArchiveEntry is one inspected member of a release archive.
type ArchiveEntry struct {
	Path       string
	Size       int64
	Mode       fs.FileMode
	SHA256     string
	Directory  bool
	Executable bool
}

// ArchiveContents is the fully inspected content of a release archive. Entries
// are sorted by path and every regular file already carries its digest, so the
// caller never has to re-read the archive to answer a question about it.
type ArchiveContents struct {
	Entries    []ArchiveEntry
	TotalBytes int64
}

// Entry looks a member up by its archive-relative path.
func (contents *ArchiveContents) Entry(name string) (ArchiveEntry, bool) {
	for _, entry := range contents.Entries {
		if entry.Path == name {
			return entry, true
		}
	}
	return ArchiveEntry{}, false
}

// ExecutablePaths lists every regular file that carries an executable bit.
func (contents *ArchiveContents) ExecutablePaths() []string {
	var paths []string
	for _, entry := range contents.Entries {
		if entry.Executable {
			paths = append(paths, entry.Path)
		}
	}
	slices.Sort(paths)
	return paths
}

// InspectArchive reads a gzip-compressed tar release archive and returns its
// contents. Anything that is not a plain file or directory is rejected: a
// symlink, hard link, device node, or FIFO in a root-unpacked archive is an
// escape primitive, not a packaging style.
//
//nolint:gocyclo // One branch per rejected archive shape; each is a named threat.
func InspectArchive(source io.Reader) (*ArchiveContents, error) {
	decompressor, err := gzip.NewReader(source)
	if err != nil {
		return nil, fmt.Errorf("archive is not gzip: %w", err)
	}
	defer func() { _ = decompressor.Close() }()

	contents := &ArchiveContents{}
	seen := map[string]struct{}{}
	reader := tar.NewReader(decompressor)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive entry: %w", err)
		}
		if len(contents.Entries) >= MaxArchiveEntries {
			return nil, fmt.Errorf("archive holds more than %d entries", MaxArchiveEntries)
		}
		name, err := cleanArchivePath(header.Name)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("archive repeats entry %q", name)
		}
		seen[name] = struct{}{}

		switch header.Typeflag {
		case tar.TypeDir:
			contents.Entries = append(contents.Entries, ArchiveEntry{
				Path:      name,
				Mode:      header.FileInfo().Mode(),
				Directory: true,
			})
			continue
		case tar.TypeReg:
		default:
			return nil, fmt.Errorf("archive entry %q is not a plain file or directory (tar type %q)",
				name, string(header.Typeflag))
		}
		if header.Size < 0 || header.Size > MaxArchiveEntryBytes {
			return nil, fmt.Errorf("archive entry %q declares %d bytes, over the %d byte limit",
				name, header.Size, int64(MaxArchiveEntryBytes))
		}
		if contents.TotalBytes+header.Size > MaxArchiveBytes {
			return nil, fmt.Errorf("archive expands past the %d byte limit", int64(MaxArchiveBytes))
		}
		digest := sha256.New()
		copied, err := io.Copy(digest, io.LimitReader(reader, MaxArchiveEntryBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read archive entry %q: %w", name, err)
		}
		if copied != header.Size {
			return nil, fmt.Errorf("archive entry %q holds %d bytes but declares %d", name, copied, header.Size)
		}
		mode := header.FileInfo().Mode()
		contents.TotalBytes += copied
		contents.Entries = append(contents.Entries, ArchiveEntry{
			Path:       name,
			Size:       copied,
			Mode:       mode,
			SHA256:     hex.EncodeToString(digest.Sum(nil)),
			Executable: mode.Perm()&0o111 != 0,
		})
	}
	slices.SortFunc(contents.Entries, func(left, right ArchiveEntry) int {
		return strings.Compare(left.Path, right.Path)
	})
	if len(contents.Entries) == 0 {
		return nil, errors.New("archive is empty")
	}
	return contents, nil
}

func cleanArchivePath(name string) (string, error) {
	trimmed := strings.TrimPrefix(name, "./")
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return "", fmt.Errorf("archive entry %q has an empty path", name)
	}
	if strings.HasPrefix(trimmed, "/") {
		return "", fmt.Errorf("archive entry %q uses an absolute path", name)
	}
	if strings.ContainsRune(trimmed, '\\') {
		return "", fmt.Errorf("archive entry %q uses a backslash path separator", name)
	}
	cleaned := path.Clean(trimmed)
	if cleaned != trimmed || cleaned == "." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("archive entry %q does not stay inside the archive", name)
	}
	return cleaned, nil
}

// VerifyArchiveBytes checks a downloaded archive against the signed size and
// digest before anything unpacks it.
func VerifyArchiveBytes(data []byte, expectedSHA256 string, expectedSize int64) error {
	if int64(len(data)) != expectedSize {
		return fmt.Errorf("archive is %d bytes, the signed manifest declares %d", len(data), expectedSize)
	}
	digest := sha256.Sum256(data)
	actual := hex.EncodeToString(digest[:])
	if actual != expectedSHA256 {
		return fmt.Errorf("archive sha256 %s does not match the signed manifest digest %s", actual, expectedSHA256)
	}
	return nil
}

// VerifyCoreArchiveContents checks an unpacked Core archive against its signed
// matrix entry: everything lives under the package root, the in-archive
// provenance is byte-identical to what was signed, both executables hash to
// their signed digests, the frozen legal closure is present, and the archive
// carries no executable file beyond the two declared programs.
//
//nolint:gocyclo // One branch per in-archive rule; each maps to a named stop condition.
func VerifyCoreArchiveContents(contents *ArchiveContents, artifact CoreArtifact) error {
	if contents == nil {
		return errors.New("core archive contents are missing")
	}
	prefix := artifact.PackageRoot + "/"
	legalFiles := 0
	for _, entry := range contents.Entries {
		if entry.Path != artifact.PackageRoot && !strings.HasPrefix(entry.Path, prefix) {
			return fmt.Errorf("core archive entry %q is outside package root %q", entry.Path, artifact.PackageRoot)
		}
		if strings.HasPrefix(entry.Path, prefix+"legal/") && !entry.Directory {
			legalFiles++
		}
	}
	if legalFiles == 0 {
		return fmt.Errorf("core archive %s carries no legal materials", artifact.Archive)
	}

	provenance, ok := contents.Entry(artifact.ProvenancePath)
	if !ok {
		return fmt.Errorf("core archive %s is missing %s", artifact.Archive, artifact.ProvenancePath)
	}
	if provenance.SHA256 != artifact.ProvenanceSHA256 {
		return fmt.Errorf("core archive %s provenance sha256 %s does not match the signed manifest",
			artifact.Archive, provenance.SHA256)
	}

	declared := make([]string, 0, len(artifact.Executables))
	for name, executable := range artifact.Executables {
		entry, found := contents.Entry(executable.Path)
		if !found {
			return fmt.Errorf("core archive %s is missing executable %s", artifact.Archive, executable.Path)
		}
		if !entry.Executable {
			return fmt.Errorf("core archive %s entry %s is not executable", artifact.Archive, executable.Path)
		}
		if entry.SHA256 != executable.SHA256 {
			return fmt.Errorf("core archive %s executable %s hashes to %s, the signed manifest declares %s",
				artifact.Archive, name, entry.SHA256, executable.SHA256)
		}
		declared = append(declared, executable.Path)
	}
	slices.Sort(declared)
	if actual := contents.ExecutablePaths(); !slices.Equal(actual, declared) {
		return fmt.Errorf("core archive %s carries executables %v, the signed manifest declares %v",
			artifact.Archive, actual, declared)
	}
	return nil
}

// VerifyReaderArchiveContents checks an unpacked Reader archive against its
// signed entry. The Reader tree is static assets served by Caddy, so any
// executable file in it is a finding, not a build artifact.
func VerifyReaderArchiveContents(contents *ArchiveContents, reader ReaderArtifact) error {
	if contents == nil {
		return errors.New("reader archive contents are missing")
	}
	provenance, ok := contents.Entry(reader.ProvenancePath)
	if !ok {
		return fmt.Errorf("reader archive %s is missing %s", reader.Archive, reader.ProvenancePath)
	}
	if provenance.SHA256 != reader.ProvenanceSHA256 {
		return fmt.Errorf("reader archive %s provenance sha256 %s does not match the signed manifest",
			reader.Archive, provenance.SHA256)
	}
	if executables := contents.ExecutablePaths(); len(executables) != 0 {
		return fmt.Errorf("reader archive %s carries executable files %v", reader.Archive, executables)
	}
	for _, build := range reader.Builds {
		prefix := build.Directory + "/"
		var files int64
		var total int64
		for _, entry := range contents.Entries {
			if entry.Directory || !strings.HasPrefix(entry.Path, prefix) {
				continue
			}
			files++
			total += entry.Size
		}
		if files != build.FileCount || total != build.TotalBytes {
			return fmt.Errorf("reader archive %s build %s holds %d files / %d bytes, the signed manifest declares %d / %d",
				reader.Archive, build.Name, files, total, build.FileCount, build.TotalBytes)
		}
	}
	return nil
}

// ReadArchiveFile returns the bytes of one member of an archive. It is used to
// re-read small provenance documents after inspection without keeping the whole
// archive in memory twice.
func ReadArchiveFile(source io.Reader, name string) ([]byte, error) {
	decompressor, err := gzip.NewReader(source)
	if err != nil {
		return nil, fmt.Errorf("archive is not gzip: %w", err)
	}
	defer func() { _ = decompressor.Close() }()
	reader := tar.NewReader(decompressor)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("archive does not contain %q", name)
		}
		if err != nil {
			return nil, fmt.Errorf("read archive entry: %w", err)
		}
		cleaned, err := cleanArchivePath(header.Name)
		if err != nil {
			return nil, err
		}
		if cleaned != name || header.Typeflag != tar.TypeReg {
			continue
		}
		var out bytes.Buffer
		if _, err := io.Copy(&out, io.LimitReader(reader, MaxArchiveEntryBytes+1)); err != nil {
			return nil, fmt.Errorf("read archive entry %q: %w", name, err)
		}
		if int64(out.Len()) > MaxArchiveEntryBytes {
			return nil, fmt.Errorf("archive entry %q is over the %d byte limit", name, int64(MaxArchiveEntryBytes))
		}
		return out.Bytes(), nil
	}
}
