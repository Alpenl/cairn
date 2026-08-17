package releasetrust

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// SignatureArtifactKind tags the detached signature document.
	SignatureArtifactKind = "cairn-release-signature"

	// SignatureSchemaVersion is the only detached signature schema accepted.
	SignatureSchemaVersion = 1

	// SignatureAlgorithm is Ed25519 from the Go standard library. No third
	// party crypto is involved anywhere in this trust path.
	SignatureAlgorithm = "ed25519"

	// SigningKeyEnv carries the base64 encoded 32-byte Ed25519 seed in CI. It
	// is a repository secret; the seed itself must never appear in the
	// repository, in a log, in a Release asset, or in a job output.
	SigningKeyEnv = "CAIRN_RELEASE_SIGNING_KEY"
)

// Signature is the detached signature document. It is itself canonical JSON so
// that a mangled signature file is a parse failure rather than a silent
// mismatch.
type Signature struct {
	SchemaVersion  int
	ArtifactKind   string
	Algorithm      string
	KeyID          string
	ManifestSHA256 string
	Signature      []byte
}

// CanonicalValue renders the detached signature as a canonical value tree.
func (signature Signature) CanonicalValue() CanonicalValue {
	return CanonicalObject{
		{Key: "algorithm", Value: CanonicalString(signature.Algorithm)},
		{Key: "artifact_kind", Value: CanonicalString(signature.ArtifactKind)},
		{Key: "key_id", Value: CanonicalString(signature.KeyID)},
		{Key: "manifest_sha256", Value: CanonicalString(signature.ManifestSHA256)},
		{Key: "schema_version", Value: CanonicalInt(int64(signature.SchemaVersion))},
		{Key: "signature", Value: CanonicalString(base64.StdEncoding.EncodeToString(signature.Signature))},
	}
}

// Canonical renders the signature file bytes.
func (signature Signature) Canonical() ([]byte, error) {
	if err := signature.validateShape(); err != nil {
		return nil, err
	}
	return EncodeCanonical(signature.CanonicalValue())
}

func (signature Signature) validateShape() error {
	if signature.SchemaVersion != SignatureSchemaVersion {
		return fmt.Errorf("signature schema_version %d is not supported (want %d)",
			signature.SchemaVersion, SignatureSchemaVersion)
	}
	if signature.ArtifactKind != SignatureArtifactKind {
		return fmt.Errorf("signature artifact_kind %q is not %q", signature.ArtifactKind, SignatureArtifactKind)
	}
	if signature.Algorithm != SignatureAlgorithm {
		return fmt.Errorf("signature algorithm %q is not %q", signature.Algorithm, SignatureAlgorithm)
	}
	if strings.TrimSpace(signature.KeyID) == "" {
		return errors.New("signature does not name a key_id")
	}
	if !sha256Pattern.MatchString(signature.ManifestSHA256) {
		return fmt.Errorf("signature manifest_sha256 %q is not a lowercase hex digest", signature.ManifestSHA256)
	}
	if len(signature.Signature) != ed25519.SignatureSize {
		return fmt.Errorf("signature is %d bytes, want %d", len(signature.Signature), ed25519.SignatureSize)
	}
	return nil
}

// ParseSignature reads a detached signature from its canonical bytes.
func ParseSignature(data []byte) (Signature, error) {
	var signature Signature
	if err := RequireCanonical(data); err != nil {
		return signature, fmt.Errorf("release signature: %w", err)
	}
	value, err := DecodeCanonical(data)
	if err != nil {
		return signature, fmt.Errorf("release signature: %w", err)
	}
	object, err := asObject(value, "signature")
	if err != nil {
		return signature, err
	}
	fields := newFieldReader(object, "signature")
	if signature.SchemaVersion, err = fields.intField("schema_version"); err != nil {
		return signature, err
	}
	if signature.ArtifactKind, err = fields.stringField("artifact_kind"); err != nil {
		return signature, err
	}
	if signature.Algorithm, err = fields.stringField("algorithm"); err != nil {
		return signature, err
	}
	if signature.KeyID, err = fields.stringField("key_id"); err != nil {
		return signature, err
	}
	if signature.ManifestSHA256, err = fields.stringField("manifest_sha256"); err != nil {
		return signature, err
	}
	encoded, err := fields.stringField("signature")
	if err != nil {
		return signature, err
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return signature, fmt.Errorf("signature is not standard base64: %w", err)
	}
	signature.Signature = raw
	if err := fields.requireExhausted(); err != nil {
		return signature, err
	}
	return signature, signature.validateShape()
}

// DecodeSigningSeed turns the base64 seed from SigningKeyEnv into a private
// key. Surrounding whitespace is tolerated because secret stores add it; the
// error never echoes the secret.
func DecodeSigningSeed(encodedSeed string) (ed25519.PrivateKey, error) {
	trimmed := strings.TrimSpace(encodedSeed)
	if trimmed == "" {
		return nil, fmt.Errorf("%s is empty: releases fail closed rather than ship unsigned", SigningKeyEnv)
	}
	seed, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%s is not standard base64", SigningKeyEnv)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s decodes to %d bytes, want a %d-byte Ed25519 seed",
			SigningKeyEnv, len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// SignManifest signs canonical manifest bytes and returns the detached
// signature document. The key_id is derived from the private key, so the
// signature can only ever name the key that actually produced it.
func SignManifest(canonicalManifest []byte, privateKey ed25519.PrivateKey) (Signature, error) {
	manifest, err := ParseManifest(canonicalManifest)
	if err != nil {
		return Signature{}, err
	}
	buildTime, err := time.Parse(time.RFC3339, manifest.BuildTime)
	if err != nil {
		return Signature{}, fmt.Errorf("manifest build_time %q is not RFC3339: %w", manifest.BuildTime, err)
	}
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok {
		return Signature{}, errors.New("signing key is not an Ed25519 key")
	}
	trusted, err := TrustedKeyForPublicKey(publicKey, buildTime)
	if err != nil {
		return Signature{}, err
	}
	digest := sha256.Sum256(canonicalManifest)
	return Signature{
		SchemaVersion:  SignatureSchemaVersion,
		ArtifactKind:   SignatureArtifactKind,
		Algorithm:      SignatureAlgorithm,
		KeyID:          trusted.KeyID,
		ManifestSHA256: hex.EncodeToString(digest[:]),
		Signature:      ed25519.Sign(privateKey, canonicalManifest),
	}, nil
}

// VerifySignature checks a detached signature against canonical manifest bytes
// and the compiled-in trust root.
//
// The manifest_sha256 member is a debugging aid, not a shortcut: it is checked,
// but the Ed25519 verification is always performed over the full manifest
// bytes, never over the digest the signature file claims.
func VerifySignature(canonicalManifest []byte, signature Signature, buildTime time.Time) (TrustedKey, error) {
	if err := signature.validateShape(); err != nil {
		return TrustedKey{}, err
	}
	key, err := LookupTrustedKey(signature.KeyID, buildTime)
	if err != nil {
		return TrustedKey{}, err
	}
	digest := sha256.Sum256(canonicalManifest)
	if hex.EncodeToString(digest[:]) != signature.ManifestSHA256 {
		return TrustedKey{}, errors.New("signature manifest_sha256 does not match the manifest bytes")
	}
	if !ed25519.Verify(key.PublicKey, canonicalManifest, signature.Signature) {
		return TrustedKey{}, fmt.Errorf("manifest signature does not verify under trusted key %s", key.KeyID)
	}
	return key, nil
}
