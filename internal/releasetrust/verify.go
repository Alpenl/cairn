package releasetrust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// VerifyRequest is everything the helper knows before it trusts a release.
//
// ExpectedTag is mandatory. Discovery may name a candidate through /latest, but
// execution is always against one exact tag the operator confirmed, so the
// verification entry point simply has no "whatever the manifest says" mode.
type VerifyRequest struct {
	// ManifestBytes and SignatureBytes are the downloaded assets, unmodified.
	ManifestBytes  []byte
	SignatureBytes []byte
	// ExpectedRepo is the only repository releases may come from.
	ExpectedRepo string
	// ExpectedTag is the exact formal tag the operator authorised.
	ExpectedTag string
	// HostOS and HostArch are the machine the release would be installed on,
	// normally runtime.GOOS and runtime.GOARCH.
	HostOS   string
	HostArch string
	// HelperProtocol is the protocol version of the running helper.
	HelperProtocol int
}

// VerifiedRelease is the result of a successful verification. Holding one means
// the signature, repository, tag, host platform, and protocol floor have all
// been checked; it does not yet mean any archive has been downloaded.
type VerifiedRelease struct {
	Manifest  Manifest
	Key       TrustedKey
	Core      CoreArtifact
	BuildTime time.Time
}

// VerifyRelease performs the manifest-level half of the trust decision. The
// order matters: nothing derived from the manifest is used before the signature
// over its exact bytes has verified under a compiled-in key.
func VerifyRelease(request VerifyRequest) (*VerifiedRelease, error) {
	manifest, err := ParseManifest(request.ManifestBytes)
	if err != nil {
		return nil, err
	}
	signature, err := ParseSignature(request.SignatureBytes)
	if err != nil {
		return nil, err
	}
	buildTime, err := time.Parse(time.RFC3339, manifest.BuildTime)
	if err != nil {
		return nil, fmt.Errorf("manifest build_time %q is not RFC3339: %w", manifest.BuildTime, err)
	}
	key, err := VerifySignature(request.ManifestBytes, signature, buildTime)
	if err != nil {
		return nil, err
	}
	if request.ExpectedRepo == "" {
		return nil, errors.New("verification requires the expected repository")
	}
	if manifest.Repo != request.ExpectedRepo {
		return nil, fmt.Errorf("manifest names repository %q, this helper only installs %q",
			manifest.Repo, request.ExpectedRepo)
	}
	if !formalTagPattern.MatchString(request.ExpectedTag) {
		return nil, fmt.Errorf("target %q is not a formal vX.Y.Z release tag", request.ExpectedTag)
	}
	if manifest.Tag != request.ExpectedTag {
		return nil, fmt.Errorf("manifest is for %s, the authorised target is %s", manifest.Tag, request.ExpectedTag)
	}
	if err := RequireHelperProtocol(manifest, request.HelperProtocol); err != nil {
		return nil, err
	}
	artifact, err := manifest.CoreFor(request.HostOS, request.HostArch)
	if err != nil {
		return nil, err
	}
	return &VerifiedRelease{Manifest: manifest, Key: key, Core: artifact, BuildTime: buildTime}, nil
}

// RequireHelperProtocol enforces the manifest's helper protocol floor. The
// helper does not self-update through the page, so an incompatible release is a
// read-only "upgrade the helper by hand" state, never an implicit upgrade.
func RequireHelperProtocol(manifest Manifest, helperProtocol int) error {
	if helperProtocol < manifest.MinimumHelperProtocol {
		return fmt.Errorf("release %s requires helper protocol %d, this helper speaks %d: upgrade the helper manually",
			manifest.Tag, manifest.MinimumHelperProtocol, helperProtocol)
	}
	return nil
}

// VerifyCoreArchive checks downloaded Core archive bytes against the signed
// matrix entry and returns the inspected contents.
func VerifyCoreArchive(data []byte, artifact CoreArtifact) (*ArchiveContents, error) {
	if err := VerifyArchiveBytes(data, artifact.SHA256, artifact.SizeBytes); err != nil {
		return nil, fmt.Errorf("core archive %s: %w", artifact.Archive, err)
	}
	contents, err := InspectArchive(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("core archive %s: %w", artifact.Archive, err)
	}
	if err := VerifyCoreArchiveContents(contents, artifact); err != nil {
		return nil, err
	}
	return contents, nil
}

// VerifyReaderArchive checks downloaded Reader archive bytes against the signed
// entry and returns the inspected contents.
func VerifyReaderArchive(data []byte, reader ReaderArtifact) (*ArchiveContents, error) {
	if err := VerifyArchiveBytes(data, reader.SHA256, reader.SizeBytes); err != nil {
		return nil, fmt.Errorf("reader archive %s: %w", reader.Archive, err)
	}
	contents, err := InspectArchive(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("reader archive %s: %w", reader.Archive, err)
	}
	if err := VerifyReaderArchiveContents(contents, reader); err != nil {
		return nil, err
	}
	return contents, nil
}

// buildProvenance mirrors the BUILD-PROVENANCE.json produced by
// scripts/core-release-build.sh.
type buildProvenance struct {
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	BuildTime   string `json:"build_time"`
	SourceState string `json:"source_state"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Binaries    map[string]struct {
		SHA256 string `json:"sha256"`
	} `json:"binaries"`
}

// VerifyCoreProvenance checks the in-archive BUILD-PROVENANCE.json against the
// signed manifest. The hash check in VerifyCoreArchiveContents already proves
// the file is the one that was signed; this proves the signed file actually
// describes this release rather than some other clean build.
//
//nolint:gocyclo // One branch per provenance field; each is an independent claim.
func VerifyCoreProvenance(raw []byte, manifest Manifest, artifact CoreArtifact) error {
	var provenance buildProvenance
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&provenance); err != nil {
		return fmt.Errorf("core archive %s provenance is not the expected document: %w", artifact.Archive, err)
	}
	if provenance.Version != manifest.Version {
		return fmt.Errorf("core archive %s provenance version %q is not %q",
			artifact.Archive, provenance.Version, manifest.Version)
	}
	if provenance.Commit != manifest.Commit {
		return fmt.Errorf("core archive %s provenance commit %q is not the release commit",
			artifact.Archive, provenance.Commit)
	}
	if provenance.BuildTime != manifest.BuildTime {
		return fmt.Errorf("core archive %s provenance build time %q is not %q",
			artifact.Archive, provenance.BuildTime, manifest.BuildTime)
	}
	if provenance.SourceState != "clean" {
		return fmt.Errorf("core archive %s was built from a %q source tree", artifact.Archive, provenance.SourceState)
	}
	if provenance.OS != artifact.OS || provenance.Arch != artifact.Arch {
		return fmt.Errorf("core archive %s provenance targets %s/%s, the manifest declares %s/%s",
			artifact.Archive, provenance.OS, provenance.Arch, artifact.OS, artifact.Arch)
	}
	if len(provenance.Binaries) != len(artifact.Executables) {
		return fmt.Errorf("core archive %s provenance describes %d binaries, the manifest declares %d",
			artifact.Archive, len(provenance.Binaries), len(artifact.Executables))
	}
	for name, executable := range artifact.Executables {
		binary, ok := provenance.Binaries[name]
		if !ok {
			return fmt.Errorf("core archive %s provenance does not describe %s", artifact.Archive, name)
		}
		if binary.SHA256 != executable.SHA256 {
			return fmt.Errorf("core archive %s provenance hash for %s disagrees with the signed manifest",
				artifact.Archive, name)
		}
	}
	return nil
}

// VerifyExecutableIdentity compares the raw `--version` output of an extracted
// executable against the signed identity. Comparison is byte for byte on
// purpose: re-deriving the expected string in the helper would create a second
// place for the format to drift.
func VerifyExecutableIdentity(name string, output []byte, artifact CoreArtifact) error {
	executable, ok := artifact.Executables[name]
	if !ok {
		return fmt.Errorf("core archive %s does not declare an executable named %s", artifact.Archive, name)
	}
	actual := string(bytes.TrimRight(output, "\n"))
	if actual != executable.Identity.VersionOutput {
		return fmt.Errorf("%s --version reported a different build than the signed manifest declares", name)
	}
	return nil
}
