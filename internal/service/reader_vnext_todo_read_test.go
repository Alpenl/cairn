package service

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// readerTodoReadStore fails every projection-maintenance call. A read that
// still reconciles therefore cannot pass this test quietly — it errors — while
// the explicit repair is expected to reach the store.
type readerTodoReadStore struct {
	repository.ReaderVNextStore

	page      model.ReaderTodoPage
	aggregate repository.ReaderHomeAggregate
	repairs   int
	sources   int
}

func (s *readerTodoReadStore) ListTodos(context.Context, string, int) (model.ReaderTodoPage, error) {
	return s.page, nil
}

func (s *readerTodoReadStore) LoadHomeAggregate(context.Context) (repository.ReaderHomeAggregate, error) {
	return s.aggregate, nil
}

func (s *readerTodoReadStore) RepairTodoProjections(context.Context) (int, error) {
	s.repairs++
	return 7, nil
}

func (s *readerTodoReadStore) ListThoughts(context.Context, string, string, int) ([]model.ReaderThought, string, error) {
	s.sources++
	return nil, "", nil
}

func (s *readerTodoReadStore) ListNotes(context.Context, string, int) ([]model.ReaderNote, int, string, error) {
	s.sources++
	return nil, 0, "", nil
}

func (s *readerTodoReadStore) ReconcileTodoProjections(context.Context, []model.ReaderTodo) error {
	s.repairs++
	return nil
}

// TestTodoReadsDoNotReconcile locks the read contract this change exists for:
// listing TODOs and loading Home must page the stored projection without
// touching a single source document or rewriting anything. The repair stays
// available, but only when an operator or a test asks for it by name.
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
	if store.repairs != 0 {
		t.Fatalf("reads reconciled the projection %d times, want 0", store.repairs)
	}
	if store.sources != 0 {
		t.Fatalf("reads scanned source documents %d times, want 0", store.sources)
	}

	projected, err := service.RepairProjectedTodos(ctx)
	if err != nil {
		t.Fatalf("RepairProjectedTodos() error = %v", err)
	}
	if projected != 7 || store.repairs != 1 {
		t.Fatalf("RepairProjectedTodos() = %d with %d store calls, want 7 with 1", projected, store.repairs)
	}
}
