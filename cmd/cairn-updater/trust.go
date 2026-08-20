package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"

	"webtag/internal/releasetrust"
)

// formalTagPattern is the only shape of target this helper accepts.
//
// It is intentionally the strictest possible reading of "a version": no
// `latest`, no branch name, no `v1.2`, no `v1.2.3-rc1`, no build metadata, no
// leading zeroes hiding a second interpretation. Discovery may suggest a
// candidate; execution is always against a tag that matched this and was then
// matched again against the tag inside the signed manifest.
var formalTagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

var fullCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// IsFormalTag reports whether target is an exact formal release tag.
func IsFormalTag(target string) bool { return formalTagPattern.MatchString(target) }

type releaseVersion struct {
	tag                 string
	major, minor, patch uint64
}

func parseReleaseTag(target string) (releaseVersion, error) {
	groups := formalTagPattern.FindStringSubmatch(target)
	if groups == nil {
		return releaseVersion{}, fmt.Errorf("%q is not a formal vX.Y.Z release tag", target)
	}
	parts := make([]uint64, 3)
	for index := range parts {
		value, err := strconv.ParseUint(groups[index+1], 10, 64)
		if err != nil {
			return releaseVersion{}, fmt.Errorf("parse release tag %q: %w", target, err)
		}
		parts[index] = value
	}
	if parts[0] == 0 && parts[1] == 0 && parts[2] == 0 {
		return releaseVersion{}, fmt.Errorf("%q is the unversioned build placeholder, not a release", target)
	}
	return releaseVersion{tag: target, major: parts[0], minor: parts[1], patch: parts[2]}, nil
}

func releaseTagFromCoreVersion(version string) (releaseVersion, error) {
	parsed, err := parseReleaseTag("v" + version)
	if err != nil {
		return releaseVersion{}, fmt.Errorf("running Core version %q is not a formal release version: %w", version, err)
	}
	return parsed, nil
}

func (version releaseVersion) sameSeries(other releaseVersion) bool {
	return version.major == other.major && version.minor == other.minor
}

// ReleaseTrust is the exact slice of internal/releasetrust the update state
// machine depends on.
//
// It exists as an interface for one reason: the compiled-in trust root is a
// private package variable, so a test outside internal/releasetrust cannot
// produce a manifest the real verifier would accept. Rather than adding a
// production-reachable "install these keys" hook — which is the one API a
// release trust root must never have — the seam is here, in the consumer. The
// production wiring is a single struct that forwards to the package functions,
// and TestProductionTrustIsTheRealPackage pins it in place.
//
// Failure-path tests do not use the seam: a tampered manifest, a broken
// signature, or a mismatched tag fails under the real verifier regardless of
// which key signed it, so those tests drive productionTrust directly.
type ReleaseTrust interface {
	VerifyRelease(request releasetrust.VerifyRequest) (*releasetrust.VerifiedRelease, error)
	VerifyCoreArchive(data []byte, artifact releasetrust.CoreArtifact) (*releasetrust.ArchiveContents, error)
	VerifyCoreProvenance(raw []byte, manifest releasetrust.Manifest, artifact releasetrust.CoreArtifact) error
	VerifyExecutableIdentity(name string, output []byte, artifact releasetrust.CoreArtifact) error
	VerifyReaderArchive(data []byte, reader releasetrust.ReaderArtifact) (*releasetrust.ArchiveContents, error)
}

// productionTrust forwards to internal/releasetrust with no behaviour of its
// own. It holds no state, so there is nothing here that could be configured
// into accepting an untrusted key.
type productionTrust struct{}

func (productionTrust) VerifyRelease(request releasetrust.VerifyRequest) (*releasetrust.VerifiedRelease, error) {
	return releasetrust.VerifyRelease(request)
}

func (productionTrust) VerifyCoreArchive(data []byte, artifact releasetrust.CoreArtifact) (*releasetrust.ArchiveContents, error) {
	return releasetrust.VerifyCoreArchive(data, artifact)
}

func (productionTrust) VerifyCoreProvenance(raw []byte, manifest releasetrust.Manifest, artifact releasetrust.CoreArtifact) error {
	return releasetrust.VerifyCoreProvenance(raw, manifest, artifact)
}

func (productionTrust) VerifyExecutableIdentity(name string, output []byte, artifact releasetrust.CoreArtifact) error {
	return releasetrust.VerifyExecutableIdentity(name, output, artifact)
}

func (productionTrust) VerifyReaderArchive(data []byte, reader releasetrust.ReaderArtifact) (*releasetrust.ArchiveContents, error) {
	return releasetrust.VerifyReaderArchive(data, reader)
}

// digestHex is the manifest fingerprint the UI shows next to the tag. It is the
// digest of the exact bytes that were verified, so an operator comparing it
// against the release page is comparing the same document the helper decided
// on.
func digestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
