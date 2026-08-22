package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
)

func readerListTrash(lifecycle ReaderHostRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := lifecycle.ListTrash(c.Request.Context(), c.Query("host_kind"), c.Query("after"), queryInt(c, "limit", 50))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerRestoreHost(lifecycle ReaderHostRoutes, kind, param string) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := lifecycle.RestoreHost(c.Request.Context(), kind, c.Param(param))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerPurgeHost(lifecycle ReaderHostRoutes, kind, param string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderHostPurgeRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		if err := lifecycle.PurgeHost(c.Request.Context(), kind, c.Param(param), request); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}
