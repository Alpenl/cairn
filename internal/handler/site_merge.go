package handler

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
)

type SiteMergeService interface {
	Preview(context.Context, dto.SiteMergePreviewRequest) (dto.SiteMergePreviewResponse, error)
	Execute(context.Context, dto.SiteMergeExecuteRequest) (dto.SiteMergeExecuteResponse, error)
}

func siteMergeExecute(service SiteMergeService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.SiteMergeExecuteRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		out, err := service.Execute(c.Request.Context(), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func siteMergePreview(service SiteMergeService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.SiteMergePreviewRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		out, err := service.Preview(c.Request.Context(), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
