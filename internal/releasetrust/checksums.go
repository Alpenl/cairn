package releasetrust

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// SHA256SUMS handling.
//
// SHA256SUMS ships in the same GitHub Release as the archives it describes, so
// whoever can replace an asset can replace the checksum list in the same
// motion. It is therefore kept strictly as a corruption / wrong-file check and
// is never consulted for authenticity — that is what the signed manifest is
// for. ParseChecksums exists mainly so a *malformed* list is a loud failure
// instead of a silently skipped check.
//
// The one property worth enforcing hard is uniqueness. A list that names the
// same file twice is ambiguous: a reader that keeps the first entry and a
// reader that keeps the last entry disagree about what "verified" means, and an
// attacker gets to pick which reader is right. Both entries are rejected
// instead.

// ParseChecksums reads GNU coreutils sha256sum output. Duplicate file names are
// an error, as are non-lowercase or malformed digests.
func ParseChecksums(data []byte) (map[string]string, error) {
	checksums := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if strings.TrimSpace(text) == "" {
			continue
		}
		digest, name, found := strings.Cut(text, " ")
		if !found {
			return nil, fmt.Errorf("SHA256SUMS line %d is not <digest> <name>", line)
		}
		// coreutils writes two spaces for text mode and " *" for binary mode.
		name = strings.TrimPrefix(name, " ")
		name = strings.TrimPrefix(name, "*")
		if !sha256Pattern.MatchString(digest) {
			return nil, fmt.Errorf("SHA256SUMS line %d does not start with a lowercase hex digest", line)
		}
		if name == "" || strings.ContainsAny(name, "/\\") {
			return nil, fmt.Errorf("SHA256SUMS line %d names %q, which is not a plain Release asset name", line, name)
		}
		if previous, duplicate := checksums[name]; duplicate {
			return nil, fmt.Errorf("SHA256SUMS lists %s twice (%s and %s): the list is ambiguous",
				name, previous, digest)
		}
		checksums[name] = digest
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read SHA256SUMS: %w", err)
	}
	if len(checksums) == 0 {
		return nil, fmt.Errorf("SHA256SUMS lists no files")
	}
	return checksums, nil
}

// CrossCheckChecksums compares SHA256SUMS against the signed manifest. A
// mismatch means the Release is internally inconsistent, which is a stop
// condition even though the manifest is the one that decides authenticity.
func CrossCheckChecksums(manifest Manifest, checksums map[string]string) error {
	expected := map[string]string{manifest.Reader.Archive: manifest.Reader.SHA256}
	for _, artifact := range manifest.Core {
		expected[artifact.Archive] = artifact.SHA256
	}
	for name, digest := range expected {
		listed, ok := checksums[name]
		if !ok {
			return fmt.Errorf("SHA256SUMS does not list %s", name)
		}
		if listed != digest {
			return fmt.Errorf("SHA256SUMS digest for %s disagrees with the signed manifest", name)
		}
	}
	return nil
}
