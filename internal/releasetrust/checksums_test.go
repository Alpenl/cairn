package releasetrust

import "testing"

// Signing has to happen before SHA256SUMS is written, so that the list can
// cover the manifest and its signature. At that moment an artifact the list has
// not reached yet is ordering, not inconsistency — but a digest that disagrees
// is inconsistency at any moment, and must not be waved through with it.
func TestListedCrossCheckToleratesAnUnsealedListButNotADisagreement(t *testing.T) {
	// Not parallel: newTestRelease installs a test trust root, which is package
	// state, and installTestTrustRoot says so. Racing it against another test
	// reading TrustedKeys is exactly what -race caught here.
	manifest := newTestRelease(t).Manifest
	readerName := manifest.Reader.Archive
	coreName := manifest.Core[0].Archive

	t.Run("an entry the list has not reached yet is tolerated", func(t *testing.T) {
		partial := map[string]string{coreName: manifest.Core[0].SHA256}
		if err := CrossCheckListedChecksums(manifest, partial); err != nil {
			t.Fatalf("lenient cross-check rejected a list that is merely incomplete: %v", err)
		}
		// The strict form is what runs after sealing, and it must still object.
		if err := CrossCheckChecksums(manifest, partial); err == nil {
			t.Fatal("strict cross-check accepted a list missing an artifact")
		}
	})

	t.Run("a listed digest that disagrees is fatal in both forms", func(t *testing.T) {
		wrong := map[string]string{
			coreName:   manifest.Core[0].SHA256,
			readerName: "00000000000000000000000000000000000000000000000000000000000000ff",
		}
		if err := CrossCheckListedChecksums(manifest, wrong); err == nil {
			t.Fatal("lenient cross-check accepted a digest that disagrees with the manifest")
		}
		if err := CrossCheckChecksums(manifest, wrong); err == nil {
			t.Fatal("strict cross-check accepted a digest that disagrees with the manifest")
		}
	})
}
