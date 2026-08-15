package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
)

func conversionPreview(service LinkConversionPreviewService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ConversionPreviewRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		out, err := service.Preview(c.Request.Context(), c.Param("link_id"), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func convertLink(service LinkConversionExecuteService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ConversionExecuteRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		out, err := service.Execute(c.Request.Context(), c.Param("link_id"), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
