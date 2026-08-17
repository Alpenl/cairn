package releasetrust

import (
	"bytes"
	"testing"
)

// `tar czf - -C dir .` writes the archive root as its own entry. The Reader
// release tarball is built exactly that way, so refusing it refused every real
// Reader archive — which is how this reached a release pipeline.
func TestArchiveRootEntryIsSkippedButAFileNamedDotIsNot(t *testing.T) {
	t.Parallel()

	t.Run("root directory entry is skipped", func(t *testing.T) {
		t.Parallel()
		for _, root := range []string{"./", "root/"} {
			archive := buildTarGz(t, []archiveFile{
				{path: root, mode: 0o755},
				{path: "root/index.html", data: []byte("<!doctype html>"), mode: 0o644},
			})
			contents, err := InspectArchive(bytes.NewReader(archive))
			if err != nil {
				t.Fatalf("InspectArchive rejected an archive whose root entry is %q: %v", root, err)
			}
			for _, entry := range contents.Entries {
				if entry.Path == "" {
					t.Fatalf("root entry %q survived as an empty path", root)
				}
			}
		}
	})

	// A *file* called "." is not a root marker, it is malformed: it would be
	// extracted, and it has nowhere to go. Skipping it because the name matches
	// would turn the fix above into a hole.
	t.Run("a plain file named dot is still refused", func(t *testing.T) {
		t.Parallel()
		for _, name := range []string{".", "./"} {
			archive := buildTarGz(t, []archiveFile{{path: name, data: []byte("payload"), mode: 0o644}})
			if _, err := InspectArchive(bytes.NewReader(archive)); err == nil {
				t.Fatalf("InspectArchive accepted a plain file named %q", name)
			}
		}
	})
}
