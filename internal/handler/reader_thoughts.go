package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
)

func readerPushThoughtOps(reader ReaderThoughtRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderThoughtOpsRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.PushThoughtOps(c.Request.Context(), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": response})
	}
}

func readerListThoughts(reader ReaderThoughtRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.ListThoughts(c.Request.Context(), c.Query("q"), c.Query("after"), queryInt(c, "limit", 50))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerSyncThoughts(reader ReaderThoughtRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.SyncThoughts(c.Request.Context(), c.Query("after"), queryInt(c, "limit", 100))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerListThoughtConflicts(reader ReaderThoughtRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.ListThoughtConflicts(c.Request.Context(), c.Query("after"), queryInt(c, "limit", 100))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerListThoughtHistory(reader ReaderThoughtRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.ListThoughtHistory(c.Request.Context(), c.Query("after"), queryInt(c, "limit", 30))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerGetThought(reader ReaderThoughtRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.GetThought(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}
