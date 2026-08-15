package config

import (
	"strings"
	"testing"
)

const (
	testReaderCursorKeyA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testReaderCursorKeyB = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

func TestResolveReaderCursorSigningKeyPrefersDedicatedKey(t *testing.T) {
	got, err := resolveReaderCursorSigningKey(
		"prod",
		testReaderCursorKeyA,
		testReaderCursorKeyB,
		"postgres://replica-a",
		"analyzer-a",
	)
	if err != nil || got != testReaderCursorKeyA {
		t.Fatalf("resolve dedicated key = %q, %v; want dedicated key", got, err)
	}
}

func TestResolveReaderCursorSigningKeyFallsBackToExplicitLinkKey(t *testing.T) {
	got, err := resolveReaderCursorSigningKey(
		"staging",
		"",
		testReaderCursorKeyA,
		"postgres://replica-a",
		"analyzer-a",
	)
	if err != nil || got != testReaderCursorKeyA {
		t.Fatalf("resolve Link fallback = %q, %v; want explicit Link key", got, err)
	}
}

func TestResolveReaderCursorSigningKeyIsStableAcrossReplicaCredentials(t *testing.T) {
	replicaA, err := resolveReaderCursorSigningKey(
		"prod", testReaderCursorKeyA, "", "postgres://replica-a", "analyzer-a",
	)
	if err != nil {
		t.Fatal(err)
	}
	replicaB, err := resolveReaderCursorSigningKey(
		"prod", testReaderCursorKeyA, "", "postgres://replica-b", "analyzer-b",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replicaA != replicaB || replicaA != testReaderCursorKeyA {
		t.Fatalf("replica keys = %q/%q, want one explicit stable key", replicaA, replicaB)
	}
}

func TestResolveReaderCursorSigningKeyRotationInvalidatesPreviousKey(t *testing.T) {
	oldKey, err := resolveReaderCursorSigningKey("prod", testReaderCursorKeyA, "", "database", "analyzer")
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := resolveReaderCursorSigningKey("prod", testReaderCursorKeyB, "", "database", "analyzer")
	if err != nil {
		t.Fatal(err)
	}
	if oldKey == newKey {
		t.Fatal("explicit Reader cursor key rotation did not change the effective key")
	}
}

func TestResolveReaderCursorSigningKeyRejectsWeakExplicitKeys(t *testing.T) {
	for _, tc := range []struct {
		name           string
		explicitReader string
		explicitLink   string
		wantEnv        string
	}{
		{name: "dedicated", explicitReader: "short-reader-key", wantEnv: "READER_CURSOR_SIGNING_KEY"},
		{name: "Link fallback", explicitLink: "short-link-key", wantEnv: "CURSOR_SIGNING_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveReaderCursorSigningKey(
				"prod", tc.explicitReader, tc.explicitLink, "database", "analyzer",
			)
			if err == nil || !strings.Contains(err.Error(), tc.wantEnv) || !strings.Contains(err.Error(), "32 bytes") {
				t.Fatalf("error = %v, want %s minimum-length rejection", err, tc.wantEnv)
			}
		})
	}
}

func TestResolveReaderCursorSigningKeyRejectsCredentialFallbackOutsideDev(t *testing.T) {
	_, err := resolveReaderCursorSigningKey(
		"prod", "", "", "postgres://postgres:postgres@localhost/webtag", "test-key",
	)
	if err == nil || !strings.Contains(err.Error(), "READER_CURSOR_SIGNING_KEY or CURSOR_SIGNING_KEY") {
		t.Fatalf("error = %v, want production explicit-key requirement", err)
	}
}

func TestResolveReaderCursorSigningKeyAllowsDevelopmentFallback(t *testing.T) {
	const (
		databaseURL = "postgres://service:local@database:5432/webtag"
		analyzerKey = "local-analyzer-key"
	)
	got, err := resolveReaderCursorSigningKey("dev", "", "", databaseURL, analyzerKey)
	if err != nil {
		t.Fatal(err)
	}
	want := deriveDevelopmentCursorSigningKey(databaseURL, analyzerKey)
	if got == "" || got != want {
		t.Fatalf("development fallback = %q, want %q", got, want)
	}
	if got == databaseURL || got == analyzerKey {
		t.Fatal("derived development cursor key exposed a source credential")
	}
}

func TestDevelopmentCursorSigningKeyBindsBothDeploymentCredentials(t *testing.T) {
	base := deriveDevelopmentCursorSigningKey("database-a", "analyzer-a")
	if got := deriveDevelopmentCursorSigningKey("database-b", "analyzer-a"); got == base {
		t.Fatal("database credential change did not rotate derived development key")
	}
	if got := deriveDevelopmentCursorSigningKey("database-a", "analyzer-b"); got == base {
		t.Fatal("analyzer credential change did not rotate derived development key")
	}
}
