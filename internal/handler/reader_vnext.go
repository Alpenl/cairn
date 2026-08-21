package handler

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
)

// ReaderService is the transport boundary for the additive Reader vNext
// surface. Keeping it in handler makes route tests independent of PostgreSQL
// and prevents the HTTP package from depending on the concrete service.
type ReaderService interface {
	PushThoughtOps(context.Context, dto.ReaderThoughtOpsRequest) ([]dto.ReaderThoughtAckResponse, error)
	ListThoughts(context.Context, string, string, int) (dto.ReaderThoughtsResponse, error)
	ListThoughtHistory(context.Context, string, int) (dto.ReaderThoughtsResponse, error)
	ListThoughtConflicts(context.Context, string, int) (dto.ReaderThoughtConflictsResponse, error)
	SyncThoughts(context.Context, string, int) (dto.ReaderThoughtsResponse, error)
	GetThought(context.Context, string) (dto.ReaderThoughtResponse, error)
	CreateNote(context.Context, dto.ReaderNoteCreateRequest) (dto.ReaderNoteResponse, error)
	ListNotes(context.Context, string, int) (dto.ReaderNotesResponse, error)
	GetNote(context.Context, string) (dto.ReaderNoteResponse, error)
	SaveNoteDraft(context.Context, string, dto.ReaderNoteDraftRequest) (dto.ReaderNoteResponse, error)
	DiscardNoteDraft(context.Context, string, int64) error
	PublishNote(context.Context, string, dto.ReaderNotePublishRequest) (dto.ReaderNoteResponse, error)
	DeleteNote(context.Context, string) (dto.ReaderHostLifecycleResponse, error)
	RestoreNote(context.Context, string) (dto.ReaderHostLifecycleResponse, error)
	ListNoteHistory(context.Context, string, int) ([]dto.ReaderNoteHistoryResponse, error)
	RestoreNoteRevision(context.Context, string, int64, dto.ReaderNoteRestoreRequest) (dto.ReaderNoteResponse, error)
	CreateInbox(context.Context, dto.ReaderInboxCreateRequest) (dto.ReaderInboxResponse, error)
	ListInbox(context.Context, string, string, int) (dto.ReaderInboxResponsePage, error)
	GetInbox(context.Context, string) (dto.ReaderInboxResponse, error)
	PatchInbox(context.Context, string, dto.ReaderInboxPatchRequest, int64) (dto.ReaderInboxResponse, error)
	ConfirmInbox(context.Context, string, int64) (map[string]string, error)
	ConfirmAIProposals(context.Context, string) (dto.ReaderInboxConfirmAIProposalsResponse, error)
	DiscardInbox(context.Context, string) error
	RestoreInbox(context.Context, string) error
	ConfirmInboxBulk(context.Context, []string, map[string]int64) ([]model.ReaderInboxBulkResult, error)
	DiscardInboxBulk(context.Context, []string) ([]model.ReaderInboxBulkResult, error)
	ResummarizeInbox(context.Context, string) (dto.ReaderInboxResponse, error)
	CreateTodo(context.Context, dto.ReaderTodoCreateRequest) (dto.ReaderTodoResponse, error)
	ListTodos(context.Context, string, int) (dto.ReaderTodosResponse, error)
	PatchTodo(context.Context, string, dto.ReaderTodoPatchRequest) (dto.ReaderTodoResponse, error)
	DeleteTodo(context.Context, string) error
	GetEngagement(context.Context, string) (dto.ReaderEngagementResponse, error)
	PatchEngagement(context.Context, string, dto.ReaderEngagementRequest) (dto.ReaderEngagementResponse, error)
	Home(context.Context) (dto.ReaderHomeResponse, error)
	FeedWithSources(context.Context, string, string, []string, int) (dto.ReaderFeedResponse, error)
	FeedbackFeed(context.Context, string, string) (dto.ReaderFeedFeedbackResponse, error)
	RelatedTags(context.Context, string, int) (dto.ReaderRelatedTagsResponse, error)
	Activity(context.Context, string, string, int) (dto.ReaderActivityResponse, error)
	PatchLinkMetadata(context.Context, string, dto.ReaderLinkMetadataRequest, int64) (dto.ReaderLinkMetadataResponse, error)
	CompleteAI(context.Context, dto.ReaderAIRequest) (dto.ReaderAIResponse, error)
	RestoreHost(context.Context, string, string) (dto.ReaderHostLifecycleResponse, error)
	PurgeHost(context.Context, string, string, dto.ReaderHostPurgeRequest) error
	ListTrash(context.Context, string, string, int) (dto.ReaderTrashResponse, error)
}

// RegisterReaderRoutes mounts only the Reader vNext endpoints. The existing
// RSS /api/feed-items resource remains separate: a mixed Reader Feed item is
// a different union with a snapshot cursor and explicit reason metadata.
func RegisterReaderRoutes(router gin.IRouter, reader ReaderService) {
	if reader == nil {
		return
	}
	registerReaderThoughtRoutes(router, apiRoutePrefix, reader)
	registerReaderNoteRoutes(router, apiRoutePrefix, reader)
	registerReaderInboxRoutes(router, apiRoutePrefix, reader)
	registerReaderTodoRoutes(router, apiRoutePrefix, reader)
	registerReaderAggregateRoutes(router, apiRoutePrefix, reader)
	registerReaderHostLifecycleRoutes(router, apiRoutePrefix, reader)
}

func registerReaderHostLifecycleRoutes(router gin.IRouter, prefix string, lifecycle ReaderService) {
	router.GET(prefix+"/trash", readerListTrash(lifecycle))
	router.POST(prefix+"/links/:link_id/restore", readerRestoreHost(lifecycle, "link", "link_id"))
	router.DELETE(prefix+"/links/:link_id/purge", readerPurgeHost(lifecycle, "link", "link_id"))
	router.DELETE(prefix+"/notes/:id/purge", readerPurgeHost(lifecycle, "note", "id"))
	router.DELETE(prefix+"/inbox/:id/purge", readerPurgeHost(lifecycle, "inbox", "id"))
}

func registerReaderThoughtRoutes(router gin.IRouter, prefix string, reader ReaderService) {
	router.POST(prefix+"/annotations/ops", readerPushThoughtOps(reader))
	router.GET(prefix+"/annotations", readerListThoughts(reader))
	router.GET(prefix+"/annotations/sync", readerSyncThoughts(reader))
	router.GET(prefix+"/annotations/conflicts", readerListThoughtConflicts(reader))
	router.GET(prefix+"/annotations/history", readerListThoughtHistory(reader))
	router.GET(prefix+"/annotations/:id", readerGetThought(reader))
}

func registerReaderNoteRoutes(router gin.IRouter, prefix string, reader ReaderService) {
	router.POST(prefix+"/notes", readerCreateNote(reader))
	router.GET(prefix+"/notes", readerListNotes(reader))
	router.GET(prefix+"/notes/:id", readerGetNote(reader))
	router.PATCH(prefix+"/notes/:id/draft", readerSaveNoteDraft(reader))
	router.DELETE(prefix+"/notes/:id/draft", readerDiscardNoteDraft(reader))
	router.POST(prefix+"/notes/:id/publish", readerPublishNote(reader))
	router.DELETE(prefix+"/notes/:id", readerDeleteNote(reader))
	router.POST(prefix+"/notes/:id/restore", readerRestoreNote(reader))
	router.GET(prefix+"/notes/:id/history", readerListNoteHistory(reader))
	router.POST(prefix+"/notes/:id/history/:revision/restore", readerRestoreNoteRevision(reader))
}

func registerReaderInboxRoutes(router gin.IRouter, prefix string, reader ReaderService) {
	router.POST(prefix+"/inbox", readerCreateInbox(reader))
	router.GET(prefix+"/inbox", readerListInbox(reader))
	// Register collection actions before /inbox/:id so Gin gives the static
	// bulk branch precedence over the parameterized item branch.
	router.POST(prefix+"/inbox/bulk/confirm", readerConfirmInboxBulk(reader))
	router.POST(prefix+"/inbox/bulk/discard", readerDiscardInboxBulk(reader))
	router.POST(prefix+"/inbox/confirm-ai-proposals", readerConfirmAIProposals(reader))
	router.GET(prefix+"/inbox/:id", readerGetInbox(reader))
	router.PATCH(prefix+"/inbox/:id", readerPatchInbox(reader))
	router.POST(prefix+"/inbox/:id/confirm", readerConfirmInbox(reader))
	router.POST(prefix+"/inbox/:id/discard", readerDiscardInbox(reader))
	router.POST(prefix+"/inbox/:id/restore", readerRestoreInbox(reader))
	router.POST(prefix+"/inbox/:id/resummarize", readerResummarizeInbox(reader))
}

func registerReaderTodoRoutes(router gin.IRouter, prefix string, reader ReaderService) {
	router.POST(prefix+"/todos", readerCreateTodo(reader))
	router.GET(prefix+"/todos", readerListTodos(reader))
	router.PATCH(prefix+"/todos/:id", readerPatchTodo(reader))
	router.DELETE(prefix+"/todos/:id", readerDeleteTodo(reader))
}

func registerReaderAggregateRoutes(router gin.IRouter, prefix string, reader ReaderService) {
	router.GET(prefix+"/home", readerHome(reader))
	router.GET(prefix+"/reader-feed", readerFeed(reader))
	router.POST(prefix+"/reader-feed/feedback", readerFeedFeedback(reader))
	router.GET(prefix+"/engagement/:link_id", readerGetEngagement(reader))
	router.PATCH(prefix+"/engagement/:link_id", readerPatchEngagement(reader))
	router.PATCH(prefix+"/links/:link_id/metadata", readerPatchMetadata(reader))
	router.GET(prefix+"/reader/related-tags", readerRelatedTags(reader))
	router.GET(prefix+"/reader/activity", readerActivity(reader))
	router.POST(prefix+"/ai", readerCompleteAI(reader))
}

func readerPushThoughtOps(reader ReaderService) gin.HandlerFunc {
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

func readerListThoughts(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.ListThoughts(c.Request.Context(), c.Query("q"), c.Query("after"), queryInt(c, "limit", 50))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerSyncThoughts(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.SyncThoughts(c.Request.Context(), c.Query("after"), queryInt(c, "limit", 100))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerListThoughtConflicts(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.ListThoughtConflicts(c.Request.Context(), c.Query("after"), queryInt(c, "limit", 100))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerListThoughtHistory(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.ListThoughtHistory(c.Request.Context(), c.Query("after"), queryInt(c, "limit", 30))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerGetThought(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.GetThought(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerCreateNote(reader ReaderService) gin.HandlerFunc {
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

func readerListNotes(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.ListNotes(c.Request.Context(), c.Query("after"), queryInt(c, "limit", 30))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerGetNote(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.GetNote(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerSaveNoteDraft(reader ReaderService) gin.HandlerFunc {
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

func readerDiscardNoteDraft(reader ReaderService) gin.HandlerFunc {
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

func readerPublishNote(reader ReaderService) gin.HandlerFunc {
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

func readerDeleteNote(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.DeleteNote(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerRestoreNote(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.RestoreNote(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerListNoteHistory(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.ListNoteHistory(c.Request.Context(), c.Param("id"), queryInt(c, "limit", 50))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": response})
	}
}

func readerRestoreNoteRevision(reader ReaderService) gin.HandlerFunc {
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

func readerCreateInbox(reader ReaderService) gin.HandlerFunc {
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

func readerListInbox(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.ListInbox(c.Request.Context(), c.Query("partition"), c.Query("after"), queryInt(c, "limit", 30))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerGetInbox(reader ReaderService) gin.HandlerFunc {
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

func readerPatchInbox(reader ReaderService) gin.HandlerFunc {
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

func readerConfirmInbox(reader ReaderService) gin.HandlerFunc {
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

func readerConfirmAIProposals(reader ReaderService) gin.HandlerFunc {
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

func readerConfirmInboxBulk(reader ReaderService) gin.HandlerFunc {
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

func readerDiscardInbox(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := reader.DiscardInbox(c.Request.Context(), c.Param("id")); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusOK)
	}
}

func readerListTrash(lifecycle ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := lifecycle.ListTrash(c.Request.Context(), c.Query("host_kind"), c.Query("after"), queryInt(c, "limit", 50))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerRestoreHost(lifecycle ReaderService, kind, param string) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := lifecycle.RestoreHost(c.Request.Context(), kind, c.Param(param))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerPurgeHost(lifecycle ReaderService, kind, param string) gin.HandlerFunc {
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

func readerRestoreInbox(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := reader.RestoreInbox(c.Request.Context(), c.Param("id")); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusOK)
	}
}

func readerDiscardInboxBulk(reader ReaderService) gin.HandlerFunc {
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

func readerResummarizeInbox(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.ResummarizeInbox(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, response)
	}
}

func readerCreateTodo(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderTodoCreateRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.CreateTodo(c.Request.Context(), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, response)
	}
}

func readerListTodos(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := 200
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 200 {
				writeError(c, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_todo_limit", "limit must be between 1 and 200"))
				return
			}
			limit = parsed
		}
		response, err := reader.ListTodos(c.Request.Context(), c.Query("after"), limit)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerPatchTodo(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderTodoPatchRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.PatchTodo(c.Request.Context(), c.Param("id"), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerDeleteTodo(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := reader.DeleteTodo(c.Request.Context(), c.Param("id")); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func readerHome(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.Home(c.Request.Context())
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerFeed(reader ReaderService) gin.HandlerFunc {
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

func readerFeedFeedback(reader ReaderService) gin.HandlerFunc {
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

func readerGetEngagement(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.GetEngagement(c.Request.Context(), c.Param("link_id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerPatchEngagement(reader ReaderService) gin.HandlerFunc {
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

func readerPatchMetadata(reader ReaderService) gin.HandlerFunc {
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

func readerRelatedTags(reader ReaderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := reader.RelatedTags(c.Request.Context(), c.Query("link_id"), queryInt(c, "limit", 12))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerActivity(reader ReaderService) gin.HandlerFunc {
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

func readerCompleteAI(reader ReaderService) gin.HandlerFunc {
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

func parseIfMatch(c *gin.Context) (int64, error) {
	raw := strings.TrimSpace(c.GetHeader("If-Match"))
	if raw == "" {
		return 0, httperr.NewWithCode(http.StatusPreconditionRequired, "if_match_required", "If-Match is required")
	}
	raw = strings.Trim(raw, "\"")
	revision, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || revision < 1 {
		return 0, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_if_match", "If-Match must contain a positive revision")
	}
	return revision, nil
}

// parseLinkMetadataIfMatch keeps the Link metadata CAS wire contract narrow:
// it accepts only the canonical quoted numeric revision emitted by the
// endpoint. Other Reader writes retain parseIfMatch's older compatibility
// behavior while their contracts converge separately.
func parseLinkMetadataIfMatch(c *gin.Context) (int64, error) {
	values := c.Request.Header.Values("If-Match")
	if len(values) == 0 || (len(values) == 1 && values[0] == "") {
		return 0, httperr.NewWithCode(http.StatusPreconditionRequired, "if_match_required", "If-Match is required")
	}
	if len(values) != 1 {
		return 0, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_if_match", "If-Match must contain exactly one quoted positive revision")
	}
	raw := values[0]
	if len(raw) < 3 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return 0, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_if_match", "If-Match must contain a quoted positive revision")
	}
	revision, err := strconv.ParseInt(raw[1:len(raw)-1], 10, 64)
	if err != nil || revision < 1 || revision > model.LinkMetadataMaxRevision || quoteRevision(revision) != raw {
		return 0, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_if_match", "If-Match must contain a quoted positive revision")
	}
	return revision, nil
}

func quoteRevision(revision int64) string { return strconv.Quote(strconv.FormatInt(revision, 10)) }

func invalidReaderRequest(code, message string) error {
	return httperr.NewWithCode(http.StatusUnprocessableEntity, code, message)
}
