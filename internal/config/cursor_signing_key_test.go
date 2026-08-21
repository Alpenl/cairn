package config

import (
	"strings"
	"testing"
)

const (
	testCursorKeyA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testCursorKeyB = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

func TestResolveCursorSigningKeyUsesExplicitKeyAcrossReplicas(t *testing.T) {
	replicaA, err := resolveCursorSigningKey("prod", testCursorKeyA, "postgres://replica-a", "analyzer-a")
	if err != nil {
		t.Fatal(err)
	}
	replicaB, err := resolveCursorSigningKey("prod", testCursorKeyA, "postgres://replica-b", "analyzer-b")
	if err != nil {
		t.Fatal(err)
	}
	if replicaA != replicaB || replicaA != testCursorKeyA {
		t.Fatalf("replica keys = %q/%q, want one explicit stable key", replicaA, replicaB)
	}
}

func TestResolveCursorSigningKeyRotationChangesEffectiveKey(t *testing.T) {
	oldKey, err := resolveCursorSigningKey("prod", testCursorKeyA, "database", "analyzer")
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := resolveCursorSigningKey("prod", testCursorKeyB, "database", "analyzer")
	if err != nil {
		t.Fatal(err)
	}
	if oldKey == newKey {
		t.Fatal("cursor key rotation did not change the effective key")
	}
}

func TestResolveCursorSigningKeyRejectsWeakExplicitKey(t *testing.T) {
	_, err := resolveCursorSigningKey("prod", "short-key", "database", "analyzer")
	if err == nil || !strings.Contains(err.Error(), "CURSOR_SIGNING_KEY") || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("error = %v, want minimum-length rejection", err)
	}
}

func TestResolveCursorSigningKeyRejectsCredentialFallbackOutsideDev(t *testing.T) {
	_, err := resolveCursorSigningKey("prod", "", "postgres://postgres:postgres@localhost/webtag", "test-key")
	if err == nil || !strings.Contains(err.Error(), "CURSOR_SIGNING_KEY") {
		t.Fatalf("error = %v, want production explicit-key requirement", err)
	}
}

func TestResolveCursorSigningKeyAllowsDevelopmentFallback(t *testing.T) {
	const (
		databaseURL = "postgres://service:local@database:5432/webtag"
		analyzerKey = "local-analyzer-key"
	)
	got, err := resolveCursorSigningKey("dev", "", databaseURL, analyzerKey)
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
