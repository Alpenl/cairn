package main

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"

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

// IsFormalTag reports whether target is an exact formal release tag.
func IsFormalTag(target string) bool { return formalTagPattern.MatchString(target) }

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
