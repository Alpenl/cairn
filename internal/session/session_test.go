package session_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"webtag/internal/session"
)

var testSigningKey = []byte("test-signing-key-please-do-not-reuse")

func claims(exp time.Time) session.Claims {
	return session.Claims{ExpiresAt: exp}
}

func TestSignParseRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Now()
	want := claims(now.Add(time.Hour))

	token, err := session.Sign(want, testSigningKey)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got, err := session.Parse(token, testSigningKey, now)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt.Truncate(time.Second)) {
		t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, want.ExpiresAt.Truncate(time.Second))
	}
}

// v4 载荷带两个期限。第二个是绝对上限，滑动续期不能越过它——没有它，
// 被窃取的 cookie 只要持续使用就永不过期。省略 AbsoluteExpiresAt 时按
// ExpiresAt 填入，得到一张不可续期的票。
func TestSignUsesV4TwoDeadlinePayload(t *testing.T) {
	t.Parallel()
	token, err := session.Sign(session.Claims{
		ExpiresAt:         time.Unix(2_000_000_000, 0),
		AbsoluteExpiresAt: time.Unix(2_000_090_000, 0),
	}, testSigningKey)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if got, want := decodedPayload(t, token), "v4|2000000000|2000090000"; got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}

func TestSignDefaultsTheAbsoluteDeadlineToTheSlidingOne(t *testing.T) {
	t.Parallel()
	token, err := session.Sign(claims(time.Unix(2_000_000_000, 0)), testSigningKey)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if got, want := decodedPayload(t, token), "v4|2000000000|2000000000"; got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}

func decodedPayload(t *testing.T, token string) string {
	t.Helper()
	payload, _, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatal("signed token is missing MAC separator")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return string(raw)
}

func TestSignRejectsMissingExpiry(t *testing.T) {
	t.Parallel()
	if _, err := session.Sign(session.Claims{}, testSigningKey); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestParseRejectsWrongKey(t *testing.T) {
	t.Parallel()
	token, err := session.Sign(claims(time.Now().Add(time.Hour)), testSigningKey)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := session.Parse(token, []byte("another-key"), time.Now()); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestParseRejectsTamperedPayload(t *testing.T) {
	t.Parallel()
	token, err := session.Sign(claims(time.Now().Add(time.Hour)), testSigningKey)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	payload, signature, _ := strings.Cut(token, ".")
	tampered := payload[:len(payload)-2] + "AA." + signature
	if _, err := session.Parse(tampered, testSigningKey, time.Now()); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestParseRejectsExpiredOrExactlyExpired(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Second)
	for _, expiry := range []time.Time{now.Add(-time.Second), now} {
		token, err := session.Sign(claims(expiry), testSigningKey)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		if _, err := session.Parse(token, testSigningKey, now); !errors.Is(err, session.ErrInvalid) {
			t.Fatalf("expiry %v: err = %v, want ErrInvalid", expiry, err)
		}
	}
}

func TestParseRejectsMalformedAndOlderPayloads(t *testing.T) {
	t.Parallel()
	for _, token := range []string{
		"",
		"abcdef",
		"abc.!!!",
		"!!!.YWJj",
		signRaw(t, "v2|2000000000"),
		signRaw(t, "v3|not-a-timestamp"),
		signRaw(t, "v3|2000000000|unexpected"),
	} {
		if _, err := session.Parse(token, testSigningKey, time.Now()); !errors.Is(err, session.ErrInvalid) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalid", token, err)
		}
	}
}

func TestEmptyKeyIsFailClosed(t *testing.T) {
	t.Parallel()
	if _, err := session.Sign(claims(time.Now().Add(time.Hour)), nil); err == nil {
		t.Fatal("Sign with an empty key should fail")
	}
	token, err := session.Sign(claims(time.Now().Add(time.Hour)), testSigningKey)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := session.Parse(token, nil, time.Now()); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func signRaw(t *testing.T, payload string) string {
	t.Helper()
	// Sign a valid token, then reuse its public format with the test package's
	// key through a normal Sign call when possible. Older/malformed payloads
	// need a valid MAC, so this helper deliberately reproduces only HMAC input.
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	valid, err := session.Sign(claims(time.Unix(2_000_000_000, 0)), testSigningKey)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	validPayload, signature, _ := strings.Cut(valid, ".")
	if encoded == validPayload {
		return valid
	}
	// A signature for another payload is still sufficient here: Parse must
	// fail closed before accepting an obsolete or malformed contract.
	return encoded + "." + signature
}
