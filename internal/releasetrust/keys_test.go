package releasetrust

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

func syntheticKey(t *testing.T, keyID string, notBefore, notAfter time.Time) TrustedKey {
	t.Helper()
	_, key := newTestSigningKey(t, keyID)
	key.NotBefore = notBefore
	key.NotAfter = notAfter
	return key
}

func TestProductionTrustRootSatisfiesTheRotationContract(t *testing.T) {
	t.Parallel()

	keys := TrustedKeys()
	if err := ValidateKeySet(keys); err != nil {
		t.Fatalf("compiled-in trust root violates the rotation contract: %v", err)
	}
	if len(keys) == 0 {
		t.Fatal("the compiled-in trust root is empty")
	}
	for _, key := range keys {
		if len(key.PublicKey) != ed25519.PublicKeySize {
			t.Errorf("key %s is not a 32 byte Ed25519 public key", key.KeyID)
		}
	}
}

// TrustedKeys must hand out a copy. A caller that could append to the real
// slice would be able to add a trust root at runtime, which is exactly what
// compiling the keys in is meant to prevent.
func TestTrustedKeysCannotBeMutatedThroughTheAccessor(t *testing.T) {
	t.Parallel()

	before := len(trustedKeys)
	keys := TrustedKeys()
	keys = append(keys, TrustedKey{KeyID: "cairn-release-9999z"})
	keys[0].KeyID = "tampered"
	if len(trustedKeys) != before || trustedKeys[0].KeyID == "tampered" {
		t.Fatal("TrustedKeys exposed the real trust root")
	}
}

func TestKeySetRotationRules(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	overlapping := start.Add(365 * 24 * time.Hour)
	retire := overlapping.Add(MinimumRotationOverlap)

	valid := []TrustedKey{
		syntheticKey(t, "cairn-release-2026a", start, retire),
		syntheticKey(t, "cairn-release-2027b", overlapping, time.Time{}),
	}
	if err := ValidateKeySet(valid); err != nil {
		t.Fatalf("a correct rotation was rejected: %v", err)
	}

	for name, testCase := range map[string]struct {
		keys []TrustedKey
		want string
	}{
		"empty trust root": {
			keys: nil,
			want: "trust root is empty",
		},
		"malformed key id": {
			keys: []TrustedKey{syntheticKey(t, "release-key", start, time.Time{})},
			want: "does not match cairn-release",
		},
		"reused key id": {
			keys: []TrustedKey{
				syntheticKey(t, "cairn-release-2026a", start, retire),
				syntheticKey(t, "cairn-release-2026a", overlapping, time.Time{}),
			},
			want: "is reused",
		},
		"reused key material": {
			keys: func() []TrustedKey {
				first := syntheticKey(t, "cairn-release-2026a", start, retire)
				second := syntheticKey(t, "cairn-release-2027b", overlapping, time.Time{})
				second.PublicKey = first.PublicKey
				return []TrustedKey{first, second}
			}(),
			want: "reuses the public key material",
		},
		"rotation gap": {
			keys: []TrustedKey{
				syntheticKey(t, "cairn-release-2026a", start, overlapping),
				syntheticKey(t, "cairn-release-2027b", overlapping.Add(time.Hour), time.Time{}),
			},
			want: "leave a gap",
		},
		"overlap shorter than a release cycle": {
			keys: []TrustedKey{
				syntheticKey(t, "cairn-release-2026a", start, overlapping.Add(time.Hour)),
				syntheticKey(t, "cairn-release-2027b", overlapping, time.Time{}),
			},
			want: "overlap for only",
		},
		"two open ended keys": {
			keys: []TrustedKey{
				syntheticKey(t, "cairn-release-2026a", start, time.Time{}),
				syntheticKey(t, "cairn-release-2027b", overlapping, time.Time{}),
			},
			want: "only the current key may be open ended",
		},
		"no current key": {
			keys: []TrustedKey{syntheticKey(t, "cairn-release-2026a", start, retire)},
			want: "is not open ended",
		},
		"out of order": {
			keys: []TrustedKey{
				syntheticKey(t, "cairn-release-2027b", overlapping, retire),
				syntheticKey(t, "cairn-release-2026a", start, time.Time{}),
			},
			want: "must be ordered oldest first",
		},
		"undocumented key": {
			keys: func() []TrustedKey {
				key := syntheticKey(t, "cairn-release-2026a", start, time.Time{})
				key.Note = ""
				return []TrustedKey{key}
			}(),
			want: "does not record why it exists",
		},
	} {
		err := ValidateKeySet(testCase.keys)
		if err == nil {
			t.Errorf("%s: key set was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: error %q does not mention %q", name, err, testCase.want)
		}
	}
}

// A key is selected by the manifest's *signed* build time, not by wall clock.
// An old release therefore stays verifiable forever, and moving the host clock
// cannot widen a retired key's window.
func TestKeyValidityIsEvaluatedAgainstTheSignedBuildTime(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	retire := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	installTestTrustRoot(t, []TrustedKey{
		syntheticKey(t, "cairn-release-2026a", start, retire),
		syntheticKey(t, "cairn-release-2027b", retire.Add(-MinimumRotationOverlap), time.Time{}),
	})

	for name, testCase := range map[string]struct {
		keyID     string
		buildTime time.Time
		accepted  bool
	}{
		"retired key inside its window":  {"cairn-release-2026a", start.Add(time.Hour), true},
		"retired key after its window":   {"cairn-release-2026a", retire.Add(time.Hour), false},
		"retired key before its window":  {"cairn-release-2026a", start.Add(-time.Hour), false},
		"current key during the overlap": {"cairn-release-2027b", retire.Add(-time.Hour), true},
		"current key far in the future":  {"cairn-release-2027b", retire.AddDate(10, 0, 0), true},
		"unknown key":                    {"cairn-release-2099z", start.Add(time.Hour), false},
	} {
		_, err := LookupTrustedKey(testCase.keyID, testCase.buildTime)
		if testCase.accepted && err != nil {
			t.Errorf("%s: rejected: %v", name, err)
		}
		if !testCase.accepted && err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
