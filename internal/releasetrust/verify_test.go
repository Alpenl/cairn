package releasetrust

import (
	"strings"
	"testing"
)

// The healthy path, end to end: this is what the helper does between
// "operator confirmed an exact tag" and "the maintenance window opens".
func TestHealthyReleaseVerifiesEndToEnd(t *testing.T) {
	release := newTestRelease(t)

	verified, err := VerifyRelease(release.request())
	if err != nil {
		t.Fatalf("verify release: %v", err)
	}
	if verified.Core.Arch != "amd64" || verified.Key.KeyID != release.KeyID {
		t.Fatalf("verified %s under %s, want amd64 under %s", verified.Core.Arch, verified.Key.KeyID, release.KeyID)
	}

	archive := release.CoreArchives[verified.Core.Archive]
	contents, err := VerifyCoreArchive(archive, verified.Core)
	if err != nil {
		t.Fatalf("verify core archive: %v", err)
	}
	if _, ok := contents.Entry(verified.Core.ProvenancePath); !ok {
		t.Fatal("inspected contents do not expose the provenance document")
	}

	provenance, err := ReadArchiveFile(newReader(archive), verified.Core.ProvenancePath)
	if err != nil {
		t.Fatalf("read provenance: %v", err)
	}
	if err := VerifyCoreProvenance(provenance, verified.Manifest, verified.Core); err != nil {
		t.Fatalf("verify provenance: %v", err)
	}

	for _, name := range ExecutableNames {
		output := []byte(ExpectedVersionOutput(testVersion, testCommit, testBuildTime) + "\n")
		if err := VerifyExecutableIdentity(name, output, verified.Core); err != nil {
			t.Fatalf("verify %s identity: %v", name, err)
		}
	}

	if _, err := VerifyReaderArchive(release.ReaderArchive, verified.Manifest.Reader); err != nil {
		t.Fatalf("verify reader archive: %v", err)
	}

	checksums, err := ParseChecksums(release.checksums())
	if err != nil {
		t.Fatalf("parse checksums: %v", err)
	}
	if err := CrossCheckChecksums(verified.Manifest, checksums); err != nil {
		t.Fatalf("cross-check SHA256SUMS: %v", err)
	}
}

// The in-archive provenance must describe *this* release, not merely be the
// document that happened to be signed. A hash match alone would accept a
// correctly signed manifest that points at another clean build's provenance.
func TestCoreProvenanceMustDescribeTheReleaseItShipsWith(t *testing.T) {
	release := newTestRelease(t)
	artifact, err := release.Manifest.CoreFor("linux", "amd64")
	if err != nil {
		t.Fatalf("select platform: %v", err)
	}

	healthy, err := ReadArchiveFile(newReader(release.CoreArchives[artifact.Archive]), artifact.ProvenancePath)
	if err != nil {
		t.Fatalf("read provenance: %v", err)
	}
	if err := VerifyCoreProvenance(healthy, release.Manifest, artifact); err != nil {
		t.Fatalf("healthy provenance rejected: %v", err)
	}

	for name, replacement := range map[string][2]string{
		"another version":      {`"version":"1.2.3"`, `"version":"1.2.4"`},
		"another commit":       {testCommit, strings.Repeat("b", 40)},
		"another build time":   {testBuildTime, "2020-01-01T00:00:00Z"},
		"dirty source tree":    {`"source_state":"clean"`, `"source_state":"dirty"`},
		"another architecture": {`"arch":"amd64"`, `"arch":"arm64"`},
	} {
		tampered := strings.Replace(string(healthy), replacement[0], replacement[1], 1)
		if tampered == string(healthy) {
			t.Fatalf("%s: fixture does not contain %q", name, replacement[0])
		}
		if err := VerifyCoreProvenance([]byte(tampered), release.Manifest, artifact); err == nil {
			t.Errorf("%s provenance was accepted", name)
		}
	}

	if err := VerifyCoreProvenance([]byte(`{"version":"1.2.3","unexpected":true}`),
		release.Manifest, artifact); err == nil {
		t.Error("a provenance document with unknown members was accepted")
	}
}

// Executable identity is compared byte for byte against the signed string.
func TestExecutableIdentityIsComparedAgainstTheSignedOutput(t *testing.T) {
	release := newTestRelease(t)
	artifact, err := release.Manifest.CoreFor("linux", "amd64")
	if err != nil {
		t.Fatalf("select platform: %v", err)
	}

	expected := ExpectedVersionOutput(testVersion, testCommit, testBuildTime)
	if err := VerifyExecutableIdentity("webtag", []byte(expected), artifact); err != nil {
		t.Fatalf("identity without a trailing newline was rejected: %v", err)
	}
	for name, output := range map[string]string{
		"another commit":  ExpectedVersionOutput(testVersion, strings.Repeat("c", 40), testBuildTime),
		"another version": ExpectedVersionOutput("1.2.4", testCommit, testBuildTime),
		"truncated":       "cairn 1.2.3",
		"decorated":       expected + "\nwith extra output",
		"empty":           "",
	} {
		if err := VerifyExecutableIdentity("webtag", []byte(output), artifact); err == nil {
			t.Errorf("%s identity was accepted", name)
		}
	}
	if err := VerifyExecutableIdentity("debug-shell", []byte(expected), artifact); err == nil {
		t.Error("an undeclared executable name was accepted")
	}
}

// SHA256SUMS must at least agree with the signed manifest when it is present.
func TestChecksumCrossCheckReportsMissingAndDisagreeingEntries(t *testing.T) {
	release := newTestRelease(t)

	healthy, err := ParseChecksums(release.checksums())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := CrossCheckChecksums(release.Manifest, healthy); err != nil {
		t.Fatalf("healthy list rejected: %v", err)
	}

	missing := map[string]string{}
	for name, digest := range healthy {
		missing[name] = digest
	}
	delete(missing, release.Manifest.Reader.Archive)
	if err := CrossCheckChecksums(release.Manifest, missing); err == nil {
		t.Error("a checksum list missing the Reader archive was accepted")
	}

	disagreeing := map[string]string{}
	for name, digest := range healthy {
		disagreeing[name] = digest
	}
	disagreeing["cairn_1.2.3_linux_arm64.tar.gz"] = sha256Hex([]byte("other"))
	if err := CrossCheckChecksums(release.Manifest, disagreeing); err == nil {
		t.Error("a checksum list disagreeing with the signed manifest was accepted")
	}
}

// The archive inspector bounds what it will unpack. The helper runs as root, so
// "the archive said it was one file" is not a reason to write a gigabyte.
func TestArchiveInspectionRejectsUnsafeShapes(t *testing.T) {
	release := newTestRelease(t)
	artifact, err := release.Manifest.CoreFor("linux", "amd64")
	if err != nil {
		t.Fatalf("select platform: %v", err)
	}

	if _, err := InspectArchive(newReader([]byte("not a gzip stream"))); err == nil {
		t.Error("a non-gzip payload was inspected")
	}
	if _, err := InspectArchive(newReader(buildTarGz(t, nil))); err == nil {
		t.Error("an empty archive was accepted")
	}

	_, files := coreArchiveFiles("amd64")
	duplicated := buildTarGz(t, append(files, files[len(files)-1]))
	if _, err := InspectArchive(newReader(duplicated)); err == nil {
		t.Error("an archive repeating an entry was accepted")
	}

	// A package that simply lacks the legal closure is not installable.
	var withoutLegal []archiveFile
	for _, file := range files {
		if strings.Contains(file.path, "/legal/") {
			continue
		}
		withoutLegal = append(withoutLegal, file)
	}
	contents, err := InspectArchive(newReader(buildTarGz(t, withoutLegal)))
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if err := VerifyCoreArchiveContents(contents, artifact); err == nil {
		t.Error("a package without legal materials was accepted")
	}
}
