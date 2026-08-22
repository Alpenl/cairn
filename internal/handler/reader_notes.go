package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
)

func readerCreateNote(reader ReaderNoteRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderNoteCreateRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.CreateNote(c.Request.Context(), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, response)
	}
}

func readerListNotes(reader ReaderNoteRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.ListNotes(c.Request.Context(), c.Query("after"), queryInt(c, "limit", 30))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerGetNote(reader ReaderNoteRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.GetNote(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerSaveNoteDraft(reader ReaderNoteRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderNoteDraftRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.SaveNoteDraft(c.Request.Context(), c.Param("id"), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.Header("ETag", quoteRevision(response.DraftRevision))
		c.JSON(http.StatusOK, response)
	}
}

func readerDiscardNoteDraft(reader ReaderNoteRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		revision, err := parseIfMatch(c)
		if err != nil {
			writeError(c, err)
			return
		}
		if err := reader.DiscardNoteDraft(c.Request.Context(), c.Param("id"), revision); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func readerPublishNote(reader ReaderNoteRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderNotePublishRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.PublishNote(c.Request.Context(), c.Param("id"), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.Header("ETag", quoteRevision(response.PublishedRevision))
		c.JSON(http.StatusOK, response)
	}
}

func readerDeleteNote(reader ReaderNoteRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.DeleteNote(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerRestoreNote(reader ReaderNoteRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.RestoreNote(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerListNoteHistory(reader ReaderNoteRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.ListNoteHistory(c.Request.Context(), c.Param("id"), queryInt(c, "limit", 50))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": response})
	}
}

func readerRestoreNoteRevision(reader ReaderNoteRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		revision, err := strconv.ParseInt(c.Param("revision"), 10, 64)
		if err != nil || revision < 1 {
			writeError(c, invalidReaderRequest("invalid_revision", "revision must be positive"))
			return
		}
		var request dto.ReaderNoteRestoreRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.RestoreNoteRevision(c.Request.Context(), c.Param("id"), revision, request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}
