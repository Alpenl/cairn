package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/model"
)

func readerCreateInbox(reader ReaderInboxRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderInboxCreateRequest
		if !bindJSONWithLimit(c, &request, ingestMaxJSONBodyBytes) {
			return
		}
		response, err := reader.CreateInbox(c.Request.Context(), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, response)
	}
}

func readerListInbox(reader ReaderInboxRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.ListInbox(c.Request.Context(), c.Query("partition"), c.Query("after"), queryInt(c, "limit", 30))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerGetInbox(reader ReaderInboxRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.GetInbox(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.Header("ETag", quoteRevision(response.MetadataRevision))
		c.JSON(http.StatusOK, response)
	}
}

func readerPatchInbox(reader ReaderInboxRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		expected, err := parseIfMatch(c)
		if err != nil {
			writeError(c, err)
			return
		}
		var request dto.ReaderInboxPatchRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.PatchInbox(c.Request.Context(), c.Param("id"), request, expected)
		if err != nil {
			writeError(c, err)
			return
		}
		c.Header("ETag", quoteRevision(response.MetadataRevision))
		c.JSON(http.StatusOK, response)
	}
}

func readerConfirmInbox(reader ReaderInboxRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		expected := int64(-1)
		if strings.TrimSpace(c.GetHeader("If-Match")) != "" {
			var err error
			expected, err = parseIfMatch(c)
			if err != nil {
				writeError(c, err)
				return
			}
		}
		response, err := reader.ConfirmInbox(c.Request.Context(), c.Param("id"), expected)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerConfirmAIProposals(reader ReaderInboxRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderInboxConfirmAIProposalsRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.ConfirmAIProposals(c.Request.Context(), request.Partition)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerConfirmInboxBulk(reader ReaderInboxRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderInboxBulkRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		items, err := reader.ConfirmInboxBulk(c.Request.Context(), request.InboxIDs, request.ExpectedRevisions)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, readerInboxBulkResponse(items))
	}
}

func readerDiscardInbox(reader ReaderInboxRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := reader.DiscardInbox(c.Request.Context(), c.Param("id")); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusOK)
	}
}

func readerRestoreInbox(reader ReaderInboxRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := reader.RestoreInbox(c.Request.Context(), c.Param("id")); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusOK)
	}
}

func readerDiscardInboxBulk(reader ReaderInboxRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderInboxBulkRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		items, err := reader.DiscardInboxBulk(c.Request.Context(), request.InboxIDs)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, readerInboxBulkResponse(items))
	}
}

func readerInboxBulkResponse(items []model.ReaderInboxBulkResult) dto.ReaderInboxBulkResponse {
	response := dto.ReaderInboxBulkResponse{
		Atomic: true,
		Items:  make([]dto.ReaderInboxBulkItemResponse, 0, len(items)),
	}
	for _, item := range items {
		out := dto.ReaderInboxBulkItemResponse{
			InboxID: item.ID.String(),
			Status:  item.Status,
		}
		if item.LinkID != nil {
			linkID := item.LinkID.String()
			out.LinkID = &linkID
		}
		response.Items = append(response.Items, out)
	}
	return response
}

func readerResummarizeInbox(reader ReaderInboxRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.ResummarizeInbox(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, response)
	}
}
