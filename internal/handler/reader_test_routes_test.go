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

func readerTestApplications(store any, options ...service.ReaderApplicationOptions) *service.ReaderApplications {
	configured := service.ReaderApplicationOptions{}
	if len(options) > 0 {
		configured = options[0]
	}
	stores := readerServiceTestStores(store)
	if configured.InboxProposalCommands == nil {
		configured.InboxProposalCommands, _ = store.(service.InboxProposalCommands)
	}
	if configured.InboxConfirmCommands == nil {
		configured.InboxConfirmCommands, _ = store.(service.ReaderInboxConfirmCommands)
	}
	if configured.InboxBulkConfirmCommands == nil {
		configured.InboxBulkConfirmCommands, _ = store.(service.ReaderInboxBulkConfirmCommands)
	}
	if configured.InboxAIConfirmCommands == nil {
		configured.InboxAIConfirmCommands, _ = store.(service.ReaderInboxAIConfirmCommands)
	}
	if configured.FeedFeedbackCommands == nil {
		configured.FeedFeedbackCommands, _ = store.(service.ReaderFeedFeedbackCommands)
	}
	if configured.HostRestoreCommands == nil {
		configured.HostRestoreCommands, _ = store.(service.ReaderHostRestoreCommands)
	}
	fillMissingReaderHandlerTestCommands(&configured)
	return service.NewReaderApplications(stores, nil, configured)
}

func fillMissingReaderHandlerTestCommands(options *service.ReaderApplicationOptions) {
	missing := readerHandlerTestMissingCommands{}
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

var errReaderHandlerTestCommandNotImplemented = errors.New("reader handler test command dependency not implemented")

type readerHandlerTestMissingCommands struct{}

func (readerHandlerTestMissingCommands) CreateInboxProposal(context.Context, service.CreateInboxProposalCommand) (service.InboxProposalResult, error) {
	return service.InboxProposalResult{}, errReaderHandlerTestCommandNotImplemented
}

func (readerHandlerTestMissingCommands) EnsureInboxProposal(context.Context, service.EnsureInboxProposalCommand) (service.InboxProposalResult, error) {
	return service.InboxProposalResult{}, errReaderHandlerTestCommandNotImplemented
}

func (readerHandlerTestMissingCommands) ConfirmInbox(context.Context, uuid.UUID, *int64) (uuid.UUID, error) {
	return uuid.Nil, errReaderHandlerTestCommandNotImplemented
}

func (readerHandlerTestMissingCommands) BulkConfirmInbox(context.Context, []model.ReaderInboxBulkConfirmation) ([]model.ReaderInboxBulkResult, error) {
	return nil, errReaderHandlerTestCommandNotImplemented
}

func (readerHandlerTestMissingCommands) ConfirmAIProposals(context.Context, model.ReaderInboxPartition) (model.ReaderInboxAIProposalConfirmation, error) {
	return model.ReaderInboxAIProposalConfirmation{}, errReaderHandlerTestCommandNotImplemented
}

func (readerHandlerTestMissingCommands) FeedbackFeed(context.Context, string, string) (model.ReaderFeedFeedback, error) {
	return model.ReaderFeedFeedback{}, errReaderHandlerTestCommandNotImplemented
}

func (readerHandlerTestMissingCommands) RestoreHost(context.Context, model.ReaderHostKind, uuid.UUID) (model.ReaderHostLifecycleResult, error) {
	return model.ReaderHostLifecycleResult{}, errReaderHandlerTestCommandNotImplemented
}
