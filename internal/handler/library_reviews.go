package handler

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
)

type LibraryReviewService interface {
	List(context.Context, string, string, int, int) ([]dto.LibraryReviewResponse, error)
	Resolve(context.Context, string, dto.LibraryReviewResolveRequest) (dto.LibraryReviewResponse, error)
}

func listLibraryReviews(service LibraryReviewService) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := service.List(c.Request.Context(), c.Query("status"), c.Query("type"), queryInt(c, "limit", 30), queryInt(c, "offset", 0))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func resolveLibraryReview(service LibraryReviewService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.LibraryReviewResolveRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		out, err := service.Resolve(c.Request.Context(), c.Param("review_id"), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
