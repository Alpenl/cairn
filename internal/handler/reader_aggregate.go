package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
)

func readerHome(reader ReaderLibraryRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.Home(c.Request.Context())
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerFeed(reader ReaderLibraryRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		mode, after, limit := c.Query("mode"), c.Query("after"), queryInt(c, "limit", 30)
		response, err := reader.FeedWithSources(c.Request.Context(), mode, after, readerFeedSources(c), limit)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerFeedSources(c *gin.Context) []string {
	values := append([]string(nil), c.QueryArray("source")...)
	values = append(values, c.QueryArray("sources")...)
	return values
}

func readerFeedFeedback(reader ReaderLibraryRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderFeedFeedbackRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.FeedbackFeed(c.Request.Context(), c.Query("item_key"), request.Action)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerGetEngagement(reader ReaderLibraryRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.GetEngagement(c.Request.Context(), c.Param("link_id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerPatchEngagement(reader ReaderLibraryRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderEngagementRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.PatchEngagement(c.Request.Context(), c.Param("link_id"), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerPatchMetadata(reader ReaderLibraryRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		expected, err := parseLinkMetadataIfMatch(c)
		if err != nil {
			writeError(c, err)
			return
		}
		var request dto.ReaderLinkMetadataRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.PatchLinkMetadata(c.Request.Context(), c.Param("link_id"), request, expected)
		if err != nil {
			writeError(c, err)
			return
		}
		c.Header("ETag", quoteRevision(response.MetadataRevision))
		c.JSON(http.StatusOK, response)
	}
}

func readerRelatedTags(reader ReaderLibraryRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.RelatedTags(c.Request.Context(), c.Query("link_id"), queryInt(c, "limit", 12))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerActivity(reader ReaderLibraryRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.Activity(
			c.Request.Context(),
			c.Query("kind"),
			c.Query("after"),
			queryInt(c, "limit", 100),
		)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerCompleteAI(reader ReaderLibraryRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderAIRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.CompleteAI(c.Request.Context(), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}
