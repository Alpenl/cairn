package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"webtag/internal/releasetrust"
)

// The fixture builds a real release: real gzip tar archives, real executables
// that answer --version, a real canonical manifest, and a real Ed25519
// signature over the exact manifest bytes.
//
// The one thing it cannot do is sign under a key the compiled-in trust root
// accepts, because that root is private to internal/releasetrust and the
// production private key exists only in a workflow secret. So the happy path
// runs through testTrust, which performs every real check — canonical form,
// full manifest validation, Ed25519 over the received bytes, protocol floor,
// architecture selection — against a synthetic key. Failure paths do not use
// it: a tampered manifest or a broken signature fails under productionTrust
// regardless of key, so those tests drive the real verifier.

const (
	fixtureRepo      = "Alpenl/cairn"
	fixtureTag       = "v1.2.3"
	fixtureVersion   = "1.2.3"
	fixtureCommit    = "0123456789abcdef0123456789abcdef01234567"
	fixtureBuildTime = "2026-08-14T01:02:03Z"
	fixtureSchema    = "readertodoprojection2026081701"
	fixtureRiver     = 7
)

type archiveFile struct {
	path string
	data []byte
	mode int64
	dir  bool
}

func buildTarGz(t *testing.T, files []archiveFile) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	writer := tar.NewWriter(gzipWriter)
	for _, file := range files {
		header := &tar.Header{Name: file.path, Mode: file.mode, Size: int64(len(file.data)), Typeflag: tar.TypeReg}
		if file.dir {
			header.Typeflag = tar.TypeDir
			header.Size = 0
			header.Name = strings.TrimSuffix(file.path, "/") + "/"
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("write archive header %s: %v", file.path, err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write(file.data); err != nil {
				t.Fatalf("write archive body %s: %v", file.path, err)
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

// identityScript is a real executable that reports the signed identity. The
// helper runs both shipped binaries and compares their exact stdout, so the
// fixture has to ship something that actually executes.
func identityScript() []byte {
	return []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' 'cairn %s' 'commit: %s' 'built: %s'\n",
		fixtureVersion, fixtureCommit, fixtureBuildTime))
}

// migrateScript is the fixture's migrate executable.
//
// It answers --version like any release binary, and otherwise reads its
// behaviour out of a control directory the test writes to. That is what makes
// the fault-injection cases real subprocess failures rather than mocked return
// values: the plan that refuses, the migration that exits non-zero after
// committing two steps, and the run that prints an overshot ledger are all this
// program doing exactly what a broken release would do.
func migrateScript(controlDir string) []byte {
	return []byte(fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--version" ]; then
  printf '%%s\n' 'cairn %s' 'commit: %s' 'built: %s'
  exit 0
fi
CONTROL=%q
if [ "$1" = "--plan-json" ]; then
  printf '%%s' "$MIGRATION_TARGET" > "$CONTROL/plan.target"
  printf '%%s' "$DATABASE_URL" > "$CONTROL/plan.dsn"
  [ -f "$CONTROL/plan.json" ] && cat "$CONTROL/plan.json"
  if [ -f "$CONTROL/plan.exit" ]; then exit "$(cat "$CONTROL/plan.exit")"; fi
  exit 0
fi
[ -f "$CONTROL/slow" ] && sleep "$(cat "$CONTROL/slow")"
printf '%%s' "$MIGRATION_TARGET" > "$CONTROL/apply.target"
printf '%%s' "$MIGRATION_ALLOW_MANUAL" > "$CONTROL/apply.allow_manual"
echo ran >> "$CONTROL/apply.log"
[ -f "$CONTROL/apply.json" ] && cat "$CONTROL/apply.json"
if [ -f "$CONTROL/apply.exit" ]; then exit "$(cat "$CONTROL/apply.exit")"; fi
exit 0
`, fixtureVersion, fixtureCommit, fixtureBuildTime, controlDir))
}

type releaseFixture struct {
	Manifest      releasetrust.Manifest
	ManifestBytes []byte
	SignatureRaw  []byte
	CoreArchive   []byte
	ReaderArchive []byte
	PublicKey     ed25519.PublicKey
	PackageRoot   string
}

// newReleaseFixture assembles a complete, internally consistent release.
func newReleaseFixture(t *testing.T, controlDir string) *releaseFixture {
	t.Helper()
	architecture := runtime.GOARCH
	packageRoot := fmt.Sprintf("cairn_%s_linux_%s", fixtureVersion, architecture)

	webtagBinary := identityScript()
	migrateBinary := migrateScript(controlDir)
	provenance := []byte(fmt.Sprintf(
		`{"version":%q,"commit":%q,"build_time":%q,"source_state":"clean","os":"linux","arch":%q,`+
			`"binaries":{"migrate":{"sha256":%q},"webtag":{"sha256":%q}}}`,
		fixtureVersion, fixtureCommit, fixtureBuildTime, architecture,
		sha256Hex(migrateBinary), sha256Hex(webtagBinary)))

	coreFiles := []archiveFile{
		{path: packageRoot, mode: 0o755, dir: true},
		{path: packageRoot + "/BUILD-PROVENANCE.json", data: provenance, mode: 0o644},
		{path: packageRoot + "/legal", mode: 0o755, dir: true},
		{path: packageRoot + "/legal/CAIRN_LICENSE.txt", data: []byte("license"), mode: 0o644},
		{path: packageRoot + "/migrate", data: migrateBinary, mode: 0o755},
		{path: packageRoot + "/webtag", data: webtagBinary, mode: 0o755},
	}
	coreArchive := buildTarGz(t, coreFiles)
	coreContents := mustInspect(t, coreArchive)

	readerProvenance := []byte(fmt.Sprintf(`{"release_id":"reader-%s","commit_full_sha":%q}`, fixtureVersion, fixtureCommit))
	readerFiles := []archiveFile{
		{path: "embedded", mode: 0o755, dir: true},
		{path: "embedded/index.html", data: []byte("<!doctype html><title>embedded</title>"), mode: 0o644},
		{path: "embedded/app.js", data: []byte("// embedded"), mode: 0o644},
		{path: "embedded/manifest.webmanifest", data: []byte("{}"), mode: 0o644},
		{path: releasetrust.ReaderProvenanceFileName, data: readerProvenance, mode: 0o644},
		{path: "root", mode: 0o755, dir: true},
		{path: "root/index.html", data: []byte("<!doctype html><title>root</title>"), mode: 0o644},
		{path: "root/app.js", data: []byte("// root"), mode: 0o644},
		{path: "root/manifest.webmanifest", data: []byte("{}"), mode: 0o644},
	}
	readerArchive := buildTarGz(t, readerFiles)
	readerContents := mustInspect(t, readerArchive)

	manifest := releasetrust.Manifest{
		SchemaVersion:          releasetrust.ManifestSchemaVersion,
		ArtifactKind:           releasetrust.ManifestArtifactKind,
		Repo:                   fixtureRepo,
		Tag:                    fixtureTag,
		Version:                fixtureVersion,
		Commit:                 fixtureCommit,
		BuildTime:              fixtureBuildTime,
		MinimumHelperProtocol:  releasetrust.HelperProtocol,
		SchemaTarget:           fixtureSchema,
		RiverLedgerTarget:      fixtureRiver,
		OnlineUpdateCompatible: true,
		OnlineUpdateReason:     "every migration step in this release is classified as safe to apply inside the maintenance window",
		RollbackCompatible:     false,
		RollbackReason:         "this release advances the application schema target, so restoring the previous binaries would leave the database ahead of the code",
	}

	provenanceEntry, _ := coreContents.Entry(packageRoot + "/BUILD-PROVENANCE.json")
	artifact := releasetrust.CoreArtifact{
		OS:               releasetrust.ReleaseOS,
		Arch:             architecture,
		Archive:          packageRoot + ".tar.gz",
		SHA256:           sha256Hex(coreArchive),
		SizeBytes:        int64(len(coreArchive)),
		PackageRoot:      packageRoot,
		ProvenancePath:   packageRoot + "/BUILD-PROVENANCE.json",
		ProvenanceSHA256: provenanceEntry.SHA256,
		Executables:      map[string]releasetrust.Executable{},
	}
	for _, name := range releasetrust.ExecutableNames {
		entry, ok := coreContents.Entry(packageRoot + "/" + name)
		if !ok {
			t.Fatalf("fixture archive is missing %s", name)
		}
		artifact.Executables[name] = releasetrust.Executable{
			Path:   packageRoot + "/" + name,
			SHA256: entry.SHA256,
			Identity: releasetrust.Identity{
				Version:       fixtureVersion,
				Commit:        fixtureCommit,
				BuildTime:     fixtureBuildTime,
				VersionOutput: releasetrust.ExpectedVersionOutput(fixtureVersion, fixtureCommit, fixtureBuildTime),
			},
		}
	}
	manifest.Core = []releasetrust.CoreArtifact{artifact}
	manifest.Platforms = []string{artifact.Platform()}

	readerProvenanceEntry, _ := readerContents.Entry(releasetrust.ReaderProvenanceFileName)
	manifest.Reader = releasetrust.ReaderArtifact{
		Archive:          fmt.Sprintf("cairn-reader-%s.tar.gz", fixtureVersion),
		SHA256:           sha256Hex(readerArchive),
		SizeBytes:        int64(len(readerArchive)),
		ReleaseID:        "reader-" + fixtureVersion,
		Commit:           fixtureCommit,
		ProvenancePath:   releasetrust.ReaderProvenanceFileName,
		ProvenanceSHA256: readerProvenanceEntry.SHA256,
		Builds: []releasetrust.ReaderBuild{
			{Name: "embedded", BasePath: "/reader/", Directory: "embedded",
				FileCount: 3, TotalBytes: directoryBytes(t, readerContents, "embedded/")},
			{Name: "root", BasePath: "/", Directory: "root",
				FileCount: 3, TotalBytes: directoryBytes(t, readerContents, "root/")},
		},
	}

	fixture := &releaseFixture{
		Manifest:      manifest,
		CoreArchive:   coreArchive,
		ReaderArchive: readerArchive,
		PackageRoot:   packageRoot,
	}
	fixture.sign(t, manifest)
	return fixture
}

func directoryBytes(t *testing.T, contents *releasetrust.ArchiveContents, prefix string) int64 {
	t.Helper()
	var total int64
	for _, entry := range contents.Entries {
		if !entry.Directory && strings.HasPrefix(entry.Path, prefix) {
			total += entry.Size
		}
	}
	return total
}

func mustInspect(t *testing.T, archive []byte) *releasetrust.ArchiveContents {
	t.Helper()
	contents, err := releasetrust.InspectArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("inspect fixture archive: %v", err)
	}
	return contents
}

// fixtureSigningKey is deterministic so a failing test reproduces exactly.
func fixtureSigningKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index * 7)
	}
	return ed25519.NewKeyFromSeed(seed)
}

// sign canonicalises and signs the manifest. It is also the re-sign path a
// tamper test uses after mutating a field.
func (fixture *releaseFixture) sign(t *testing.T, manifest releasetrust.Manifest) {
	t.Helper()
	canonical, err := manifest.Canonical()
	if err != nil {
		t.Fatalf("canonicalise fixture manifest: %v", err)
	}
	privateKey := fixtureSigningKey(t)
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("fixture signing key is not Ed25519")
	}
	digest := sha256.Sum256(canonical)
	signature := releasetrust.Signature{
		SchemaVersion:  releasetrust.SignatureSchemaVersion,
		ArtifactKind:   releasetrust.SignatureArtifactKind,
		Algorithm:      releasetrust.SignatureAlgorithm,
		KeyID:          "cairn-release-2026t",
		ManifestSHA256: hex.EncodeToString(digest[:]),
		Signature:      ed25519.Sign(privateKey, canonical),
	}
	signatureBytes, err := signature.Canonical()
	if err != nil {
		t.Fatalf("canonicalise fixture signature: %v", err)
	}
	fixture.Manifest = manifest
	fixture.ManifestBytes = canonical
	fixture.SignatureRaw = signatureBytes
	fixture.PublicKey = publicKey
}

// testTrust is the verification seam described at the top of this file.
type testTrust struct {
	publicKey ed25519.PublicKey
}

func (trust testTrust) VerifyRelease(request releasetrust.VerifyRequest) (*releasetrust.VerifiedRelease, error) {
	// Real canonical-form enforcement and real full manifest validation.
	manifest, err := releasetrust.ParseManifest(request.ManifestBytes)
	if err != nil {
		return nil, err
	}
	signature, err := releasetrust.ParseSignature(request.SignatureBytes)
	if err != nil {
		return nil, err
	}
	buildTime, err := time.Parse(time.RFC3339, manifest.BuildTime)
	if err != nil {
		return nil, err
	}
	// Real Ed25519 verification over the exact bytes that arrived.
	digest := sha256.Sum256(request.ManifestBytes)
	if hex.EncodeToString(digest[:]) != signature.ManifestSHA256 {
		return nil, fmt.Errorf("signature manifest_sha256 does not match the manifest bytes")
	}
	if !ed25519.Verify(trust.publicKey, request.ManifestBytes, signature.Signature) {
		return nil, fmt.Errorf("manifest signature does not verify under the fixture key")
	}
	if manifest.Repo != request.ExpectedRepo {
		return nil, fmt.Errorf("manifest names repository %q, this helper only installs %q", manifest.Repo, request.ExpectedRepo)
	}
	if !IsFormalTag(request.ExpectedTag) {
		return nil, fmt.Errorf("target %q is not a formal vX.Y.Z release tag", request.ExpectedTag)
	}
	if manifest.Tag != request.ExpectedTag {
		return nil, fmt.Errorf("manifest is for %s, the authorised target is %s", manifest.Tag, request.ExpectedTag)
	}
	// Real protocol floor and real architecture selection.
	if err := releasetrust.RequireHelperProtocol(manifest, request.HelperProtocol); err != nil {
		return nil, err
	}
	artifact, err := manifest.CoreFor(request.HostOS, request.HostArch)
	if err != nil {
		return nil, err
	}
	return &releasetrust.VerifiedRelease{
		Manifest:  manifest,
		Key:       releasetrust.TrustedKey{KeyID: signature.KeyID, PublicKey: trust.publicKey},
		Core:      artifact,
		BuildTime: buildTime,
	}, nil
}

func (testTrust) VerifyCoreArchive(data []byte, artifact releasetrust.CoreArtifact) (*releasetrust.ArchiveContents, error) {
	return releasetrust.VerifyCoreArchive(data, artifact)
}

func (testTrust) VerifyCoreProvenance(raw []byte, manifest releasetrust.Manifest, artifact releasetrust.CoreArtifact) error {
	return releasetrust.VerifyCoreProvenance(raw, manifest, artifact)
}

func (testTrust) VerifyExecutableIdentity(name string, output []byte, artifact releasetrust.CoreArtifact) error {
	return releasetrust.VerifyExecutableIdentity(name, output, artifact)
}

func (testTrust) VerifyReaderArchive(data []byte, reader releasetrust.ReaderArtifact) (*releasetrust.ArchiveContents, error) {
	return releasetrust.VerifyReaderArchive(data, reader)
}

// assetServer serves the fixture's release assets over a real HTTP server.
//
// The URLs the helper builds are left exactly as releasetrust.AssetURL produced
// them — real https://github.com/... URLs — and only the transport is
// redirected. That keeps AssetURL, CheckDownloadURL and the redirect policy in
// the tested path rather than bypassed by a test-only base URL.
type assetServer struct {
	server   *httptest.Server
	assets   map[string][]byte
	requests []string
	latest   string
	// failAsset makes one named asset return 500.
	failAsset string
}

func newAssetServer(t *testing.T, fixture *releaseFixture) *assetServer {
	t.Helper()
	assets := &assetServer{assets: map[string][]byte{}, latest: fixture.Manifest.Tag}
	assets.publish(fixture)
	assets.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assets.requests = append(assets.requests, request.URL.Path)
		if strings.HasSuffix(request.URL.Path, "/releases/latest") {
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(writer, `{"tag_name":%q,"draft":false,"prerelease":false}`, assets.latest)
			return
		}
		name := request.URL.Path[strings.LastIndexByte(request.URL.Path, '/')+1:]
		if name == assets.failAsset {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		body, ok := assets.assets[name]
		if !ok {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = writer.Write(body)
	}))
	t.Cleanup(assets.server.Close)
	return assets
}

func (assets *assetServer) publish(fixture *releaseFixture) {
	assets.assets[releasetrust.ManifestFileName] = fixture.ManifestBytes
	assets.assets[releasetrust.SignatureFileName] = fixture.SignatureRaw
	assets.assets[fixture.Manifest.Core[0].Archive] = fixture.CoreArchive
	assets.assets[fixture.Manifest.Reader.Archive] = fixture.ReaderArchive
}

// downloader returns a Downloader whose transport reaches the test server while
// every URL it builds stays a real release URL.
func (assets *assetServer) downloader(t *testing.T) *Downloader {
	t.Helper()
	base, err := url.Parse(assets.server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	downloader := NewDownloader(fixtureRepo)
	downloader.client.Transport = &rewriteTransport{host: base.Host}
	return downloader
}

type rewriteTransport struct{ host string }

func (transport *rewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	rewritten := request.Clone(request.Context())
	rewritten.URL.Scheme = "http"
	rewritten.URL.Host = transport.host
	rewritten.Host = ""
	return http.DefaultTransport.RoundTrip(rewritten)
}
