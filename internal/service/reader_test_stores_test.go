package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/problem"
)

// readerTestStores wires only the feature interfaces a focused test double
// implements. It keeps production construction explicit while allowing each
// test fake to define just the methods exercised by that test.
func readerTestStores(store any) ReaderStores {
	var stores ReaderStores
	stores.Thoughts, _ = store.(ReaderThoughtStore)
	stores.Notes, _ = store.(ReaderNoteStore)
	stores.Inbox, _ = store.(ReaderInboxStore)
	stores.Todos, _ = store.(ReaderTodoStore)
	stores.Library, _ = store.(ReaderLibraryStore)
	stores.Hosts, _ = store.(ReaderHostStore)
	return stores
}

// readerTestFeatureSet is test-only composition sugar. Production routes use
// ReaderApplications' named fields directly; focused tests can still exercise
// one feature without building unrelated fakes.
type readerTestFeatureSet struct {
	*ReaderThoughtApplication
	*ReaderNoteApplication
	*ReaderInboxApplication
	*ReaderTodoApplication
	*ReaderLibraryApplication
	*ReaderHostApplication
}

func newReaderTestFeatureSet(stores ReaderStores, ai ReaderAIBackend, options ...ReaderApplicationOptions) *readerTestFeatureSet {
	var configured ReaderApplicationOptions
	if len(options) > 0 {
		configured = options[0]
	}
	if configured.InboxConfirmCommands == nil {
		configured.InboxConfirmCommands, _ = stores.Inbox.(ReaderInboxConfirmCommands)
	}
	if configured.InboxBulkConfirmCommands == nil {
		configured.InboxBulkConfirmCommands, _ = stores.Inbox.(ReaderInboxBulkConfirmCommands)
	}
	if configured.InboxAIConfirmCommands == nil {
		configured.InboxAIConfirmCommands, _ = stores.Inbox.(ReaderInboxAIConfirmCommands)
	}
	if configured.FeedFeedbackCommands == nil {
		configured.FeedFeedbackCommands, _ = stores.Library.(ReaderFeedFeedbackCommands)
	}
	if configured.HostRestoreCommands == nil {
		configured.HostRestoreCommands, _ = stores.Hosts.(ReaderHostRestoreCommands)
	}
	fillMissingReaderTestCommands(&configured)
	applications := NewReaderApplications(stores, ai, configured)
	return &readerTestFeatureSet{
		ReaderThoughtApplication: applications.Thoughts,
		ReaderNoteApplication:    applications.Notes,
		ReaderInboxApplication:   applications.Inbox,
		ReaderTodoApplication:    applications.Todos,
		ReaderLibraryApplication: applications.Library,
		ReaderHostApplication:    applications.Hosts,
	}
}

func fillMissingReaderTestCommands(options *ReaderApplicationOptions) {
	missing := readerTestMissingCommands{}
	if options.InboxProposalCommands == nil {
		options.InboxProposalCommands = missing
	}
	if options.InboxConfirmCommands == nil {
		options.InboxConfirmCommands = missing
	}
	if options.InboxBulkConfirmCommands == nil {
		options.InboxBulkConfirmCommands = missing
	}
	if options.InboxAIConfirmCommands == nil {
		options.InboxAIConfirmCommands = missing
	}
	if options.FeedFeedbackCommands == nil {
		options.FeedFeedbackCommands = missing
	}
	if options.HostRestoreCommands == nil {
		options.HostRestoreCommands = missing
	}
}

var errReaderTestCommandNotImplemented = errors.New("reader test command dependency not implemented")

type readerTestMissingCommands struct{}

func (readerTestMissingCommands) CreateInboxProposal(context.Context, CreateInboxProposalCommand) (InboxProposalResult, error) {
	return InboxProposalResult{}, errReaderTestCommandNotImplemented
}

func (readerTestMissingCommands) EnsureInboxProposal(context.Context, EnsureInboxProposalCommand) (InboxProposalResult, error) {
	return InboxProposalResult{}, errReaderTestCommandNotImplemented
}

func (readerTestMissingCommands) ConfirmInbox(context.Context, uuid.UUID, *int64) (uuid.UUID, error) {
	return uuid.Nil, errReaderTestCommandNotImplemented
}

func (readerTestMissingCommands) BulkConfirmInbox(context.Context, []model.ReaderInboxBulkConfirmation) ([]model.ReaderInboxBulkResult, error) {
	return nil, errReaderTestCommandNotImplemented
}

func (readerTestMissingCommands) ConfirmAIProposals(context.Context, model.ReaderInboxPartition) (model.ReaderInboxAIProposalConfirmation, error) {
	return model.ReaderInboxAIProposalConfirmation{}, errReaderTestCommandNotImplemented
}

func (readerTestMissingCommands) FeedbackFeed(context.Context, string, string) (model.ReaderFeedFeedback, error) {
	return model.ReaderFeedFeedback{}, errReaderTestCommandNotImplemented
}

func (readerTestMissingCommands) RestoreHost(context.Context, model.ReaderHostKind, uuid.UUID) (model.ReaderHostLifecycleResult, error) {
	return model.ReaderHostLifecycleResult{}, errReaderTestCommandNotImplemented
}

func problemHTTPStatus(err *problem.Error) int {
	carrier, ok := httperr.As(err)
	if !ok {
		return 0
	}
	return carrier.HTTPStatus()
}
