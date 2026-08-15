package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

type readerInboxPartitionStoreStub struct {
	repository.ReaderVNextStore
	items            []model.ReaderInbox
	activeCount      int
	expiredCount     int
	next             string
	listCalls        int
	listPartition    model.ReaderInboxPartition
	confirmResult    model.ReaderInboxAIProposalConfirmation
	confirmCalls     int
	confirmPartition model.ReaderInboxPartition
}

func (s *readerInboxPartitionStoreStub) ListInbox(_ context.Context, partition model.ReaderInboxPartition, _ string, _ int) ([]model.ReaderInbox, int, int, string, error) {
	s.listCalls++
	s.listPartition = partition
	return append([]model.ReaderInbox(nil), s.items...), s.activeCount, s.expiredCount, s.next, nil
}

func (s *readerInboxPartitionStoreStub) ConfirmAIProposals(_ context.Context, partition model.ReaderInboxPartition) (model.ReaderInboxAIProposalConfirmation, error) {
	s.confirmCalls++
	s.confirmPartition = partition
	return s.confirmResult, nil
}

func TestListInboxDefaultsToActiveAndMapsExpiryCounts(t *testing.T) {
	now := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	expiresAt := now.Add(30 * 24 * time.Hour)
	store := &readerInboxPartitionStoreStub{
		items: []model.ReaderInbox{{
			ID:        uuid.New(),
			URL:       "https://example.com/active",
			Status:    "pending",
			ExpiresAt: &expiresAt,
			Expired:   false,
			CreatedAt: now,
			UpdatedAt: now,
		}},
		activeCount:  3,
		expiredCount: 2,
		next:         "next-page",
	}

	page, err := NewReaderVNextService(store, nil).ListInbox(context.Background(), "", "", 30)
	if err != nil {
		t.Fatalf("ListInbox() error = %v", err)
	}
	if store.listCalls != 1 || store.listPartition != model.ReaderInboxPartitionActive {
		t.Fatalf("ListInbox() partition calls = %d/%q, want 1/%q", store.listCalls, store.listPartition, model.ReaderInboxPartitionActive)
	}
	if page.ActiveCount != 3 || page.ExpiredCount != 2 || page.NextCursor != "next-page" || len(page.Items) != 1 {
		t.Fatalf("ListInbox() page = %#v", page)
	}
	if page.Items[0].ExpiresAt == nil || !page.Items[0].ExpiresAt.Equal(expiresAt) || page.Items[0].ExpiredAt != nil || page.Items[0].Expired {
		t.Fatalf("ListInbox() expiry mapping = %#v", page.Items[0])
	}
}

func TestReaderInboxPartitionValidationStopsStorageCalls(t *testing.T) {
	store := &readerInboxPartitionStoreStub{}
	service := NewReaderVNextService(store, nil)

	_, err := service.ListInbox(context.Background(), "other", "", 30)
	assertReaderHTTPError(t, err, http.StatusUnprocessableEntity, "invalid_inbox_partition")
	if store.listCalls != 0 {
		t.Fatalf("ListInbox() storage calls = %d, want 0", store.listCalls)
	}

	_, err = service.ConfirmAIProposals(context.Background(), "")
	assertReaderHTTPError(t, err, http.StatusUnprocessableEntity, "invalid_inbox_partition")
	if store.confirmCalls != 0 {
		t.Fatalf("ConfirmAIProposals() storage calls = %d, want 0", store.confirmCalls)
	}
}

func TestConfirmAIProposalsMapsServerSelectedAtomicBatch(t *testing.T) {
	first, second, linkID := uuid.New(), uuid.New(), uuid.New()
	store := &readerInboxPartitionStoreStub{confirmResult: model.ReaderInboxAIProposalConfirmation{
		Items: []model.ReaderInboxBulkResult{
			{ID: first, Status: "confirmed", LinkID: &linkID},
			{ID: second, Status: "confirmed"},
		},
		RemainingCount: 4,
	}}

	response, err := NewReaderVNextService(store, nil).ConfirmAIProposals(context.Background(), "expired")
	if err != nil {
		t.Fatalf("ConfirmAIProposals() error = %v", err)
	}
	if store.confirmCalls != 1 || store.confirmPartition != model.ReaderInboxPartitionExpired {
		t.Fatalf("ConfirmAIProposals() storage calls/partition = %d/%q", store.confirmCalls, store.confirmPartition)
	}
	if !response.Atomic || response.RemainingCount != 4 || len(response.Items) != 2 {
		t.Fatalf("ConfirmAIProposals() response = %#v", response)
	}
	if response.Items[0].InboxID != first.String() || response.Items[0].LinkID == nil || *response.Items[0].LinkID != linkID.String() || response.Items[1].InboxID != second.String() {
		t.Fatalf("ConfirmAIProposals() items = %#v", response.Items)
	}
}
