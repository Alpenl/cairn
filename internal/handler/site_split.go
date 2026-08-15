package handler

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"webtag/internal/dto"
)

type SiteSplitService interface {
	Preview(context.Context, string, dto.SiteSplitRequest) (dto.SiteSplitPreviewResponse, error)
	Execute(context.Context, string, dto.SiteSplitRequest) (dto.SiteSplitExecuteResponse, error)
}

func siteSplitPreview(service SiteSplitService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.SiteSplitRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		out, err := service.Preview(c.Request.Context(), c.Param("site_id"), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
func siteSplitExecute(service SiteSplitService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.SiteSplitRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		out, err := service.Execute(c.Request.Context(), c.Param("site_id"), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
