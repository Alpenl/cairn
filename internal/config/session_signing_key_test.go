package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
)

type failingSessionKeyReader struct{}

func (failingSessionKeyReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy unavailable")
}

func TestSessionSigningKeyProviderGeneratesOneProcessStableKey(t *testing.T) {
	provider := &sessionSigningKeyProvider{source: bytes.NewReader(bytes.Repeat([]byte{0x5a}, 64))}
	first, err := provider.keyForProcess()
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.keyForProcess()
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Fatalf("ephemeral keys = %q and %q, want one stable non-empty process key", first, second)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(first)
	if err != nil || len(decoded) != ephemeralSessionSigningKeyBytes {
		t.Fatalf("decoded key length/error = %d/%v, want %d bytes", len(decoded), err, ephemeralSessionSigningKeyBytes)
	}
}

func TestSessionSigningKeyProviderCachesEntropyFailure(t *testing.T) {
	provider := &sessionSigningKeyProvider{source: failingSessionKeyReader{}}
	if key, err := provider.keyForProcess(); key != "" || err == nil {
		t.Fatalf("key/error = %q/%v, want startup-blocking failure", key, err)
	}
	if key, err := provider.keyForProcess(); key != "" || err == nil {
		t.Fatalf("second key/error = %q/%v, want cached startup-blocking failure", key, err)
	}
}

func TestResolveSessionSigningKeyPreservesExplicitValue(t *testing.T) {
	const explicit = "operator-controlled-key"
	got, ephemeral, err := resolveSessionSigningKey(explicit)
	if err != nil || got != explicit {
		t.Fatalf("resolve explicit key = %q, %v", got, err)
	}
	if ephemeral {
		t.Fatal("an explicitly configured key must not be reported as ephemeral")
	}
}

// 空值必须被如实标记为 ephemeral：启动 WARN 完全建立在这个返回值上，
// 一旦这里谎报 false，那条「重启即掉线」的提示就永远不会打印，而它正是
// 这个静默陷阱唯一的对外信号。
func TestResolveSessionSigningKeyReportsTheEphemeralFallback(t *testing.T) {
	got, ephemeral, err := resolveSessionSigningKey("")
	if err != nil || got == "" {
		t.Fatalf("resolve empty key = %q, %v, want a generated key", got, err)
	}
	if !ephemeral {
		t.Fatal("a generated per-process key must be reported as ephemeral")
	}
}
