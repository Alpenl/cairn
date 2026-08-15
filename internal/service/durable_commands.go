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
	URL                        string
	Destination                string
	SourceKind                 string
	SourceKey                  string
	InputTitle                 *string
	InputText                  *string
	InputHTML                  *string
	InputImages                []string
	SourceMetadata             map[string]any
	Description                *string
	Status                     model.LinkStatus
	Domain                     *string
	ContentType                *string
	PathDepth                  *int
	ParentPath                 *string
	ParentID                   *uuid.UUID
	RequestedLibraryKind       model.RequestedLibraryKind
	RequestedLibraryKindSource model.RequestedLibraryKindSource
	PredictedLibraryKind       *model.LibraryKind
}

// InboxCaptureWriter is the narrow persistence seam used by capture
// destinations. It intentionally does not expose the full Reader store to the
// link submission service.
type InboxCaptureWriter interface {
	CreateInbox(context.Context, model.ReaderInbox) (*model.ReaderInbox, error)
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
	Job   *model.ReaderInboxJob
}

// InboxProposalOrphanRepairResult reports the rows selected by one bounded
// repair transaction and the subset that committed with a matching River job.
type InboxProposalOrphanRepairResult struct {
	Claimed  int
	Repaired int
}

// InboxProposalOrphanRepairer is the worker-facing boundary for recovering
// proposal attempts whose business rows survived without active River work.
type InboxProposalOrphanRepairer interface {
	RepairInboxProposalOrphans(context.Context, int) (InboxProposalOrphanRepairResult, error)
}

// InboxProposalCommands is the service-facing atomic write boundary. Its
// implementation owns pgx transactions and River InsertTx details.
type InboxProposalCommands interface {
	CreateInboxProposal(context.Context, CreateInboxProposalCommand) (InboxProposalResult, error)
	EnsureInboxProposal(context.Context, EnsureInboxProposalCommand) (InboxProposalResult, error)
}

type SubmitLinkCommand struct {
	Capture LinkCapture
}

type RequeueLinkCommand struct {
	LinkID  uuid.UUID
	Capture *LinkCapture
}

type UpdateLinkIntentCommand struct {
	LinkID uuid.UUID
	Kind   model.RequestedLibraryKind
	Source model.RequestedLibraryKindSource
}

type UpdateLinkIntentResult struct {
	Status model.LinkStatus
	Job    *model.ParseJob
}

type SubmitLinksBatchCommand struct {
	Captures []LinkCapture
}

type LinkSubmissionResult struct {
	Link     *model.Link
	Job      *model.ParseJob
	Inserted bool
	Restored bool
	Error    error
}

// LinkSubmissionCommands is the consumer-owned seam for product state and
// durable parse work. Its implementation owns transaction and queue details.
type LinkSubmissionCommands interface {
	SubmitLink(context.Context, SubmitLinkCommand) (LinkSubmissionResult, error)
	RequeueLink(context.Context, RequeueLinkCommand) (LinkSubmissionResult, error)
	UpdateLinkIntent(context.Context, UpdateLinkIntentCommand) (UpdateLinkIntentResult, error)
	SubmitLinksBatch(context.Context, SubmitLinksBatchCommand) ([]LinkSubmissionResult, error)
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
	ParseJobID      *uuid.UUID
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
