package releasetrust

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
)

// The signing key rotation contract.
//
// Public keys are compiled in. That is the whole point: the helper must not be
// able to learn a new trust root from the network, from a Release asset, or
// from a configuration file an application compromise could reach. Rotating the
// trust root means shipping a new helper build.
//
// Rules, all enforced by ValidateKeySet:
//
//   - A key is identified by key_id, which is written into the detached
//     signature. Verification selects the key by key_id and then requires the
//     signature to verify under exactly that key. A key_id is never reused,
//     even for the same key material.
//   - Every key declares a validity window [NotBefore, NotAfter). The window is
//     evaluated against the manifest's signed build_time, not against wall
//     clock time, so an old release stays verifiable forever and an attacker
//     cannot widen a window by moving a clock.
//   - Exactly one key is open ended (zero NotAfter). That is the current
//     signing key, and it must be the last entry.
//   - Windows must overlap by at least MinimumRotationOverlap. This is the
//     "both keys live for one release cycle" rule in mechanical form: the new
//     key starts signing while the old key is still trusted, so a helper that
//     has not been updated yet can still verify the releases produced during
//     the changeover.
//   - Windows must not leave a gap. A gap means some build_time verifies under
//     no key at all, which would be indistinguishable from an unsigned release.
//
// Retiring a key is therefore a two-step operation: first add the successor
// with NotBefore set to its first release and set the incumbent's NotAfter at
// least MinimumRotationOverlap later, ship that helper; only after the overlap
// has elapsed does the old key stop being used for signing.

// MinimumRotationOverlap is how long an outgoing and an incoming signing key
// must both be trusted. One release cycle is the floor: a helper that has not
// been rebuilt yet still has to be able to verify releases produced during the
// changeover.
const MinimumRotationOverlap = 30 * 24 * time.Hour

var keyIDPattern = regexp.MustCompile(`^cairn-release-[0-9]{4}[a-z]$`)

// TrustedKey is one compiled-in Ed25519 release signing key.
type TrustedKey struct {
	// KeyID is the stable identifier written into the detached signature.
	KeyID string
	// PublicKey is the 32-byte Ed25519 public key.
	PublicKey ed25519.PublicKey
	// NotBefore is the first release build_time this key may sign.
	NotBefore time.Time
	// NotAfter is the exclusive end of the window. The zero value means the
	// key is current and open ended.
	NotAfter time.Time
	// Note records why the key exists and how it was produced. It is
	// documentation, never an input to verification.
	Note string
}

// Active reports whether the key may sign a release built at buildTime.
func (key TrustedKey) Active(buildTime time.Time) bool {
	if buildTime.Before(key.NotBefore) {
		return false
	}
	return key.NotAfter.IsZero() || buildTime.Before(key.NotAfter)
}

func mustDecodePublicKey(encoded string) ed25519.PublicKey {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		panic("releasetrust: trusted public key is not base64: " + err.Error())
	}
	if len(raw) != ed25519.PublicKeySize {
		panic(fmt.Sprintf("releasetrust: trusted public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize))
	}
	return ed25519.PublicKey(raw)
}

// trustedKeys is the compiled-in trust root, ordered oldest first.
//
// Only the public half is here, and only the public half ever may be. The
// private seed lives exclusively in the release workflow's
// CAIRN_RELEASE_SIGNING_KEY secret.
var trustedKeys = []TrustedKey{
	{
		KeyID:     "cairn-release-2026a",
		PublicKey: mustDecodePublicKey("jU8lfNUpa25v5cmBWA1pjjPjsGt9+eRS0zfvw09FoeM="),
		NotBefore: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		Note: "Bootstrap release signing key for issue #41 stage 0. " +
			"Rotate to a successor generated on the maintainer's own machine " +
			"before the first production page-triggered update.",
	},
}

// TrustedKeys returns a copy of the compiled-in trust root. Callers cannot
// mutate the real set, which keeps "add a key at runtime" from being possible
// even by accident.
func TrustedKeys() []TrustedKey {
	return slices.Clone(trustedKeys)
}

// LookupTrustedKey selects the key a signature names and checks that it was
// valid for the release's signed build_time.
func LookupTrustedKey(keyID string, buildTime time.Time) (TrustedKey, error) {
	for _, key := range trustedKeys {
		if key.KeyID != keyID {
			continue
		}
		if !key.Active(buildTime) {
			return TrustedKey{}, fmt.Errorf("release signing key %s was not valid at build time %s",
				keyID, buildTime.UTC().Format(time.RFC3339))
		}
		return key, nil
	}
	return TrustedKey{}, fmt.Errorf("release signing key %s is not a trusted key", keyID)
}

// TrustedKeyForPublicKey finds the trusted entry a private key belongs to. The
// signer uses it so the key_id is derived from the secret rather than declared
// next to it: a mislabelled secret then fails at signing time instead of
// producing a signature no helper can attribute.
func TrustedKeyForPublicKey(publicKey ed25519.PublicKey, buildTime time.Time) (TrustedKey, error) {
	for _, key := range trustedKeys {
		if !key.PublicKey.Equal(publicKey) {
			continue
		}
		if !key.Active(buildTime) {
			return TrustedKey{}, fmt.Errorf("release signing key %s was not valid at build time %s",
				key.KeyID, buildTime.UTC().Format(time.RFC3339))
		}
		return key, nil
	}
	return TrustedKey{}, errors.New("the supplied signing key is not part of the compiled-in trust root")
}

// ValidateKeySet enforces the rotation contract on any candidate key set. The
// production trust root is checked with it, and so are the synthetic sets in
// the tests, so the rules are exercised even while only one real key exists.
//
//nolint:gocyclo // One branch per rotation rule; merging them would blur which rule failed.
func ValidateKeySet(keys []TrustedKey) error {
	if len(keys) == 0 {
		return errors.New("the trust root is empty: no release could ever be verified")
	}
	seenIDs := map[string]struct{}{}
	seenKeys := map[string]struct{}{}
	for index, key := range keys {
		if !keyIDPattern.MatchString(key.KeyID) {
			return fmt.Errorf("key %d id %q does not match cairn-release-YYYYx", index, key.KeyID)
		}
		if _, duplicate := seenIDs[key.KeyID]; duplicate {
			return fmt.Errorf("key id %q is reused", key.KeyID)
		}
		seenIDs[key.KeyID] = struct{}{}
		if len(key.PublicKey) != ed25519.PublicKeySize {
			return fmt.Errorf("key %s is %d bytes, want %d", key.KeyID, len(key.PublicKey), ed25519.PublicKeySize)
		}
		material := base64.StdEncoding.EncodeToString(key.PublicKey)
		if _, duplicate := seenKeys[material]; duplicate {
			return fmt.Errorf("key %s reuses the public key material of an earlier entry", key.KeyID)
		}
		seenKeys[material] = struct{}{}
		if key.NotBefore.IsZero() {
			return fmt.Errorf("key %s has no validity start", key.KeyID)
		}
		if !key.NotAfter.IsZero() && !key.NotBefore.Before(key.NotAfter) {
			return fmt.Errorf("key %s ends before it starts", key.KeyID)
		}
		if key.Note == "" {
			return fmt.Errorf("key %s does not record why it exists", key.KeyID)
		}
	}
	for index := 1; index < len(keys); index++ {
		previous, current := keys[index-1], keys[index]
		if !previous.NotBefore.Before(current.NotBefore) {
			return fmt.Errorf("key %s does not start after %s: the trust root must be ordered oldest first",
				current.KeyID, previous.KeyID)
		}
		if previous.NotAfter.IsZero() {
			return fmt.Errorf("key %s is open ended but is followed by %s: only the current key may be open ended",
				previous.KeyID, current.KeyID)
		}
		if current.NotBefore.After(previous.NotAfter) {
			return fmt.Errorf("keys %s and %s leave a gap: releases built between them verify under no key",
				previous.KeyID, current.KeyID)
		}
		if overlap := previous.NotAfter.Sub(current.NotBefore); overlap < MinimumRotationOverlap {
			return fmt.Errorf("keys %s and %s overlap for only %s, want at least %s so both stay trusted for a release cycle",
				previous.KeyID, current.KeyID, overlap, MinimumRotationOverlap)
		}
	}
	if last := keys[len(keys)-1]; !last.NotAfter.IsZero() {
		return fmt.Errorf("current key %s is not open ended: no key could sign the next release", last.KeyID)
	}
	return nil
}
