package releasetrust

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Release fixtures.
//
// The whole trust path is exercised against archives built in memory rather
// than against a recorded Release. A recorded Release would freeze one set of
// digests and quietly stop covering the tamper cases the moment anything about
// packaging changed; building the bytes here means every threat test can
// produce the *exact* corruption it claims to test and then prove which check
// rejected it.

const (
	testRepo      = "Alpenl/cairn"
	testTag       = "v1.2.3"
	testVersion   = "1.2.3"
	testCommit    = "0123456789abcdef0123456789abcdef01234567"
	testBuildTime = "2026-08-14T01:02:03Z"
)

type archiveFile struct {
	path string
	data []byte
	mode int64
}

func buildTarGz(t *testing.T, files []archiveFile) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	writer := tar.NewWriter(gzipWriter)
	for _, file := range files {
		header := &tar.Header{
			Name:     file.path,
			Mode:     file.mode,
			Size:     int64(len(file.data)),
			Typeflag: tar.TypeReg,
		}
		if strings.HasSuffix(file.path, "/") {
			header.Typeflag = tar.TypeDir
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("write tar header %s: %v", file.path, err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write(file.data); err != nil {
				t.Fatalf("write tar body %s: %v", file.path, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return compressed.Bytes()
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// testRelease is one synthetic release: the archives, the canonical manifest,
// and a detached signature under a trust root installed for the test.
type testRelease struct {
	Manifest      Manifest
	ManifestBytes []byte
	Signature     Signature
	SigBytes      []byte
	CoreArchives  map[string][]byte
	ReaderArchive []byte
	SigningKey    ed25519.PrivateKey
	KeyID         string
}

// installTestTrustRoot swaps the compiled-in trust root for the duration of one
// test. Tests that call it must not run in parallel: the trust root is package
// state on purpose, because a helper that can be handed a key set at runtime is
// a helper whose trust root an attacker can hand it too.
func installTestTrustRoot(t *testing.T, keys []TrustedKey) {
	t.Helper()
	original := trustedKeys
	trustedKeys = keys
	t.Cleanup(func() { trustedKeys = original })
}

func newTestSigningKey(t *testing.T, keyID string) (ed25519.PrivateKey, TrustedKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index) ^ keyID[len(keyID)-1]
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("generated key is not Ed25519")
	}
	return privateKey, TrustedKey{
		KeyID:     keyID,
		PublicKey: publicKey,
		NotBefore: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Note:      "synthetic key for tests",
	}
}

func coreArchiveFiles(architecture string) (string, []archiveFile) {
	packageRoot := fmt.Sprintf("cairn_%s_linux_%s", testVersion, architecture)
	webtag := []byte("ELF-webtag-" + architecture)
	migrate := []byte("ELF-migrate-" + architecture)
	provenance := []byte(fmt.Sprintf(`{"version":%q,"commit":%q,"build_time":%q,"source_state":"clean",`+
		`"os":"linux","arch":%q,"binaries":{"webtag":{"sha256":%q},"migrate":{"sha256":%q}}}`,
		testVersion, testCommit, testBuildTime, architecture, sha256Hex(webtag), sha256Hex(migrate)))
	return packageRoot, []archiveFile{
		{path: packageRoot + "/", mode: 0o755},
		{path: packageRoot + "/BUILD-PROVENANCE.json", data: provenance, mode: 0o644},
		{path: packageRoot + "/legal/", mode: 0o755},
		{path: packageRoot + "/legal/CAIRN_LICENSE.txt", data: []byte("license"), mode: 0o644},
		{path: packageRoot + "/migrate", data: migrate, mode: 0o755},
		{path: packageRoot + "/webtag", data: webtag, mode: 0o755},
	}
}

func readerArchiveFiles() []archiveFile {
	builds := map[string][]archiveFile{
		"embedded": {
			{path: "embedded/index.html", data: []byte("<script src=/reader/assets/app.js></script>"), mode: 0o644},
			{path: "embedded/assets/app.js", data: []byte("embedded-bundle"), mode: 0o644},
			{path: "embedded/sw.js", data: []byte("/api"), mode: 0o644},
		},
		"root": {
			{path: "root/index.html", data: []byte("<script src=/assets/app.js></script>"), mode: 0o644},
			{path: "root/assets/app.js", data: []byte("root-bundle"), mode: 0o644},
			{path: "root/sw.js", data: []byte("/api"), mode: 0o644},
		},
	}
	basePaths := map[string]string{"embedded": "/reader/", "root": "/"}

	var descriptions []string
	var files []archiveFile
	for _, name := range []string{"embedded", "root"} {
		var total int64
		for _, file := range builds[name] {
			total += int64(len(file.data))
		}
		descriptions = append(descriptions, fmt.Sprintf(
			`{"base_path":%q,"directory":%q,"file_count":%d,"name":%q,"total_bytes":%d}`,
			basePaths[name], name, len(builds[name]), name, total))
		files = append(files, builds[name]...)
	}
	provenance := []byte(fmt.Sprintf(
		`{"artifact_kind":"reader-vnext-release-manifest","builds":[%s],"commit_full_sha":%q,`+
			`"generated_at":%q,"release_id":"reader-%s","schema_version":1}`,
		strings.Join(descriptions, ","), testCommit, testBuildTime, testVersion))
	files = append(files, archiveFile{path: ReaderProvenanceFileName, data: provenance, mode: 0o644})
	slices.SortFunc(files, func(left, right archiveFile) int { return strings.Compare(left.path, right.path) })
	return files
}

// newTestRelease builds a complete, internally consistent, signed release.
func newTestRelease(t *testing.T) *testRelease {
	t.Helper()
	privateKey, trusted := newTestSigningKey(t, "cairn-release-2026t")
	installTestTrustRoot(t, []TrustedKey{trusted})

	release := &testRelease{
		CoreArchives: map[string][]byte{},
		SigningKey:   privateKey,
		KeyID:        trusted.KeyID,
	}
	manifest := Manifest{
		SchemaVersion:          ManifestSchemaVersion,
		ArtifactKind:           ManifestArtifactKind,
		Repo:                   testRepo,
		Tag:                    testTag,
		Version:                testVersion,
		Commit:                 testCommit,
		BuildTime:              testBuildTime,
		MinimumHelperProtocol:  HelperProtocol,
		SchemaTarget:           "readertodoprojection2026081701",
		RiverLedgerTarget:      7,
		OnlineUpdateCompatible: false,
		OnlineUpdateReason:     "migration steps carry no online-update classification yet",
		RollbackCompatible:     false,
		RollbackReason:         "the previous release's migration targets were not supplied",
	}
	for _, architecture := range []string{"amd64", "arm64"} {
		packageRoot, files := coreArchiveFiles(architecture)
		archive := buildTarGz(t, files)
		name := packageRoot + ".tar.gz"
		release.CoreArchives[name] = archive

		contents, err := InspectArchive(bytes.NewReader(archive))
		if err != nil {
			t.Fatalf("inspect fixture archive: %v", err)
		}
		provenance, _ := contents.Entry(packageRoot + "/BUILD-PROVENANCE.json")
		artifact := CoreArtifact{
			OS:               ReleaseOS,
			Arch:             architecture,
			Archive:          name,
			SHA256:           sha256Hex(archive),
			SizeBytes:        int64(len(archive)),
			PackageRoot:      packageRoot,
			ProvenancePath:   packageRoot + "/BUILD-PROVENANCE.json",
			ProvenanceSHA256: provenance.SHA256,
			Executables:      map[string]Executable{},
		}
		for _, executable := range ExecutableNames {
			entry, _ := contents.Entry(packageRoot + "/" + executable)
			artifact.Executables[executable] = Executable{
				Path:   packageRoot + "/" + executable,
				SHA256: entry.SHA256,
				Identity: Identity{
					Version:       testVersion,
					Commit:        testCommit,
					BuildTime:     testBuildTime,
					VersionOutput: ExpectedVersionOutput(testVersion, testCommit, testBuildTime),
				},
			}
		}
		manifest.Core = append(manifest.Core, artifact)
		manifest.Platforms = append(manifest.Platforms, artifact.Platform())
	}

	readerArchive := buildTarGz(t, readerArchiveFiles())
	release.ReaderArchive = readerArchive
	readerContents, err := InspectArchive(bytes.NewReader(readerArchive))
	if err != nil {
		t.Fatalf("inspect fixture reader archive: %v", err)
	}
	readerProvenance, _ := readerContents.Entry(ReaderProvenanceFileName)
	manifest.Reader = ReaderArtifact{
		Archive:          fmt.Sprintf("cairn-reader-%s.tar.gz", testVersion),
		SHA256:           sha256Hex(readerArchive),
		SizeBytes:        int64(len(readerArchive)),
		ReleaseID:        "reader-" + testVersion,
		Commit:           testCommit,
		ProvenancePath:   ReaderProvenanceFileName,
		ProvenanceSHA256: readerProvenance.SHA256,
		Builds: []ReaderBuild{
			{Name: "embedded", BasePath: "/reader/", Directory: "embedded", FileCount: 3,
				TotalBytes: int64(len("<script src=/reader/assets/app.js></script>") + len("embedded-bundle") + len("/api"))},
			{Name: "root", BasePath: "/", Directory: "root", FileCount: 3,
				TotalBytes: int64(len("<script src=/assets/app.js></script>") + len("root-bundle") + len("/api"))},
		},
	}

	release.Manifest = manifest
	release.sign(t, manifest)
	return release
}

// sign re-signs the release after a test mutated the manifest.
func (release *testRelease) sign(t *testing.T, manifest Manifest) {
	t.Helper()
	canonical, err := manifest.Canonical()
	if err != nil {
		t.Fatalf("canonicalise fixture manifest: %v", err)
	}
	signature, err := SignManifest(canonical, release.SigningKey)
	if err != nil {
		t.Fatalf("sign fixture manifest: %v", err)
	}
	signatureBytes, err := signature.Canonical()
	if err != nil {
		t.Fatalf("canonicalise fixture signature: %v", err)
	}
	release.Manifest = manifest
	release.ManifestBytes = canonical
	release.Signature = signature
	release.SigBytes = signatureBytes
}

// request is the verification request a healthy helper on linux/amd64 makes.
func (release *testRelease) request() VerifyRequest {
	return VerifyRequest{
		ManifestBytes:  release.ManifestBytes,
		SignatureBytes: release.SigBytes,
		ExpectedRepo:   testRepo,
		ExpectedTag:    testTag,
		HostOS:         ReleaseOS,
		HostArch:       "amd64",
		HelperProtocol: HelperProtocol,
	}
}

// checksums renders the SHA256SUMS the Release ships alongside the archives.
func (release *testRelease) checksums() []byte {
	var out bytes.Buffer
	names := make([]string, 0, len(release.CoreArchives))
	for name := range release.CoreArchives {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(&out, "%s  %s\n", sha256Hex(release.CoreArchives[name]), name)
	}
	fmt.Fprintf(&out, "%s  %s\n", sha256Hex(release.ReaderArchive), release.Manifest.Reader.Archive)
	return out.Bytes()
}

func newReader(data []byte) *bytes.Reader { return bytes.NewReader(data) }
