package handler

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/service"
)

type readerInboxApplicationStore struct {
	service.ReaderInboxStore
	bulkCalls    int
	restoreCalls int
}

func (s *readerInboxApplicationStore) BulkConfirmInbox(_ context.Context, confirmations []model.ReaderInboxBulkConfirmation) ([]model.ReaderInboxBulkResult, error) {
	s.bulkCalls++
	return nil, nil
}

func (s *readerInboxApplicationStore) RestoreInbox(context.Context, uuid.UUID) error {
	s.restoreCalls++
	return nil
}

func TestReaderInboxRoutesRejectPartialBatchRevisionsBeforeCommands(t *testing.T) {
	t.Parallel()

	store := &readerInboxApplicationStore{}
	applications := readerTestApplications(store, service.ReaderApplicationOptions{InboxBulkConfirmCommands: store})
	routes := NewReaderInboxRoutes(applications.Inbox)
	first, second := uuid.NewString(), uuid.NewString()

	_, err := routes.ConfirmInboxBulk(context.Background(), []string{first, second}, map[string]int64{first: 4})
	if err == nil {
		t.Fatal("ConfirmInboxBulk() error = nil for partial expected revisions")
	}
	if store.bulkCalls != 0 {
		t.Fatalf("ConfirmInboxBulk() command calls = %d, want 0", store.bulkCalls)
	}
}

func TestReaderInboxRoutesRejectInvalidIDBeforeApplicationStore(t *testing.T) {
	t.Parallel()

	store := &readerInboxApplicationStore{}
	applications := readerTestApplications(store)
	routes := NewReaderInboxRoutes(applications.Inbox)

	if err := routes.RestoreInbox(context.Background(), "not-a-uuid"); err == nil {
		t.Fatal("RestoreInbox() error = nil for invalid id")
	}
	if store.restoreCalls != 0 {
		t.Fatalf("RestoreInbox() store calls = %d, want 0", store.restoreCalls)
	}
}
