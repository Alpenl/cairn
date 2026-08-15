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

// resolveReaderCursorSigningKey keeps Reader/Thought cursors independent from
// deployment credentials in every non-development environment. CURSOR_SIGNING_KEY
// remains a compatibility fallback for operators that already use one explicit
// key for both Link and Reader cursors.
func resolveReaderCursorSigningKey(
	appEnv, explicitReader, explicitLink, databaseURL, analyzerAPIKey string,
) (string, error) {
	for _, candidate := range []struct {
		name  string
		value string
	}{
		{name: "READER_CURSOR_SIGNING_KEY", value: explicitReader},
		{name: "CURSOR_SIGNING_KEY", value: explicitLink},
	} {
		key := strings.TrimSpace(candidate.value)
		if key == "" {
			continue
		}
		if len([]byte(key)) < minimumExplicitCursorSigningKeyBytes {
			return "", fmt.Errorf("%s must be at least %d bytes", candidate.name, minimumExplicitCursorSigningKeyBytes)
		}
		return key, nil
	}

	if strings.ToLower(strings.TrimSpace(appEnv)) != "dev" {
		return "", fmt.Errorf("READER_CURSOR_SIGNING_KEY or CURSOR_SIGNING_KEY is required when APP_ENV is not dev")
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
