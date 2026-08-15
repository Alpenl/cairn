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
	SigningKey      []byte
	TTL             time.Duration
	Authenticator   *middleware.InstallationAuthenticator
	AllowOpenAccess bool
	Representations middleware.VersionReader
}

type sessionCreateRequest struct {
	Token string `json:"token" binding:"max=512"`
}

// RegisterSessionExchangeRoutes mounts the login and logout endpoints outside
// the protected group. The login exchanges the static installation token for
// a short-lived HttpOnly cookie; explicitly open installations need no token.
func RegisterSessionExchangeRoutes(router gin.IRoutes, opts SessionOptions) {
	if len(opts.SigningKey) == 0 ||
		(!opts.AllowOpenAccess && (opts.Authenticator == nil || !opts.Authenticator.Configured())) {
		return
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = session.DefaultTTL
	}
	// 与其他公开路由一样按 apiRoutePrefixes 挂载：只挂 /api/session 会让固定
	// 使用 /api/v1 前缀的客户端在登录这一步拿到 404，整个 v1 表面不可用。
	for _, prefix := range apiRoutePrefixes {
		router.POST(prefix+"/session", createSession(opts, ttl))
		router.DELETE(prefix+"/session", deleteSession())
	}
}

func RegisterSessionIdentityRoute(router gin.IRoutes) {
	for _, prefix := range apiRoutePrefixes {
		router.GET(prefix+"/session", getSessionIdentity())
	}
}

func createSession(opts SessionOptions, ttl time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req sessionCreateRequest
		if !bindJSONWithLimit(c, &req, defaultMaxJSONBodyBytes) {
			return
		}
		if !opts.AllowOpenAccess && !opts.Authenticator.Authenticate(req.Token) {
			middleware.JSONErrorWithSlug(c, http.StatusUnauthorized, middleware.ErrCodeUnauthorized, "installation token is invalid")
			return
		}

		identity, err := resolveInstallationRepresentation(c, opts.Representations)
		if err != nil {
			middleware.JSONErrorWithSlug(c, http.StatusInternalServerError, middleware.ErrCodeInternalError, "installation namespace is unavailable")
			return
		}
		expiresAt := time.Now().Add(ttl).Truncate(time.Second)
		token, err := session.Sign(session.Claims{ExpiresAt: expiresAt}, opts.SigningKey)
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

func resolveInstallationRepresentation(c *gin.Context, versions middleware.VersionReader) (representation.ClientIdentity, error) {
	if versions == nil {
		return representation.ClientIdentity{}, representation.ErrInvalidVersion
	}
	components, err := representation.NewComponentSet()
	if err != nil {
		return representation.ClientIdentity{}, err
	}
	base, err := versions.Current(c.Request.Context(), components)
	if err != nil {
		return representation.ClientIdentity{}, err
	}
	return representation.NewClientIdentity(base)
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
