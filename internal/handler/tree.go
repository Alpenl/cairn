package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/middleware"
	"webtag/internal/model"
)

// getTree 处理 GET /api/tree：默认返回链接树；?view=domains 返回
// debug 域名摘要。
func getTree(service TreeService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Query("view") == "domains" {
			kind, valid := model.NormalizeOptionalLibraryKind(c.Query("library_kind"))
			if !valid {
				writeError(c, httperr.NewWithCode(
					http.StatusUnprocessableEntity,
					httperr.CodeInvalidRequestedLibraryKind,
					"library_kind must be reading or site",
				))
				return
			}
			var response dto.DomainTreeSummaryEnvelope
			var err error
			if kind == "" {
				response, err = service.ListDomains(c.Request.Context())
			} else {
				response, err = service.ListDomainsScoped(c.Request.Context(), string(kind))
			}
			if err != nil {
				writeError(c, err)
				return
			}
			middleware.CacheableJSON(c, http.StatusOK, response)
			return
		}
		resp, err := service.Get(c.Request.Context(), c.Query("domain"))
		if err != nil {
			writeError(c, err)
			return
		}
		middleware.CacheableJSON(c, http.StatusOK, resp)
	}
}
