package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/middleware"
	"webtag/internal/representation"
	"webtag/internal/session"
)

// SessionOptions configures browser session exchange for one installation.
type SessionOptions struct {
	SigningKey     []byte
	TTL            time.Duration
	Authenticator  *middleware.InstallationAuthenticator
	IdentityReader middleware.IdentityReader
}

type sessionCreateRequest struct {
	Token string `json:"token" binding:"max=512"`
}

// RegisterSessionExchangeRoutes mounts the login and logout endpoints outside
// the protected group. The login exchanges the static installation token for
// a short-lived HttpOnly cookie.
func RegisterSessionExchangeRoutes(router gin.IRoutes, opts SessionOptions) {
	if len(opts.SigningKey) == 0 || opts.Authenticator == nil || !opts.Authenticator.Configured() {
		return
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = session.DefaultTTL
	}
	router.POST(apiRoutePrefix+"/session", createSession(opts, ttl))
	router.DELETE(apiRoutePrefix+"/session", deleteSession())
}

func RegisterSessionIdentityRoute(router gin.IRoutes) {
	router.GET(apiRoutePrefix+"/session", getSessionIdentity())
}

func createSession(opts SessionOptions, ttl time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req sessionCreateRequest
		if !bindJSONWithLimit(c, &req, defaultMaxJSONBodyBytes) {
			return
		}
		if !opts.Authenticator.Authenticate(req.Token) {
			middleware.JSONErrorWithSlug(c, http.StatusUnauthorized, middleware.ErrCodeUnauthorized, "installation token is invalid")
			return
		}

		identity, err := resolveInstallationIdentity(c, opts.IdentityReader)
		if err != nil {
			middleware.JSONErrorWithSlug(c, http.StatusInternalServerError, middleware.ErrCodeInternalError, "installation namespace is unavailable")
			return
		}
		now := time.Now()
		expiresAt := now.Add(ttl).Truncate(time.Second)
		// The absolute deadline is fixed at login and never moves: renewal may
		// slide ExpiresAt forward many times, but one login is worth at most
		// this much wall-clock no matter how actively the cookie is used.
		absoluteExpiresAt := now.Add(session.DefaultAbsoluteTTL).Truncate(time.Second)
		token, err := session.Sign(session.Claims{
			ExpiresAt:         expiresAt,
			AbsoluteExpiresAt: absoluteExpiresAt,
		}, opts.SigningKey)
		if err != nil {
			middleware.JSONErrorWithSlug(c, http.StatusInternalServerError, middleware.ErrCodeInternalError, "failed to create session")
			return
		}

		middleware.SetSessionCookie(c, token, expiresAt)
		setIdentityResponseHeaders(c, identity.ClientDataNamespace)
		c.JSON(http.StatusCreated, dto.SessionCreated{
			ExpiresAt:              expiresAt,
			ClientDataNamespace:    identity.ClientDataNamespace,
			RepresentationContract: representation.Contract,
		})
	}
}

func getSessionIdentity() gin.HandlerFunc {
	return func(c *gin.Context) {
		identity, ok := representation.ClientIdentityFromContext(c.Request.Context())
		if !ok {
			middleware.JSONErrorWithSlug(c, http.StatusInternalServerError, middleware.ErrCodeInternalError, "installation namespace is unavailable")
			return
		}
		response := dto.SessionIdentity{
			ClientDataNamespace:    identity.ClientDataNamespace,
			RepresentationContract: representation.Contract,
		}
		setIdentityResponseHeaders(c, response.ClientDataNamespace)
		c.JSON(http.StatusOK, response)
	}
}

func resolveInstallationIdentity(c *gin.Context, identities middleware.IdentityReader) (representation.ClientIdentity, error) {
	if identities == nil {
		return representation.ClientIdentity{}, representation.ErrInvalidIdentity
	}
	return identities.Current(c.Request.Context())
}

func setIdentityResponseHeaders(c *gin.Context, namespace string) {
	c.Header("Cache-Control", "private, no-store")
	c.Header(representation.DataNamespaceHeader, namespace)
}

func deleteSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		middleware.ClearSessionCookie(c)
		c.Status(http.StatusNoContent)
	}
}
