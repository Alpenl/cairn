package dbintegration

import (
	"context"
	"errors"

	"webtag/internal/repository"
	"webtag/internal/service"
)

func postgresReaderStores(store *repository.PGXReaderVNextRepository) service.ReaderStores {
	return service.ReaderStores{
		Thoughts: store,
		Notes:    store,
		Inbox:    store,
		Todos:    store,
		Library:  store,
		Hosts:    store,
	}
}

func postgresReaderApplications(store *repository.PGXReaderVNextRepository) *service.ReaderApplications {
	return service.NewReaderApplications(postgresReaderStores(store), nil, postgresReaderApplicationOptions(store))
}

func postgresReaderApplicationOptions(store *repository.PGXReaderVNextRepository) service.ReaderApplicationOptions {
	return service.ReaderApplicationOptions{
		InboxProposalCommands:    dbIntegrationInboxProposalCommands{store: store},
		InboxConfirmCommands:     store,
		InboxBulkConfirmCommands: store,
		InboxAIConfirmCommands:   store,
		FeedFeedbackCommands:     store,
		HostRestoreCommands:      store,
	}
}

type dbIntegrationInboxProposalCommands struct {
	store *repository.PGXReaderVNextRepository
}

func (c dbIntegrationInboxProposalCommands) CreateInboxProposal(ctx context.Context, command service.CreateInboxProposalCommand) (service.InboxProposalResult, error) {
	item, err := c.store.CreateInbox(ctx, command.Inbox)
	if err != nil {
		return service.InboxProposalResult{}, err
	}
	return service.InboxProposalResult{Inbox: item}, nil
}

func (dbIntegrationInboxProposalCommands) EnsureInboxProposal(context.Context, service.EnsureInboxProposalCommand) (service.InboxProposalResult, error) {
	return service.InboxProposalResult{}, errors.New("dbintegration: EnsureInboxProposal requires durable queue commands")
}
