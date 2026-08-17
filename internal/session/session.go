// Package session provides short-lived signed browser sessions stored in an
// HttpOnly cookie. Session claims carry two deadlines and nothing else; this
// single-installation build has no tenant, API-key, scope, or external identity
// projection.
//
// Two deadlines rather than one, because a single one cannot satisfy both
// halves of the requirement:
//
//   - ExpiresAt is the sliding deadline. Every authenticated request that finds
//     it close to running out pushes it back, so someone who keeps using the
//     Reader is never logged out mid-use.
//   - AbsoluteExpiresAt is the ceiling that sliding cannot cross. Without it a
//     stolen cookie stays alive forever, since the thief renews it by using it.
//     With it, one login is worth at most DefaultAbsoluteTTL no matter how
//     actively it is exercised.
//
// Session mode deliberately does not persist the installation token in the
// browser (see reader/src/lib/settings.ts), so an expiry the user did not
// intend is not a small annoyance: there is no credential left to recover with
// and the only way back is re-entering the token by hand.
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

// DefaultTTL is how long one session survives without being used.
const DefaultTTL = 12 * time.Hour

// DefaultAbsoluteTTL caps the total life of a single login. Renewal may push
// ExpiresAt forward repeatedly but never past this.
const DefaultAbsoluteTTL = 30 * 24 * time.Hour

// renewLeadFraction decides how early a request renews. Renewing on every
// request would put a Set-Cookie on nearly every response for no benefit;
// waiting until the last moment would drop anyone whose tab sat idle just
// over the line. Half the TTL means an active session is refreshed at most
// once every six hours and always has at least six hours of slack.
const renewLeadFraction = 2

var ErrInvalid = errors.New("session: credential is invalid or expired")

type Claims struct {
	// ExpiresAt is the sliding deadline, moved forward by Renew.
	ExpiresAt time.Time
	// AbsoluteExpiresAt is the ceiling renewal cannot cross. Zero means the
	// claim is not renewable — that is how a v3 token, minted before this
	// field existed, is represented.
	AbsoluteExpiresAt time.Time
}

const (
	// payloadVersion is what Sign emits. Parse also accepts legacyPayloadVersion
	// so that deploying this change does not invalidate the sessions already in
	// people's browsers — the very failure this change exists to stop.
	payloadVersion       = "v4"
	legacyPayloadVersion = "v3"
)

func Sign(claims Claims, key []byte) (string, error) {
	if len(key) == 0 {
		return "", errors.New("session: signing key is empty")
	}
	if claims.ExpiresAt.IsZero() {
		return "", ErrInvalid
	}
	absolute := claims.AbsoluteExpiresAt
	if absolute.IsZero() {
		absolute = claims.ExpiresAt
	}
	if absolute.Before(claims.ExpiresAt) {
		return "", ErrInvalid
	}
	payload := payloadVersion + "|" +
		strconv.FormatInt(claims.ExpiresAt.Unix(), 10) + "|" +
		strconv.FormatInt(absolute.Unix(), 10)
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
	claims, err := decodeClaims(parts)
	if err != nil {
		return Claims{}, err
	}
	if !now.Before(claims.ExpiresAt) {
		return Claims{}, fmt.Errorf("%w: expired at %s", ErrInvalid, claims.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if !claims.AbsoluteExpiresAt.IsZero() && !now.Before(claims.AbsoluteExpiresAt) {
		return Claims{}, fmt.Errorf("%w: absolute deadline passed at %s", ErrInvalid,
			claims.AbsoluteExpiresAt.UTC().Format(time.RFC3339))
	}
	return claims, nil
}

// decodeClaims reads either payload shape. A v3 token carries only the sliding
// deadline and comes back with a zero AbsoluteExpiresAt, which Renew reads as
// "not renewable": such a session runs out on its original schedule and the
// next login mints a v4 token that can slide.
func decodeClaims(parts []string) (Claims, error) {
	switch {
	case len(parts) == 3 && parts[0] == payloadVersion:
		expiresAt, err := unixField(parts[1])
		if err != nil {
			return Claims{}, err
		}
		absolute, err := unixField(parts[2])
		if err != nil {
			return Claims{}, err
		}
		if absolute.Before(expiresAt) {
			return Claims{}, ErrInvalid
		}
		return Claims{ExpiresAt: expiresAt, AbsoluteExpiresAt: absolute}, nil
	case len(parts) == 2 && parts[0] == legacyPayloadVersion:
		expiresAt, err := unixField(parts[1])
		if err != nil {
			return Claims{}, err
		}
		return Claims{ExpiresAt: expiresAt}, nil
	default:
		return Claims{}, ErrInvalid
	}
}

func unixField(raw string) (time.Time, error) {
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, ErrInvalid
	}
	return time.Unix(seconds, 0), nil
}

// Renew reports the claims a still-valid session should be re-signed with, and
// whether re-signing is worth doing at all.
//
// It declines in three cases, each for its own reason: the claim is a legacy
// non-renewable one; the deadline is still comfortably far away, so a
// Set-Cookie would be noise; or the absolute ceiling is close enough that
// sliding would gain nothing. In the last case the session is left to run out
// on schedule — that is the point of the ceiling.
func Renew(claims Claims, now time.Time, ttl time.Duration) (Claims, bool) {
	if claims.AbsoluteExpiresAt.IsZero() || ttl <= 0 {
		return Claims{}, false
	}
	if claims.ExpiresAt.Sub(now) > ttl/renewLeadFraction {
		return Claims{}, false
	}
	next := now.Add(ttl).Truncate(time.Second)
	if next.After(claims.AbsoluteExpiresAt) {
		next = claims.AbsoluteExpiresAt
	}
	if !next.After(claims.ExpiresAt) {
		return Claims{}, false
	}
	return Claims{ExpiresAt: next, AbsoluteExpiresAt: claims.AbsoluteExpiresAt}, true
}

func mac(encodedPayload string, key []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(encodedPayload))
	return h.Sum(nil)
}
