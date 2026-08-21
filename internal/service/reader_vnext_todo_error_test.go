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

type readerTodoPatchErrorStore struct {
	ReaderTodoStore
	calls int
	err   error
}

func (s *readerTodoPatchErrorStore) PatchTodo(context.Context, model.ReaderTodoPatch) (*model.ReaderTodo, error) {
	s.calls++
	return nil, s.err
}

type readerTodoPatchCaptureStore struct {
	ReaderTodoStore
	command *model.ReaderTodoPatch
	err     error
}

func (s *readerTodoPatchCaptureStore) PatchTodo(_ context.Context, command model.ReaderTodoPatch) (*model.ReaderTodo, error) {
	s.command = &command
	return nil, s.err
}

func TestReaderTodoPatchErrorContract(t *testing.T) {
	tests := []struct {
		name       string
		repository error
		wantStatus int
		wantCode   string
	}{
		{name: "not found wins before revision applicability", repository: repository.ErrNotFound, wantStatus: http.StatusNotFound, wantCode: "reader_not_found"},
		{name: "projected revision conflict", repository: repository.ErrRevisionConflict, wantStatus: http.StatusConflict, wantCode: httperr.CodeRevisionConflict},
		{name: "standalone host revision is not applicable", repository: repository.ErrReaderTodoHostRevisionNotApplicable, wantStatus: http.StatusUnprocessableEntity, wantCode: "todo_host_revision_not_applicable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &readerTodoPatchErrorStore{err: tt.repository}
			service := NewReaderVNextService(readerTestStores(store), nil)
			id := uuid.New().String()
			_, err := service.PatchTodo(context.Background(), id, dto.ReaderTodoPatchRequest{})
			carrier, ok := httperr.As(err)
			if !ok {
				t.Fatalf("PatchTodo() error = %v, want HTTP error", err)
			}
			if carrier.HTTPStatus() != tt.wantStatus {
				t.Fatalf("PatchTodo() status = %d, want %d", carrier.HTTPStatus(), tt.wantStatus)
			}
			coder, ok := carrier.(httperr.ErrorCoder)
			if !ok {
				t.Fatalf("PatchTodo() error = %v, want coded HTTP error", err)
			}
			if coder.HTTPErrorCode() != tt.wantCode {
				t.Fatalf("PatchTodo() error code = %q, want %q", coder.HTTPErrorCode(), tt.wantCode)
			}
			if store.calls != 1 {
				t.Fatalf("PatchTodo() store calls = %d, want 1", store.calls)
			}
		})
	}
}

func TestReaderTodoPatchRejectsInvalidIDBeforeStore(t *testing.T) {
	store := &readerTodoPatchErrorStore{}
	service := NewReaderVNextService(readerTestStores(store), nil)

	_, err := service.PatchTodo(context.Background(), "not-a-uuid", dto.ReaderTodoPatchRequest{})
	carrier, ok := httperr.As(err)
	if !ok || carrier.HTTPStatus() != http.StatusUnprocessableEntity {
		t.Fatalf("PatchTodo() error = %v, want 422 HTTP error", err)
	}
	coder, ok := carrier.(httperr.ErrorCoder)
	if !ok {
		t.Fatalf("PatchTodo() error = %v, want coded HTTP error", err)
	}
	if coder.HTTPErrorCode() != "invalid_todo_id" {
		t.Fatalf("PatchTodo() error code = %q, want invalid_todo_id", coder.HTTPErrorCode())
	}
	if store.calls != 0 {
		t.Fatalf("PatchTodo() store calls = %d, want 0", store.calls)
	}
}

func TestReaderTodoPatchPassesNegativeHostRevisionToProjectedCAS(t *testing.T) {
	negative := int64(-1)
	done := true
	store := &readerTodoPatchCaptureStore{err: repository.ErrRevisionConflict}
	service := NewReaderVNextService(readerTestStores(store), nil)

	_, err := service.PatchTodo(context.Background(), uuid.New().String(), dto.ReaderTodoPatchRequest{
		Done:                 &done,
		ExpectedHostRevision: &negative,
	})
	carrier, ok := httperr.As(err)
	if !ok || carrier.HTTPStatus() != http.StatusConflict {
		t.Fatalf("PatchTodo() error = %v, want 409 HTTP error", err)
	}
	coder, ok := carrier.(httperr.ErrorCoder)
	if !ok {
		t.Fatalf("PatchTodo() error = %v, want coded HTTP error", err)
	}
	if coder.HTTPErrorCode() != httperr.CodeRevisionConflict {
		t.Fatalf("PatchTodo() error code = %q, want %q", coder.HTTPErrorCode(), httperr.CodeRevisionConflict)
	}
	if store.command == nil {
		t.Fatal("PatchTodo() did not reach store")
	}
	if !store.command.ExpectedHostRevisionSet || store.command.ExpectedHostRevision == nil || *store.command.ExpectedHostRevision != negative {
		t.Fatalf("PatchTodo() command = %#v, want present revision -1", store.command)
	}
}
