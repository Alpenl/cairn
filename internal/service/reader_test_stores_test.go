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
	noCommands := readerNoopCommands{}
	if configured.InboxProposalCommands == nil {
		configured.InboxProposalCommands, _ = stores.Inbox.(InboxProposalCommands)
		if configured.InboxProposalCommands == nil {
			configured.InboxProposalCommands = noCommands
		}
	}
	if configured.InboxConfirmCommands == nil {
		configured.InboxConfirmCommands, _ = stores.Inbox.(ReaderInboxConfirmCommands)
		if configured.InboxConfirmCommands == nil {
			configured.InboxConfirmCommands = noCommands
		}
	}
	if configured.InboxBulkConfirmCommands == nil {
		configured.InboxBulkConfirmCommands, _ = stores.Inbox.(ReaderInboxBulkConfirmCommands)
		if configured.InboxBulkConfirmCommands == nil {
			configured.InboxBulkConfirmCommands = noCommands
		}
	}
	if configured.InboxAIConfirmCommands == nil {
		configured.InboxAIConfirmCommands, _ = stores.Inbox.(ReaderInboxAIConfirmCommands)
		if configured.InboxAIConfirmCommands == nil {
			configured.InboxAIConfirmCommands = noCommands
		}
	}
	if configured.FeedFeedbackCommands == nil {
		configured.FeedFeedbackCommands, _ = stores.Library.(ReaderFeedFeedbackCommands)
		if configured.FeedFeedbackCommands == nil {
			configured.FeedFeedbackCommands = noCommands
		}
	}
	if configured.HostRestoreCommands == nil {
		configured.HostRestoreCommands, _ = stores.Hosts.(ReaderHostRestoreCommands)
		if configured.HostRestoreCommands == nil {
			configured.HostRestoreCommands = noCommands
		}
	}
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

func problemHTTPStatus(err *problem.Error) int {
	carrier, ok := httperr.As(err)
	if !ok {
		return 0
	}
	return carrier.HTTPStatus()
}

type readerNoopCommands struct{}

func (readerNoopCommands) CreateInboxProposal(context.Context, CreateInboxProposalCommand) (InboxProposalResult, error) {
	return InboxProposalResult{}, errors.New("unexpected test call: CreateInboxProposal")
}

func (readerNoopCommands) EnsureInboxProposal(context.Context, EnsureInboxProposalCommand) (InboxProposalResult, error) {
	return InboxProposalResult{}, errors.New("unexpected test call: EnsureInboxProposal")
}

func (readerNoopCommands) ConfirmInbox(context.Context, uuid.UUID, *int64) (uuid.UUID, error) {
	return uuid.Nil, errors.New("unexpected test call: ConfirmInbox")
}

func (readerNoopCommands) BulkConfirmInbox(context.Context, []model.ReaderInboxBulkConfirmation) ([]model.ReaderInboxBulkResult, error) {
	return nil, errors.New("unexpected test call: BulkConfirmInbox")
}

func (readerNoopCommands) ConfirmAIProposals(context.Context, model.ReaderInboxPartition) (model.ReaderInboxAIProposalConfirmation, error) {
	return model.ReaderInboxAIProposalConfirmation{}, errors.New("unexpected test call: ConfirmAIProposals")
}

func (readerNoopCommands) FeedbackFeed(context.Context, string, string) (model.ReaderFeedFeedback, error) {
	return model.ReaderFeedFeedback{}, errors.New("unexpected test call: FeedbackFeed")
}

func (readerNoopCommands) RestoreHost(context.Context, model.ReaderHostKind, uuid.UUID) (model.ReaderHostLifecycleResult, error) {
	return model.ReaderHostLifecycleResult{}, errors.New("unexpected test call: RestoreHost")
}
