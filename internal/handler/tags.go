package handler

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/middleware"
)

// listTags 处理 GET /api/tags：返回所有标签及对应的使用次数。
func listTags(service TagService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var resp []dto.TagCountResponse
		var err error
		if scoped, ok := service.(interface {
			ListScoped(context.Context, string) ([]dto.TagCountResponse, error)
		}); ok {
			resp, err = scoped.ListScoped(c.Request.Context(), c.Query("library_kind"))
		} else {
			resp, err = service.List(c.Request.Context())
		}
		if err != nil {
			writeError(c, err)
			return
		}
		if resp == nil {
			resp = []dto.TagCountResponse{}
		}
		middleware.CacheableJSON(c, http.StatusOK, resp)
	}
}
