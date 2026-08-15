package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type readerReattachStoreStub struct {
	repository.ReaderVNextStore

	command model.ReaderThoughtReattachCommand
	calls   int
	err     error
}

func (s *readerReattachStoreStub) ReattachThought(_ context.Context, command model.ReaderThoughtReattachCommand) (*model.ReaderThought, error) {
	s.calls++
	s.command = command
	return nil, s.err
}

func TestReaderReattachThoughtRejectsInvalidRequestBeforeStore(t *testing.T) {
	targetID := uuid.New()
	base := dto.ReaderThoughtReattachRequest{
		TargetHostKind:       "link",
		TargetHostID:         targetID.String(),
		ExpectedLastSequence: 4,
		ExpectedHostRevision: 2,
	}
	tests := []struct {
		name       string
		request    dto.ReaderThoughtReattachRequest
		wantCode   string
		wantStatus int
	}{
		{
			name:       "unsupported target kind",
			request:    dto.ReaderThoughtReattachRequest{TargetHostKind: "site", TargetHostID: targetID.String(), ExpectedLastSequence: 4, ExpectedHostRevision: 2},
			wantCode:   "invalid_thought_host",
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name:       "target host id is not a UUID",
			request:    dto.ReaderThoughtReattachRequest{TargetHostKind: base.TargetHostKind, TargetHostID: "not-a-uuid", ExpectedLastSequence: base.ExpectedLastSequence, ExpectedHostRevision: base.ExpectedHostRevision},
			wantCode:   "invalid_target_host_id",
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name:       "negative source sequence",
			request:    dto.ReaderThoughtReattachRequest{TargetHostKind: base.TargetHostKind, TargetHostID: base.TargetHostID, ExpectedLastSequence: -1, ExpectedHostRevision: base.ExpectedHostRevision},
			wantCode:   "reader_last_sequence_invalid",
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name:       "non-positive target revision",
			request:    dto.ReaderThoughtReattachRequest{TargetHostKind: base.TargetHostKind, TargetHostID: base.TargetHostID, ExpectedLastSequence: base.ExpectedLastSequence, ExpectedHostRevision: 0},
			wantCode:   "reader_host_revision_required",
			wantStatus: http.StatusUnprocessableEntity,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &readerReattachStoreStub{}
			service := NewReaderVNextService(store, nil)
			_, err := service.ReattachThought(context.Background(), "thought-1", tc.request)
			assertReaderReattachHTTPError(t, err, tc.wantStatus, tc.wantCode)
			if store.calls != 0 {
				t.Fatalf("ReattachThought store calls = %d, want 0", store.calls)
			}
		})
	}
}

func TestReaderReattachThoughtMapsLifecycleAndCASFailures(t *testing.T) {
	targetID := uuid.New()
	request := dto.ReaderThoughtReattachRequest{
		TargetHostKind:       "link",
		TargetHostID:         targetID.String(),
		ExpectedLastSequence: 4,
		ExpectedHostRevision: 2,
	}
	tests := []struct {
		name       string
		storeErr   error
		wantCode   string
		wantStatus int
	}{
		{name: "missing source or target", storeErr: repository.ErrNotFound, wantStatus: http.StatusNotFound, wantCode: "reader_not_found"},
		{name: "active source is not reattachable", storeErr: repository.ErrReaderThoughtReattachInvalidState, wantStatus: http.StatusConflict, wantCode: "thought_reattach_invalid_state"},
		{name: "source or target revision changed", storeErr: repository.ErrRevisionConflict, wantStatus: http.StatusConflict, wantCode: httperr.CodeRevisionConflict},
		{name: "quote cannot be uniquely reanchored", storeErr: repository.ErrInvalidReaderReanchor, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_reanchor_ops"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &readerReattachStoreStub{err: tc.storeErr}
			service := NewReaderVNextService(store, nil)
			_, err := service.ReattachThought(context.Background(), "thought-1", request)
			assertReaderReattachHTTPError(t, err, tc.wantStatus, tc.wantCode)
			if store.calls != 1 {
				t.Fatalf("ReattachThought store calls = %d, want 1", store.calls)
			}
			if store.command.ThoughtID != "thought-1" || store.command.TargetHostID != targetID.String() {
				t.Fatalf("ReattachThought command = %#v, want normalized IDs", store.command)
			}
		})
	}
}

func assertReaderReattachHTTPError(t *testing.T, err error, wantStatus int, wantCode string) {
	t.Helper()
	carrier, ok := httperr.As(err)
	if !ok {
		t.Fatalf("error = %v, want HTTP error", err)
	}
	if carrier.HTTPStatus() != wantStatus {
		t.Fatalf("HTTP status = %d, want %d; error=%v", carrier.HTTPStatus(), wantStatus, err)
	}
	coder, ok := carrier.(httperr.ErrorCoder)
	if !ok || coder.HTTPErrorCode() != wantCode {
		got := ""
		if ok {
			got = coder.HTTPErrorCode()
		}
		t.Fatalf("error code = %q, want %q; error=%v", got, wantCode, err)
	}
}
