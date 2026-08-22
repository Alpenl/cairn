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

type ReaderThoughtRoutes interface {
	PushThoughtOps(context.Context, dto.ReaderThoughtOpsRequest) ([]dto.ReaderThoughtAckResponse, error)
	ListThoughts(context.Context, string, string, int) (dto.ReaderThoughtsResponse, error)
	ListThoughtHistory(context.Context, string, int) (dto.ReaderThoughtsResponse, error)
	ListThoughtConflicts(context.Context, string, int) (dto.ReaderThoughtConflictsResponse, error)
	SyncThoughts(context.Context, string, int) (dto.ReaderThoughtsResponse, error)
	GetThought(context.Context, string) (dto.ReaderThoughtResponse, error)
}

type ReaderNoteRoutes interface {
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
}

type ReaderInboxRoutes interface {
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
}

type ReaderTodoRoutes interface {
	CreateTodo(context.Context, dto.ReaderTodoCreateRequest) (dto.ReaderTodoResponse, error)
	ListTodos(context.Context, string, int) (dto.ReaderTodosResponse, error)
	PatchTodo(context.Context, string, dto.ReaderTodoPatchRequest) (dto.ReaderTodoResponse, error)
	DeleteTodo(context.Context, string) error
}

type ReaderLibraryRoutes interface {
	GetEngagement(context.Context, string) (dto.ReaderEngagementResponse, error)
	PatchEngagement(context.Context, string, dto.ReaderEngagementRequest) (dto.ReaderEngagementResponse, error)
	Home(context.Context) (dto.ReaderHomeResponse, error)
	FeedWithSources(context.Context, string, string, []string, int) (dto.ReaderFeedResponse, error)
	FeedbackFeed(context.Context, string, string) (dto.ReaderFeedFeedbackResponse, error)
	RelatedTags(context.Context, string, int) (dto.ReaderRelatedTagsResponse, error)
	Activity(context.Context, string, string, int) (dto.ReaderActivityResponse, error)
	PatchLinkMetadata(context.Context, string, dto.ReaderLinkMetadataRequest, int64) (dto.ReaderLinkMetadataResponse, error)
	CompleteAI(context.Context, dto.ReaderAIRequest) (dto.ReaderAIResponse, error)
}

type ReaderHostRoutes interface {
	RestoreHost(context.Context, string, string) (dto.ReaderHostLifecycleResponse, error)
	PurgeHost(context.Context, string, string, dto.ReaderHostPurgeRequest) error
	ListTrash(context.Context, string, string, int) (dto.ReaderTrashResponse, error)
}

// ReaderRoutes keeps each Reader feature independently replaceable and
// testable. Production wires one feature-specific HTTP adapter into each field.
type ReaderRoutes struct {
	Thoughts ReaderThoughtRoutes
	Notes    ReaderNoteRoutes
	Inbox    ReaderInboxRoutes
	Todos    ReaderTodoRoutes
	Library  ReaderLibraryRoutes
	Hosts    ReaderHostRoutes
}

func (r ReaderRoutes) Enabled() bool {
	return r.Thoughts != nil || r.Notes != nil || r.Inbox != nil ||
		r.Todos != nil || r.Library != nil || r.Hosts != nil
}

// RegisterReaderRoutes mounts only the Reader vNext endpoints. The existing
// RSS /api/feed-items resource remains separate: a mixed Reader Feed item is
// a different union with a snapshot cursor and explicit reason metadata.
func RegisterReaderRoutes(router gin.IRouter, reader ReaderRoutes) {
	if reader.Thoughts != nil {
		registerReaderThoughtRoutes(router, apiRoutePrefix, reader.Thoughts)
	}
	if reader.Notes != nil {
		registerReaderNoteRoutes(router, apiRoutePrefix, reader.Notes)
	}
	if reader.Inbox != nil {
		registerReaderInboxRoutes(router, apiRoutePrefix, reader.Inbox)
	}
	if reader.Todos != nil {
		registerReaderTodoRoutes(router, apiRoutePrefix, reader.Todos)
	}
	if reader.Library != nil {
		registerReaderLibraryRoutes(router, apiRoutePrefix, reader.Library)
	}
	if reader.Hosts != nil {
		registerReaderHostLifecycleRoutes(router, apiRoutePrefix, reader.Hosts)
	}
}

func registerReaderHostLifecycleRoutes(router gin.IRouter, prefix string, lifecycle ReaderHostRoutes) {
	router.GET(prefix+"/trash", readerListTrash(lifecycle))
	router.POST(prefix+"/links/:link_id/restore", readerRestoreHost(lifecycle, "link", "link_id"))
	router.DELETE(prefix+"/links/:link_id/purge", readerPurgeHost(lifecycle, "link", "link_id"))
	router.DELETE(prefix+"/notes/:id/purge", readerPurgeHost(lifecycle, "note", "id"))
	router.DELETE(prefix+"/inbox/:id/purge", readerPurgeHost(lifecycle, "inbox", "id"))
}

func registerReaderThoughtRoutes(router gin.IRouter, prefix string, reader ReaderThoughtRoutes) {
	router.POST(prefix+"/annotations/ops", readerPushThoughtOps(reader))
	router.GET(prefix+"/annotations", readerListThoughts(reader))
	router.GET(prefix+"/annotations/sync", readerSyncThoughts(reader))
	router.GET(prefix+"/annotations/conflicts", readerListThoughtConflicts(reader))
	router.GET(prefix+"/annotations/history", readerListThoughtHistory(reader))
	router.GET(prefix+"/annotations/:id", readerGetThought(reader))
}

func registerReaderNoteRoutes(router gin.IRouter, prefix string, reader ReaderNoteRoutes) {
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

func registerReaderInboxRoutes(router gin.IRouter, prefix string, reader ReaderInboxRoutes) {
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

func registerReaderTodoRoutes(router gin.IRouter, prefix string, reader ReaderTodoRoutes) {
	router.POST(prefix+"/todos", readerCreateTodo(reader))
	router.GET(prefix+"/todos", readerListTodos(reader))
	router.PATCH(prefix+"/todos/:id", readerPatchTodo(reader))
	router.DELETE(prefix+"/todos/:id", readerDeleteTodo(reader))
}

func registerReaderLibraryRoutes(router gin.IRouter, prefix string, reader ReaderLibraryRoutes) {
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
