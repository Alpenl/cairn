package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"webtag/internal/representation"
	"webtag/internal/session"
)

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
	Authenticator   *InstallationAuthenticator
	AllowOpenAccess bool
	SessionKey      []byte
	Representations VersionReader
}

// PublicAuth accepts the static Bearer token, an explicitly enabled anonymous
// request, or a valid HttpOnly session accompanied by the CSRF header. Every
// successful path enters the same installation namespace.
func PublicAuth(opts PublicAuthOptions) gin.HandlerFunc {
	return func(c *gin.Context) {
		authorization := strings.TrimSpace(c.GetHeader("Authorization"))
		if authorization != "" {
			token, ok := bearerToken(authorization)
			if ok && opts.Authenticator.Authenticate(token) {
				allowInstallation(c, opts.Representations)
				return
			}
			rejectPublicAuth(c)
			return
		}
		if handled := tryInstallationSession(c, opts); handled {
			return
		}
		if opts.AllowOpenAccess {
			allowInstallation(c, opts.Representations)
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
	if _, err := session.Parse(cookie.Value, opts.SessionKey, time.Now()); err != nil {
		ClearSessionCookie(c)
		rejectPublicAuth(c)
		return true
	}
	allowInstallation(c, opts.Representations)
	return true
}

func allowInstallation(c *gin.Context, versions VersionReader) {
	if versions == nil {
		JSONErrorWithSlug(c, http.StatusInternalServerError, ErrCodeInternalError, "installation namespace is unavailable")
		return
	}
	components, err := representation.NewComponentSet()
	if err != nil {
		JSONErrorWithSlug(c, http.StatusInternalServerError, ErrCodeInternalError, "installation namespace is unavailable")
		return
	}
	base, err := versions.Current(c.Request.Context(), components)
	if err != nil {
		JSONErrorWithSlug(c, http.StatusInternalServerError, ErrCodeInternalError, "installation namespace is unavailable")
		return
	}
	identity, err := representation.NewClientIdentity(base)
	if err != nil {
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
