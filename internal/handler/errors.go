package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/middleware"
	"webtag/internal/observability"
)

func writeError(c *gin.Context, err error) {
	// httperr.As walks the unwrap chain looking for any value that satisfies
	// httperr.StatusCarrier — that is the single public contract shared
	// between the service and presentation layers. It also unwraps pipeline
	// wrappers like *service.PipelineRunError so a 4xx httperr.Error nested
	// inside still surfaces as the right status instead of a generic 500.
	if carrier, ok := httperr.As(err); ok {
		// Retry-After is optional metadata: the rate-limit / cooldown
		// paths attach it via httperr.NewWithRetryAfter, every other
		// 4xx leaves it at zero. Setting the header before JSONError
		// ensures clients see both the body envelope and the timing
		// hint in one response.
		if rc, ok := carrier.(interface{ RetryAfterSeconds() int }); ok {
			if seconds := rc.RetryAfterSeconds(); seconds > 0 {
				c.Header("Retry-After", strconv.Itoa(seconds))
			}
		}
		// 优先取 service 层注入的 slug（httperr.NewWithCode 等构造）。
		// 没实现 ErrorCoder 或 slug 为空时回退到 default_<status>，与既有
		// JSONError 调用语义一致。这条路径是 service → handler 唯一的
		// slug 透出口。
		slug := ""
		if coder, ok := carrier.(httperr.ErrorCoder); ok {
			slug = coder.HTTPErrorCode()
		}
		var currentIdentity *dto.TranslationSourceIdentity
		if provider, ok := carrier.(httperr.CurrentIdentityProvider); ok {
			if identity, present := provider.HTTPCurrentIdentity(); present {
				currentIdentity = &dto.TranslationSourceIdentity{
					ContentRevision: identity.ContentRevision,
					BlockKey:        identity.BlockKey,
					SourceHash:      identity.SourceHash,
				}
			}
		}
		middleware.JSONErrorWithSlugAndCurrentIdentity(
			c,
			carrier.HTTPStatus(),
			slug,
			carrier.HTTPMessage(),
			currentIdentity,
		)
		return
	}
	if logger := observability.FromContext(c.Request.Context()); logger != nil {
		// Wave 2 H5：500 路径上的 err 通常带完整上游错误链——这正是
		// pgx DSN / 上游 URL / sk- 凭据最常露出的地方。SafeError 脱敏后
		// 才允许进 stdout，避免凭据在日志聚合系统里散落。
		logger.Error("http handler returned internal error",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"route", c.FullPath(),
			"error", observability.SafeError(err),
		)
	}
	middleware.JSONErrorWithSlug(c, http.StatusInternalServerError, middleware.ErrCodeInternalError, "内部服务器错误")
}
