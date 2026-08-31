package handler

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/service"
)

func readerTestRoutes(candidate any) ReaderRoutes {
	if applications, ok := candidate.(*service.ReaderApplications); ok {
		return ReaderRoutes{
			Thoughts: NewReaderThoughtRoutes(applications.Thoughts), Notes: NewReaderNoteRoutes(applications.Notes), Inbox: NewReaderInboxRoutes(applications.Inbox),
			Todos: NewReaderTodoRoutes(applications.Todos), Library: NewReaderLibraryRoutes(applications.Library), Hosts: NewReaderHostRoutes(applications.Hosts),
		}
	}
	var routes ReaderRoutes
	routes.Thoughts, _ = candidate.(ReaderThoughtRoutes)
	routes.Notes, _ = candidate.(ReaderNoteRoutes)
	routes.Inbox, _ = candidate.(ReaderInboxRoutes)
	routes.Todos, _ = candidate.(ReaderTodoRoutes)
	routes.Library, _ = candidate.(ReaderLibraryRoutes)
	routes.Hosts, _ = candidate.(ReaderHostRoutes)
	return routes
}

func readerServiceTestStores(store any) service.ReaderStores {
	var stores service.ReaderStores
	stores.Thoughts, _ = store.(service.ReaderThoughtStore)
	stores.Notes, _ = store.(service.ReaderNoteStore)
	stores.Inbox, _ = store.(service.ReaderInboxStore)
	stores.Todos, _ = store.(service.ReaderTodoStore)
	stores.Library, _ = store.(service.ReaderLibraryStore)
	stores.Hosts, _ = store.(service.ReaderHostStore)
	return stores
}

func readerServiceTestApplications(store any, options ...service.ReaderApplicationOptions) *service.ReaderApplications {
	stores := readerServiceTestStores(store)
	return service.NewReaderApplications(stores, nil, readerServiceTestOptions(stores, options...))
}

func readerServiceTestOptions(stores service.ReaderStores, options ...service.ReaderApplicationOptions) service.ReaderApplicationOptions {
	var configured service.ReaderApplicationOptions
	if len(options) > 0 {
		configured = options[0]
	}
	noCommands := readerNoopCommands{}
	if configured.InboxProposalCommands == nil {
		configured.InboxProposalCommands, _ = stores.Inbox.(service.InboxProposalCommands)
		if configured.InboxProposalCommands == nil {
			configured.InboxProposalCommands = noCommands
		}
	}
	if configured.InboxConfirmCommands == nil {
		configured.InboxConfirmCommands, _ = stores.Inbox.(service.ReaderInboxConfirmCommands)
		if configured.InboxConfirmCommands == nil {
			configured.InboxConfirmCommands = noCommands
		}
	}
	if configured.InboxBulkConfirmCommands == nil {
		configured.InboxBulkConfirmCommands, _ = stores.Inbox.(service.ReaderInboxBulkConfirmCommands)
		if configured.InboxBulkConfirmCommands == nil {
			configured.InboxBulkConfirmCommands = noCommands
		}
	}
	if configured.InboxAIConfirmCommands == nil {
		configured.InboxAIConfirmCommands, _ = stores.Inbox.(service.ReaderInboxAIConfirmCommands)
		if configured.InboxAIConfirmCommands == nil {
			configured.InboxAIConfirmCommands = noCommands
		}
	}
	if configured.FeedFeedbackCommands == nil {
		configured.FeedFeedbackCommands, _ = stores.Library.(service.ReaderFeedFeedbackCommands)
		if configured.FeedFeedbackCommands == nil {
			configured.FeedFeedbackCommands = noCommands
		}
	}
	if configured.HostRestoreCommands == nil {
		configured.HostRestoreCommands, _ = stores.Hosts.(service.ReaderHostRestoreCommands)
		if configured.HostRestoreCommands == nil {
			configured.HostRestoreCommands = noCommands
		}
	}
	return configured
}

type readerNoopCommands struct{}

func (readerNoopCommands) CreateInboxProposal(context.Context, service.CreateInboxProposalCommand) (service.InboxProposalResult, error) {
	return service.InboxProposalResult{}, errors.New("unexpected test call: CreateInboxProposal")
}

func (readerNoopCommands) EnsureInboxProposal(context.Context, service.EnsureInboxProposalCommand) (service.InboxProposalResult, error) {
	return service.InboxProposalResult{}, errors.New("unexpected test call: EnsureInboxProposal")
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
