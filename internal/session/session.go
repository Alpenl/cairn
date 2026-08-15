// Package session provides short-lived signed browser sessions stored in an
// HttpOnly cookie. Session claims contain only expiry; this single-installation
// build has no tenant, API-key, scope, or external identity projection.
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const CookieName = "webtag_session"
const HeaderName = "X-WebTag-Session"
const DefaultTTL = 12 * time.Hour

var ErrInvalid = errors.New("session: credential is invalid or expired")

type Claims struct {
	ExpiresAt time.Time
}

const payloadVersion = "v3"

func Sign(claims Claims, key []byte) (string, error) {
	if len(key) == 0 {
		return "", errors.New("session: signing key is empty")
	}
	if claims.ExpiresAt.IsZero() {
		return "", ErrInvalid
	}
	payload := payloadVersion + "|" + strconv.FormatInt(claims.ExpiresAt.Unix(), 10)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac(encoded, key)), nil
}

func Parse(token string, key []byte, now time.Time) (Claims, error) {
	if len(key) == 0 {
		return Claims{}, ErrInvalid
	}
	encoded, signature, ok := strings.Cut(token, ".")
	if !ok {
		return Claims{}, ErrInvalid
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || subtle.ConstantTimeCompare(gotMAC, mac(encoded, key)) != 1 {
		return Claims{}, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Claims{}, ErrInvalid
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 2 || parts[0] != payloadVersion {
		return Claims{}, ErrInvalid
	}
	expiresUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return Claims{}, ErrInvalid
	}
	expiresAt := time.Unix(expiresUnix, 0)
	if !now.Before(expiresAt) {
		return Claims{}, fmt.Errorf("%w: expired at %s", ErrInvalid, expiresAt.UTC().Format(time.RFC3339))
	}
	return Claims{ExpiresAt: expiresAt}, nil
}

func mac(encodedPayload string, key []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(encodedPayload))
	return h.Sum(nil)
}
