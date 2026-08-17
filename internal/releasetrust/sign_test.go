package releasetrust

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestSignatureRoundTripsThroughItsCanonicalBytes(t *testing.T) {
	release := newTestRelease(t)

	parsed, err := ParseSignature(release.SigBytes)
	if err != nil {
		t.Fatalf("parse signature: %v", err)
	}
	reencoded, err := parsed.Canonical()
	if err != nil {
		t.Fatalf("re-canonicalise signature: %v", err)
	}
	if !bytes.Equal(reencoded, release.SigBytes) {
		t.Fatal("parsing and re-encoding the signature changed its bytes")
	}
	if parsed.KeyID != release.KeyID {
		t.Fatalf("signature names key %s, want %s", parsed.KeyID, release.KeyID)
	}
}

// The signature file's manifest_sha256 is convenience, not authority. Verifying
// the Ed25519 signature over that digest instead of over the manifest bytes
// would let an attacker who can pick the digest pick the document.
func TestSignatureVerificationCoversTheManifestBytesNotTheDeclaredDigest(t *testing.T) {
	release := newTestRelease(t)

	buildTime, err := time.Parse(time.RFC3339, testBuildTime)
	if err != nil {
		t.Fatalf("parse build time: %v", err)
	}
	forged := release.Signature
	forged.ManifestSHA256 = sha256Hex([]byte("some other document"))
	if _, err := VerifySignature(release.ManifestBytes, forged, buildTime); err == nil {
		t.Fatal("a signature declaring the wrong manifest digest was accepted")
	}

	tampered := append([]byte(nil), release.ManifestBytes...)
	tampered[len(tampered)-2] = ' '
	if _, err := VerifySignature(tampered, release.Signature, buildTime); err == nil {
		t.Fatal("a signature was accepted over manifest bytes it does not cover")
	}
}

func TestSigningKeyIsDerivedFromTheSecretNotDeclaredBesideIt(t *testing.T) {
	release := newTestRelease(t)

	signature, err := SignManifest(release.ManifestBytes, release.SigningKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if signature.KeyID != release.KeyID {
		t.Fatalf("signature names key %s, want %s", signature.KeyID, release.KeyID)
	}

	strangerSeed := make([]byte, ed25519.SeedSize)
	for index := range strangerSeed {
		strangerSeed[index] = 0xAB
	}
	stranger := ed25519.NewKeyFromSeed(strangerSeed)
	if _, err := SignManifest(release.ManifestBytes, stranger); err == nil {
		t.Fatal("a key outside the trust root was allowed to sign a release")
	}
}

// The signing seed is only ever read from the environment, and an unusable
// value is an error rather than a fallback. There is no "unsigned but marked"
// state anywhere in this path.
func TestSigningSeedDecodingFailsClosed(t *testing.T) {
	t.Parallel()

	valid := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	if _, err := DecodeSigningSeed("  " + valid + "\n"); err != nil {
		t.Fatalf("a valid seed with surrounding whitespace was rejected: %v", err)
	}
	for name, seed := range map[string]string{
		"empty":       "",
		"whitespace":  "   \n",
		"not base64":  "not-base-64-!!",
		"wrong size":  base64.StdEncoding.EncodeToString([]byte{1, 2, 3}),
		"private key": base64.StdEncoding.EncodeToString(make([]byte, ed25519.PrivateKeySize)),
	} {
		_, err := DecodeSigningSeed(seed)
		if err == nil {
			t.Errorf("%s seed was accepted", name)
			continue
		}
		if strings.Contains(err.Error(), seed) && seed != "" {
			t.Errorf("%s seed error echoes the secret material", name)
		}
	}
}

func TestSignatureDocumentRejectsMalformedShapes(t *testing.T) {
	release := newTestRelease(t)

	for name, replacement := range map[string][2]string{
		"wrong artifact kind": {`"artifact_kind": "cairn-release-signature"`, `"artifact_kind": "cairn-image-signature"`},
		"unsupported schema":  {`"schema_version": 1`, `"schema_version": 2`},
		"foreign algorithm":   {`"algorithm": "ed25519"`, `"algorithm": "rsa-pkcs1"`},
	} {
		tampered := strings.Replace(string(release.SigBytes), replacement[0], replacement[1], 1)
		if tampered == string(release.SigBytes) {
			t.Fatalf("%s: fixture does not contain %q", name, replacement[0])
		}
		if _, err := ParseSignature([]byte(tampered)); err == nil {
			t.Errorf("%s: signature document was accepted", name)
		}
	}
}
