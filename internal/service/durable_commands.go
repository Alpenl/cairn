package service

import (
	"context"

	"github.com/google/uuid"

	"webtag/internal/model"
)

// LinkCapture is the application input shared by submit, ingest, refresh, and
// batch commands. Persistence-specific defaults and encodings are owned by the
// durable adapter and repository implementation.
type LinkCapture struct {
	URL                     string
	Destination             string
	SourceKind              string
	SourceKey               string
	InputTitle              *string
	InputText               *string
	InputHTML               *string
	InputImages             []string
	SourceMetadata          map[string]any
	Description             *string
	Status                  model.LinkStatus
	Domain                  *string
	ContentType             *string
	PathDepth               *int
	ParentPath              *string
	ParentID                *uuid.UUID
	RequestedLibraryKind    model.RequestedLibraryKind
	UserSelectedLibraryKind bool
}

// InboxCaptureWriter is the narrow lookup seam used by capture destinations.
// Creation goes through InboxProposalCommands so the Inbox row and River work
// cannot commit independently.
type InboxCaptureWriter interface {
	GetInboxByURL(context.Context, string) (*model.ReaderInbox, error)
}

// CreateInboxProposalCommand carries the product row into the durable Inbox
// scheduling boundary. The adapter creates the row, its proposal attempt, and
// the River job in one PostgreSQL transaction.
type CreateInboxProposalCommand struct {
	Inbox model.ReaderInbox
}

// EnsureInboxProposalCommand repairs or replays the active proposal attempt
// for an existing Inbox row. Re-enqueueing the same args is safe because River
// applies the job's active-state uniqueness policy inside the transaction.
type EnsureInboxProposalCommand struct {
	InboxID                  uuid.UUID
	ExpectedMetadataRevision int64
}

type InboxProposalResult struct {
	Inbox *model.ReaderInbox
}

// InboxProposalCommands is the service-facing atomic write boundary. Its
// implementation owns pgx transactions and River InsertTx details.
type InboxProposalCommands interface {
	CreateInboxProposal(context.Context, CreateInboxProposalCommand) (InboxProposalResult, error)
	EnsureInboxProposal(context.Context, EnsureInboxProposalCommand) (InboxProposalResult, error)
}

// ReaderInboxConfirmCommands owns one Inbox confirmation that may restore a
// canonical Link and therefore must coordinate PostgreSQL state with River in
// one transaction.
type ReaderInboxConfirmCommands interface {
	ConfirmInbox(context.Context, uuid.UUID, *int64) (uuid.UUID, error)
}

type ReaderInboxBulkConfirmCommands interface {
	BulkConfirmInbox(context.Context, []model.ReaderInboxBulkConfirmation) ([]model.ReaderInboxBulkResult, error)
}

type ReaderInboxAIConfirmCommands interface {
	ConfirmAIProposals(context.Context, model.ReaderInboxPartition) (model.ReaderInboxAIProposalConfirmation, error)
}

// ReaderFeedFeedbackCommands owns feed feedback that may create, restore, or
// trash a Feed-managed Link.
type ReaderFeedFeedbackCommands interface {
	FeedbackFeed(context.Context, string, string) (model.ReaderFeedFeedback, error)
}

// ReaderHostRestoreCommands owns polymorphic host restoration. Link restores
// can schedule durable parse work; Note and Inbox restores use the same
// application command without exposing transaction details.
type ReaderHostRestoreCommands interface {
	RestoreHost(context.Context, model.ReaderHostKind, uuid.UUID) (model.ReaderHostLifecycleResult, error)
}

type SubmitLinkCommand struct {
	Capture LinkCapture
}

type RequeueLinkCommand struct {
	LinkID  uuid.UUID
	Capture *LinkCapture
}

type SetLinkLibraryKindCommand struct {
	LinkID   uuid.UUID
	Kind     model.LibraryKind
	Override bool
}

type SetLinkLibraryKindResult struct {
	Status model.LinkStatus
}

type LinkSubmissionResult struct {
	Link     *model.Link
	Enqueued bool
}

// LinkSubmissionCommands is the consumer-owned seam for product state and
// durable parse work. Its implementation owns transaction and queue details.
type LinkSubmissionCommands interface {
	SubmitLink(context.Context, SubmitLinkCommand) (LinkSubmissionResult, error)
	RequeueLink(context.Context, RequeueLinkCommand) (LinkSubmissionResult, error)
	SetLinkLibraryKind(context.Context, SetLinkLibraryKindCommand) (SetLinkLibraryKindResult, error)
}

type ConvertLinkCommand struct {
	LinkID                  uuid.UUID
	TargetKind              model.LibraryKind
	ExpectedContentRevision int64
	TargetSiteID            *uuid.UUID
	ExpectedSiteRevision    *int64
	PreservedUserNote       *string
}

type ConvertLinkResult struct {
	LinkID          uuid.UUID
	Kind            model.LibraryKind
	ContentRevision int64
	Status          model.LinkStatus
	SiteID          *uuid.UUID
	SiteRevision    *int64
	EntryID         *uuid.UUID
}

type LinkConversionCommands interface {
	ConvertLink(context.Context, ConvertLinkCommand) (ConvertLinkResult, error)
}

type DeleteLinkCommand struct {
	LinkID uuid.UUID
}

type LinkDeletionCommands interface {
	DeleteLink(context.Context, DeleteLinkCommand) error
}
