package middleware

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"webtag/internal/representation"
	"webtag/internal/session"
)

// IdentityReader resolves the stable installation identity namespace.
type IdentityReader interface {
	Current(context.Context) (representation.ClientIdentity, error)
}

// InstallationAuthenticator validates the one static credential configured
// for this installation. Bearer authentication and session exchange share it.
type InstallationAuthenticator struct {
	token []byte
}

func NewInstallationAuthenticator(token string) *InstallationAuthenticator {
	return &InstallationAuthenticator{token: []byte(token)}
}

func (a *InstallationAuthenticator) Authenticate(plaintext string) bool {
	return a != nil && len(a.token) > 0 && len(plaintext) == len(a.token) &&
		subtle.ConstantTimeCompare([]byte(plaintext), a.token) == 1
}

func (a *InstallationAuthenticator) Configured() bool {
	return a != nil && len(a.token) > 0
}

type PublicAuthOptions struct {
	Authenticator  *InstallationAuthenticator
	SessionKey     []byte
	IdentityReader IdentityReader
	// SessionTTL is the window each renewal grants. Zero falls back to
	// session.DefaultTTL so a caller that does not care still renews.
	SessionTTL time.Duration
}

// PublicAuth accepts the static Bearer token or a valid HttpOnly session
// accompanied by the CSRF header. Every successful path enters the same
// installation namespace.
func PublicAuth(opts PublicAuthOptions) gin.HandlerFunc {
	return func(c *gin.Context) {
		authorization := strings.TrimSpace(c.GetHeader("Authorization"))
		if authorization != "" {
			token, ok := bearerToken(authorization)
			if ok && opts.Authenticator.Authenticate(token) {
				allowInstallation(c, opts.IdentityReader)
				return
			}
			rejectPublicAuth(c)
			return
		}
		if handled := tryInstallationSession(c, opts); handled {
			return
		}
		rejectPublicAuth(c)
	}
}

func bearerToken(header string) (string, bool) {
	fields := strings.Fields(header)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || fields[1] == "" {
		return "", false
	}
	return fields[1], true
}

func tryInstallationSession(c *gin.Context, opts PublicAuthOptions) bool {
	if len(opts.SessionKey) == 0 || c.GetHeader(session.HeaderName) == "" {
		return false
	}
	cookie, err := c.Request.Cookie(session.CookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	now := time.Now()
	claims, err := session.Parse(cookie.Value, opts.SessionKey, now)
	if err != nil {
		ClearSessionCookie(c)
		rejectPublicAuth(c)
		return true
	}
	renewSessionCookie(c, opts, claims, now)
	allowInstallation(c, opts.IdentityReader)
	return true
}

// renewSessionCookie pushes the sliding deadline forward when the session is
// close to running out.
//
// Without this the cookie expires on a fixed schedule no matter how actively
// it is used, and because session mode keeps no installation token in the
// browser there is nothing left to re-authenticate with — the Reader can only
// show "unauthorized" and ask for the token again. Renewal is best effort: a
// signing failure here must not fail a request that is already authenticated,
// since the existing cookie is still valid for a while yet.
func renewSessionCookie(c *gin.Context, opts PublicAuthOptions, claims session.Claims, now time.Time) {
	ttl := opts.SessionTTL
	if ttl <= 0 {
		ttl = session.DefaultTTL
	}
	renewed, ok := session.Renew(claims, now, ttl)
	if !ok {
		return
	}
	token, err := session.Sign(renewed, opts.SessionKey)
	if err != nil {
		return
	}
	SetSessionCookie(c, token, renewed.ExpiresAt)
}

func allowInstallation(c *gin.Context, identities IdentityReader) {
	if identities == nil {
		JSONErrorWithSlug(c, http.StatusInternalServerError, ErrCodeInternalError, "installation namespace is unavailable")
		return
	}
	identity, err := identities.Current(c.Request.Context())
	if err != nil {
		JSONErrorWithSlug(c, http.StatusInternalServerError, ErrCodeInternalError, "installation namespace is unavailable")
		return
	}
	if !identity.Valid() {
		JSONErrorWithSlug(c, http.StatusInternalServerError, ErrCodeInternalError, "installation namespace is unavailable")
		return
	}
	ctx := representation.WithClientIdentity(c.Request.Context(), identity)
	c.Request = c.Request.WithContext(ctx)
	c.Header(representation.DataNamespaceHeader, identity.ClientDataNamespace)
	c.Next()
}

func rejectPublicAuth(c *gin.Context) {
	c.Header("WWW-Authenticate", `Bearer realm="api"`)
	JSONErrorWithSlug(c, http.StatusUnauthorized, ErrCodeUnauthorized, "unauthorized")
}
