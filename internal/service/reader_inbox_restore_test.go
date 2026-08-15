package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/httperr"
	"webtag/internal/repository"
)

type readerInboxRestoreStoreStub struct {
	repository.ReaderVNextStore
	restoreErr   error
	restoredID   uuid.UUID
	restoreCalls int
}

func (s *readerInboxRestoreStoreStub) RestoreInbox(_ context.Context, id uuid.UUID) error {
	s.restoreCalls++
	s.restoredID = id
	return s.restoreErr
}

func TestRestoreInboxUsesDedicatedRepositoryCommandForRetries(t *testing.T) {
	inboxID := uuid.New()
	store := &readerInboxRestoreStoreStub{}
	service := NewReaderVNextService(store, nil)

	if err := service.RestoreInbox(context.Background(), inboxID.String()); err != nil {
		t.Fatalf("first RestoreInbox() error = %v", err)
	}
	if err := service.RestoreInbox(context.Background(), inboxID.String()); err != nil {
		t.Fatalf("retry RestoreInbox() error = %v", err)
	}
	if store.restoreCalls != 2 || store.restoredID != inboxID {
		t.Fatalf("RestoreInbox() calls/id = %d/%s, want 2/%s", store.restoreCalls, store.restoredID, inboxID)
	}
}

func TestRestoreInboxMapsMissingAndInvalidIdentityErrors(t *testing.T) {
	tests := []struct {
		name       string
		rawID      string
		restoreErr error
		status     int
		code       string
	}{
		{name: "invalid id", rawID: "not-a-uuid", status: http.StatusUnprocessableEntity, code: "invalid_inbox_id"},
		{name: "missing item", rawID: uuid.NewString(), restoreErr: repository.ErrNotFound, status: http.StatusNotFound, code: "reader_not_found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &readerInboxRestoreStoreStub{restoreErr: tc.restoreErr}
			service := NewReaderVNextService(store, nil)

			err := service.RestoreInbox(context.Background(), tc.rawID)
			assertReaderHTTPError(t, err, tc.status, tc.code)
			if tc.rawID == "not-a-uuid" && store.restoreCalls != 0 {
				t.Fatalf("RestoreInbox() calls = %d, want 0 for invalid identity", store.restoreCalls)
			}
		})
	}
}

func assertReaderHTTPError(t *testing.T, err error, wantStatus int, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want status %d (%s)", wantStatus, wantCode)
	}
	carrier, ok := httperr.As(err)
	if !ok {
		t.Fatalf("error = %v, want httperr carrier", err)
	}
	if carrier.HTTPStatus() != wantStatus {
		t.Fatalf("HTTP status = %d, want %d", carrier.HTTPStatus(), wantStatus)
	}
	coder, ok := carrier.(httperr.ErrorCoder)
	if !ok || coder.HTTPErrorCode() != wantCode {
		t.Fatalf("error code = %q, want %q", errorCode(carrier), wantCode)
	}
}

func errorCode(carrier httperr.StatusCarrier) string {
	if coder, ok := carrier.(httperr.ErrorCoder); ok {
		return coder.HTTPErrorCode()
	}
	return ""
}
