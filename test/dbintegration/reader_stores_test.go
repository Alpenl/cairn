package dbintegration

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/app/durablework"
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

func postgresReaderApplications(t *testing.T, pool *pgxpool.Pool, store *repository.PGXReaderVNextRepository) *service.ReaderApplications {
	t.Helper()
	stores := postgresReaderStores(store)
	return service.NewReaderApplications(stores, nil, postgresReaderApplicationOptions(t, pool, store))
}

func postgresReaderApplicationOptions(t *testing.T, pool *pgxpool.Pool, store *repository.PGXReaderVNextRepository) service.ReaderApplicationOptions {
	t.Helper()
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	inboxCommands := durablework.NewInboxCommands(pool, store, queue)
	readerCommands := durablework.NewReaderCommands(pool, store, queue)
	return service.ReaderApplicationOptions{
		InboxProposalCommands:    inboxCommands,
		InboxConfirmCommands:     readerCommands,
		InboxBulkConfirmCommands: readerCommands,
		InboxAIConfirmCommands:   readerCommands,
		FeedFeedbackCommands:     readerCommands,
		HostRestoreCommands:      readerCommands,
	}
}
