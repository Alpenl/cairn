package service

import (
	"context"

	"github.com/google/uuid"

	"webtag/internal/model"
)

type ReaderThoughtStore interface {
	AppendThoughtOps(context.Context, []model.ReaderThoughtOp) ([]model.ReaderThoughtAck, error)
	ListThoughts(context.Context, string, string, int) ([]model.ReaderThought, string, error)
	ListThoughtsSince(context.Context, string, int) ([]model.ReaderThought, string, error)
	ListThoughtHistory(context.Context, string, int) ([]model.ReaderThought, string, error)
	ListThoughtConflicts(context.Context, string, int) ([]model.ReaderThoughtConflict, string, error)
	GetThought(context.Context, string) (*model.ReaderThought, error)
}

type ReaderNoteStore interface {
	CreateNote(context.Context, model.ReaderNote) (*model.ReaderNote, error)
	ListNotes(context.Context, string, int) ([]model.ReaderNote, int, string, error)
	GetNote(context.Context, uuid.UUID) (*model.ReaderNote, error)
	SaveNoteDraft(context.Context, model.ReaderNoteDraftCommand) (*model.ReaderNote, error)
	DiscardNoteDraft(context.Context, model.ReaderNoteDiscardDraftCommand) error
	PublishNote(context.Context, model.ReaderNotePublishCommand) (*model.ReaderNote, error)
	ListNoteHistory(context.Context, uuid.UUID, int) ([]model.ReaderNoteHistory, error)
	RestoreNoteRevision(context.Context, model.ReaderNoteRestoreCommand) (*model.ReaderNote, error)
}

type ReaderInboxStore interface {
	CreateInbox(context.Context, model.ReaderInbox) (*model.ReaderInbox, error)
	ListInbox(context.Context, model.ReaderInboxPartition, string, int) ([]model.ReaderInboxListItem, int, int, string, error)
	GetInbox(context.Context, uuid.UUID) (*model.ReaderInbox, error)
	PatchInbox(context.Context, model.ReaderInboxPatch) (*model.ReaderInbox, error)
	DiscardInbox(context.Context, uuid.UUID) error
	RestoreInbox(context.Context, uuid.UUID) error
	ConfirmInbox(context.Context, uuid.UUID, *int64) (uuid.UUID, error)
	BulkConfirmInbox(context.Context, []model.ReaderInboxBulkConfirmation) ([]model.ReaderInboxBulkResult, error)
	ConfirmAIProposals(context.Context, model.ReaderInboxPartition) (model.ReaderInboxAIProposalConfirmation, error)
	BulkDiscardInbox(context.Context, []uuid.UUID) ([]model.ReaderInboxBulkResult, error)
}

type ReaderTodoStore interface {
	CreateTodo(context.Context, model.ReaderTodo) (*model.ReaderTodo, error)
	ListTodos(context.Context, string, int) (model.ReaderTodoPage, error)
	PatchTodo(context.Context, model.ReaderTodoPatch) (*model.ReaderTodo, error)
	DeleteTodo(context.Context, uuid.UUID) error
}

type ReaderLibraryStore interface {
	GetEngagement(context.Context, uuid.UUID) (*model.ReaderEngagement, error)
	PatchEngagement(context.Context, model.ReaderEngagementPatch) (*model.ReaderEngagement, error)
	LoadHomeAggregate(context.Context) (model.ReaderHomeAggregate, error)
	ListFeedWithSources(context.Context, string, string, []string, int) (*model.ReaderFeedPage, error)
	FeedbackFeed(context.Context, string, string) (model.ReaderFeedFeedback, error)
	RelatedTags(context.Context, *uuid.UUID, int) ([]string, error)
	ListActivity(context.Context, model.ReaderActivityQuery) (model.ReaderActivityPage, error)
	UpdateLinkMetadata(context.Context, model.ReaderLinkMetadataPatch) (model.ReaderLinkMetadataUpdate, error)
	GetAIContext(context.Context, uuid.UUID) (*model.ReaderAIContext, error)
}

type ReaderHostStore interface {
	SoftDeleteHost(context.Context, model.ReaderHostKind, uuid.UUID) (model.ReaderHostLifecycleResult, error)
	RestoreHost(context.Context, model.ReaderHostKind, uuid.UUID) (model.ReaderHostLifecycleResult, error)
	PurgeHost(context.Context, model.ReaderHostKind, uuid.UUID, uuid.UUID) error
	ListTrash(context.Context, *model.ReaderHostKind, string, int) ([]model.ReaderTrashItem, int, string, error)
}

// ReaderStores groups independent Reader persistence seams without forcing a
// test for one feature to implement every other feature. Production assigns
// the same PostgreSQL adapter to each field.
type ReaderStores struct {
	Thoughts ReaderThoughtStore
	Notes    ReaderNoteStore
	Inbox    ReaderInboxStore
	Todos    ReaderTodoStore
	Library  ReaderLibraryStore
	Hosts    ReaderHostStore
}
