package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"webtag/internal/migrate"
	"webtag/internal/releasetrust"
)

type manifestInputs struct {
	DistDir             string
	Repo                string
	Tag                 string
	Commit              string
	BuildTime           string
	PreviousTarget      string
	PreviousRiverTarget int
}

var coreArchivePattern = regexp.MustCompile(`^cairn_([0-9]+\.[0-9]+\.[0-9]+)_linux_([a-z0-9]+)\.tar\.gz$`)

// readReleaseAsset reads one file out of the release asset directory. Names are
// derived from the release identity rather than taken from the caller, so the
// path can only ever land inside the directory being packaged.
func readReleaseAsset(distDir, name string) ([]byte, error) {
	if name != filepath.Base(name) {
		return nil, fmt.Errorf("release asset %q is not a plain file name", name)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Clean(distDir), name)) //nolint:gosec // The name is a validated plain file name inside the release directory.
	if err != nil {
		return nil, fmt.Errorf("read release asset %s: %w", name, err)
	}
	return data, nil
}

func buildManifest(inputs manifestInputs) (releasetrust.Manifest, error) {
	var manifest releasetrust.Manifest
	if inputs.Repo == "" || inputs.Tag == "" || inputs.Commit == "" || inputs.BuildTime == "" {
		return manifest, errors.New("generate requires --repo, --tag, --commit, and --build-time")
	}
	version := strings.TrimPrefix(inputs.Tag, "v")

	target, err := schemaTarget(migrate.Steps())
	if err != nil {
		return manifest, err
	}
	riverTarget, err := riverLedgerTarget()
	if err != nil {
		return manifest, err
	}
	onlineCompatible, onlineReason := onlineUpdateDecision(classifySteps(migrate.Steps()))
	rollbackCompatible, rollbackReason := rollbackDecision(target, riverTarget,
		inputs.PreviousTarget, inputs.PreviousRiverTarget)

	manifest = releasetrust.Manifest{
		SchemaVersion:          releasetrust.ManifestSchemaVersion,
		ArtifactKind:           releasetrust.ManifestArtifactKind,
		Repo:                   inputs.Repo,
		Tag:                    inputs.Tag,
		Version:                version,
		Commit:                 inputs.Commit,
		BuildTime:              inputs.BuildTime,
		MinimumHelperProtocol:  releasetrust.HelperProtocol,
		SchemaTarget:           target,
		RiverLedgerTarget:      riverTarget,
		OnlineUpdateCompatible: onlineCompatible,
		OnlineUpdateReason:     onlineReason,
		RollbackCompatible:     rollbackCompatible,
		RollbackReason:         rollbackReason,
	}

	if manifest.Core, err = collectCoreArtifacts(inputs.DistDir, manifest); err != nil {
		return manifest, err
	}
	for _, artifact := range manifest.Core {
		manifest.Platforms = append(manifest.Platforms, artifact.Platform())
	}
	if manifest.Reader, err = collectReaderArtifact(inputs.DistDir, manifest); err != nil {
		return manifest, err
	}
	if err := manifest.Validate(); err != nil {
		return manifest, err
	}
	// Generation ends by reading the manifest back the way the helper will.
	// A manifest that describes archives it cannot itself verify would be
	// discovered on the production host instead of on the release runner.
	if err := verifyManifestAgainstAssets(inputs.DistDir, manifest, io.Discard); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func collectCoreArtifacts(distDir string, manifest releasetrust.Manifest) ([]releasetrust.CoreArtifact, error) {
	pattern := filepath.Join(filepath.Clean(distDir), fmt.Sprintf("cairn_%s_linux_*.tar.gz", manifest.Version))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("scan release archives: %w", err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no Core archive matching %s", filepath.Base(pattern))
	}
	slices.Sort(matches)

	artifacts := make([]releasetrust.CoreArtifact, 0, len(matches))
	for _, match := range matches {
		name := filepath.Base(match)
		groups := coreArchivePattern.FindStringSubmatch(name)
		if groups == nil {
			return nil, fmt.Errorf("archive %s does not use the canonical Core archive name", name)
		}
		if groups[1] != manifest.Version {
			return nil, fmt.Errorf("archive %s is not version %s", name, manifest.Version)
		}
		data, err := readReleaseAsset(distDir, name)
		if err != nil {
			return nil, err
		}
		contents, err := releasetrust.InspectArchive(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", name, err)
		}
		packageRoot := strings.TrimSuffix(name, ".tar.gz")
		artifact := releasetrust.CoreArtifact{
			OS:             releasetrust.ReleaseOS,
			Arch:           groups[2],
			Archive:        name,
			SHA256:         digestOf(data),
			SizeBytes:      int64(len(data)),
			PackageRoot:    packageRoot,
			ProvenancePath: packageRoot + "/BUILD-PROVENANCE.json",
			Executables:    map[string]releasetrust.Executable{},
		}
		provenance, ok := contents.Entry(artifact.ProvenancePath)
		if !ok {
			return nil, fmt.Errorf("archive %s is missing %s", name, artifact.ProvenancePath)
		}
		artifact.ProvenanceSHA256 = provenance.SHA256
		for _, executableName := range releasetrust.ExecutableNames {
			path := packageRoot + "/" + executableName
			entry, found := contents.Entry(path)
			if !found {
				return nil, fmt.Errorf("archive %s is missing executable %s", name, executableName)
			}
			artifact.Executables[executableName] = releasetrust.Executable{
				Path:   path,
				SHA256: entry.SHA256,
				Identity: releasetrust.Identity{
					Version:   manifest.Version,
					Commit:    manifest.Commit,
					BuildTime: manifest.BuildTime,
					VersionOutput: releasetrust.ExpectedVersionOutput(
						manifest.Version, manifest.Commit, manifest.BuildTime),
				},
			}
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, nil
}

type readerProvenance struct {
	SchemaVersion int    `json:"schema_version"`
	ArtifactKind  string `json:"artifact_kind"`
	ReleaseID     string `json:"release_id"`
	CommitFullSHA string `json:"commit_full_sha"`
	GeneratedAt   string `json:"generated_at"`
	Builds        []struct {
		Name      string `json:"name"`
		BasePath  string `json:"base_path"`
		Directory string `json:"directory"`
		FileCount int64  `json:"file_count"`
		TotalSize int64  `json:"total_bytes"`
	} `json:"builds"`
}

func collectReaderArtifact(distDir string, manifest releasetrust.Manifest) (releasetrust.ReaderArtifact, error) {
	var artifact releasetrust.ReaderArtifact
	name := fmt.Sprintf("cairn-reader-%s.tar.gz", manifest.Version)
	data, err := readReleaseAsset(distDir, name)
	if err != nil {
		return artifact, err
	}
	contents, err := releasetrust.InspectArchive(bytes.NewReader(data))
	if err != nil {
		return artifact, fmt.Errorf("inspect %s: %w", name, err)
	}
	entry, ok := contents.Entry(releasetrust.ReaderProvenanceFileName)
	if !ok {
		return artifact, fmt.Errorf("archive %s is missing %s", name, releasetrust.ReaderProvenanceFileName)
	}
	raw, err := releasetrust.ReadArchiveFile(bytes.NewReader(data), releasetrust.ReaderProvenanceFileName)
	if err != nil {
		return artifact, err
	}
	var provenance readerProvenance
	if err := json.Unmarshal(raw, &provenance); err != nil {
		return artifact, fmt.Errorf("parse %s: %w", releasetrust.ReaderProvenanceFileName, err)
	}
	if provenance.ArtifactKind != "reader-vnext-release-manifest" {
		return artifact, fmt.Errorf("%s is not a Reader release manifest", releasetrust.ReaderProvenanceFileName)
	}
	if provenance.CommitFullSHA != manifest.Commit {
		return artifact, fmt.Errorf("reader provenance commit %s is not the release commit", provenance.CommitFullSHA)
	}
	artifact = releasetrust.ReaderArtifact{
		Archive:          name,
		SHA256:           digestOf(data),
		SizeBytes:        int64(len(data)),
		ReleaseID:        provenance.ReleaseID,
		Commit:           provenance.CommitFullSHA,
		ProvenancePath:   releasetrust.ReaderProvenanceFileName,
		ProvenanceSHA256: entry.SHA256,
	}
	for _, build := range provenance.Builds {
		artifact.Builds = append(artifact.Builds, releasetrust.ReaderBuild{
			Name:       build.Name,
			BasePath:   build.BasePath,
			Directory:  build.Directory,
			FileCount:  build.FileCount,
			TotalBytes: build.TotalSize,
		})
	}
	slices.SortFunc(artifact.Builds, func(left, right releasetrust.ReaderBuild) int {
		return strings.Compare(left.Name, right.Name)
	})
	return artifact, nil
}

func digestOf(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// verifyRelease is the offline rehearsal of what the helper does on the
// production host: parse the manifest, verify the detached signature against
// the compiled-in trust root, then check every archive it names.
func verifyRelease(distDir, repo, tag, hostArch string, report io.Writer) error {
	if repo == "" || tag == "" {
		return errors.New("verify requires --repo and --tag")
	}
	manifestBytes, err := readReleaseAsset(distDir, releasetrust.ManifestFileName)
	if err != nil {
		return err
	}
	signatureBytes, err := readReleaseAsset(distDir, releasetrust.SignatureFileName)
	if err != nil {
		return err
	}
	manifest, err := releasetrust.ParseManifest(manifestBytes)
	if err != nil {
		return err
	}
	architectures := []string{hostArch}
	if hostArch == "" {
		architectures = architectures[:0]
		for _, artifact := range manifest.Core {
			architectures = append(architectures, artifact.Arch)
		}
	}
	for _, architecture := range architectures {
		verified, err := releasetrust.VerifyRelease(releasetrust.VerifyRequest{
			ManifestBytes:  manifestBytes,
			SignatureBytes: signatureBytes,
			ExpectedRepo:   repo,
			ExpectedTag:    tag,
			HostOS:         releasetrust.ReleaseOS,
			HostArch:       architecture,
			HelperProtocol: releasetrust.HelperProtocol,
		})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(report, "verified signature over %s for %s/%s under key %s\n",
			releasetrust.ManifestFileName, verified.Core.OS, verified.Core.Arch, verified.Key.KeyID)
	}
	return verifyManifestAgainstAssets(distDir, manifest, report)
}

// verifyManifestAgainstAssets checks every archive the manifest names against
// the manifest, including in-archive provenance and the "no executable beyond
// the two declared programs" rule.
func verifyManifestAgainstAssets(distDir string, manifest releasetrust.Manifest, report io.Writer) error {
	for _, artifact := range manifest.Core {
		data, err := readReleaseAsset(distDir, artifact.Archive)
		if err != nil {
			return err
		}
		if _, err := releasetrust.VerifyCoreArchive(data, artifact); err != nil {
			return err
		}
		provenance, err := releasetrust.ReadArchiveFile(bytes.NewReader(data), artifact.ProvenancePath)
		if err != nil {
			return err
		}
		if err := releasetrust.VerifyCoreProvenance(provenance, manifest, artifact); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(report, "verified %s (%d bytes, %s)\n", artifact.Archive, artifact.SizeBytes, artifact.Platform())
	}
	readerData, err := readReleaseAsset(distDir, manifest.Reader.Archive)
	if err != nil {
		return err
	}
	if _, err := releasetrust.VerifyReaderArchive(readerData, manifest.Reader); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(report, "verified %s (%d bytes)\n", manifest.Reader.Archive, manifest.Reader.SizeBytes)

	// SHA256SUMS stays a corruption check, but it must at least agree with the
	// signed manifest when it is present.
	sums, err := readReleaseAsset(distDir, "SHA256SUMS")
	if err != nil {
		return nil //nolint:nilerr // SHA256SUMS is optional evidence, not the trust root.
	}
	checksums, err := releasetrust.ParseChecksums(sums)
	if err != nil {
		return err
	}
	// Generate runs before the Release is sealed, so an artifact SHA256SUMS
	// has not reached yet is ordering rather than inconsistency. A listed
	// digest that disagrees is still fatal. The strict comparison runs in
	// verify, after sealing.
	if err := releasetrust.CrossCheckListedChecksums(manifest, checksums); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(report, "SHA256SUMS agrees with the signed manifest so far as it is written")
	return nil
}

func generateKeyPair(output io.Writer) error {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return fmt.Errorf("draw signing seed: %w", err)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok {
		return errors.New("generated key is not an Ed25519 key")
	}
	_, _ = fmt.Fprintf(output, "%s=%s\n", releasetrust.SigningKeyEnv, base64.StdEncoding.EncodeToString(seed))
	_, _ = fmt.Fprintf(output, "public_key=%s\n", base64.StdEncoding.EncodeToString(publicKey))
	_, _ = fmt.Fprintln(output, "Store the seed in the repository secret and never write it to a file in the repository.")
	_, _ = fmt.Fprintln(output, "Add the public key to internal/releasetrust/keys.go, keeping the outgoing key trusted")
	_, _ = fmt.Fprintln(output, "for at least releasetrust.MinimumRotationOverlap after the new key starts signing.")
	return nil
}
