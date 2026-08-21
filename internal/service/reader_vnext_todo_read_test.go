package service

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// readerTodoReadStore records source scans. TODO reads must use their stored
// projection without walking Thought or Note bodies.
type readerTodoReadStore struct {
	repository.ReaderVNextStore

	page      model.ReaderTodoPage
	aggregate repository.ReaderHomeAggregate
	sources   int
}

func (s *readerTodoReadStore) ListTodos(context.Context, string, int) (model.ReaderTodoPage, error) {
	return s.page, nil
}

func (s *readerTodoReadStore) LoadHomeAggregate(context.Context) (repository.ReaderHomeAggregate, error) {
	return s.aggregate, nil
}

func (s *readerTodoReadStore) ListThoughts(context.Context, string, string, int) ([]model.ReaderThought, string, error) {
	s.sources++
	return nil, "", nil
}

func (s *readerTodoReadStore) ListNotes(context.Context, string, int) ([]model.ReaderNote, int, string, error) {
	s.sources++
	return nil, 0, "", nil
}

// TestTodoReadsDoNotReconcile locks the read contract this change exists for:
// listing TODOs and loading Home must page the stored projection without
// touching a single source document or rewriting anything.
func TestTodoReadsDoNotReconcile(t *testing.T) {
	t.Parallel()

	hostKind, hostID := "note", uuid.New().String()
	projection := model.ReaderTodo{
		ID: uuid.New(), Text: "stored projection", OriginKind: "note",
		OriginHostKind: &hostKind, OriginHostID: &hostID, HostRevision: 3,
	}
	store := &readerTodoReadStore{
		page:      model.ReaderTodoPage{Items: []model.ReaderTodo{projection}},
		aggregate: repository.ReaderHomeAggregate{Todos: []model.ReaderTodo{projection}, Counts: map[string]int{"todos": 1}},
	}
	service := NewReaderVNextService(store, nil)
	ctx := context.Background()

	todos, err := service.ListTodos(ctx, "", 200)
	if err != nil {
		t.Fatalf("ListTodos() error = %v", err)
	}
	if len(todos.Items) != 1 || todos.Items[0].Text != "stored projection" {
		t.Fatalf("ListTodos() = %#v, want the stored projection", todos)
	}
	if _, err := service.Home(ctx); err != nil {
		t.Fatalf("Home() error = %v", err)
	}
	if store.sources != 0 {
		t.Fatalf("reads scanned source documents %d times, want 0", store.sources)
	}
}
