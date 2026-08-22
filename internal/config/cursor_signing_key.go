package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
)

const derivedCursorSigningKeyDomain = "cairn-cursor-signing-key-v1\x00"
const minimumExplicitCursorSigningKeyBytes = 32

// resolveCursorSigningKey returns the one installation-wide key used by every
// opaque cursor. Development may derive a stable local key; deployed builds
// must configure one explicitly so cursors survive restarts and replicas.
func resolveCursorSigningKey(appEnv, explicit, databaseURL, analyzerAPIKey string) (string, error) {
	key := strings.TrimSpace(explicit)
	if key != "" {
		if len([]byte(key)) < minimumExplicitCursorSigningKeyBytes {
			return "", fmt.Errorf("CURSOR_SIGNING_KEY must be at least %d bytes", minimumExplicitCursorSigningKeyBytes)
		}
		return key, nil
	}

	if strings.ToLower(strings.TrimSpace(appEnv)) != "dev" {
		return "", fmt.Errorf("CURSOR_SIGNING_KEY is required when APP_ENV is not dev")
	}

	return deriveDevelopmentCursorSigningKey(databaseURL, analyzerAPIKey), nil
}

// deriveDevelopmentCursorSigningKey preserves the previous deterministic
// fallback for local development, where convenience is preferred over a
// separately managed secret.
func deriveDevelopmentCursorSigningKey(databaseURL, analyzerAPIKey string) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(derivedCursorSigningKeyDomain))
	writeCursorKeyPart(digest, strings.TrimSpace(databaseURL))
	writeCursorKeyPart(digest, strings.TrimSpace(analyzerAPIKey))
	return hex.EncodeToString(digest.Sum(nil))
}

func writeCursorKeyPart(digest hash.Hash, value string) {
	_, _ = fmt.Fprintf(digest, "%d:", len(value))
	_, _ = digest.Write([]byte(value))
}
