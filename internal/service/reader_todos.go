package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/problem"
)

type ReaderTodoCreateCommand struct {
	Text  string
	DueAt *time.Time
}

type ReaderTodoPatchCommand struct {
	ID                      uuid.UUID
	Text                    *string
	DueAt                   *time.Time
	DueAtSet                bool
	Done                    *bool
	ExpectedHostRevision    *int64
	ExpectedHostRevisionSet bool
}

func (s *ReaderTodoApplication) CreateTodo(ctx context.Context, command ReaderTodoCreateCommand) (model.ReaderTodo, error) {
	text := strings.TrimSpace(command.Text)
	if text == "" {
		return model.ReaderTodo{}, problem.NewWithCode(problem.Invalid, "todo_text_required", "todo text is required")
	}
	todo, err := s.todos.CreateTodo(ctx, model.ReaderTodo{Text: text, DueAt: command.DueAt, OriginKind: "standalone"})
	if err != nil {
		return model.ReaderTodo{}, mapReaderError(err)
	}
	return *todo, nil
}

// ListTodos pages the stored projection and nothing else. Thought and Note
// commands maintain the projection as they commit, so reads do not reconcile
// or rewrite the entire installation.
func (s *ReaderTodoApplication) ListTodos(ctx context.Context, after string, limit int) (model.ReaderTodoPage, error) {
	page, err := s.todos.ListTodos(ctx, after, limit)
	if err != nil {
		return model.ReaderTodoPage{}, mapReaderError(err)
	}
	return page, nil
}

func (s *ReaderTodoApplication) PatchTodo(ctx context.Context, command ReaderTodoPatchCommand) (model.ReaderTodo, error) {
	item, err := s.todos.PatchTodo(ctx, model.ReaderTodoPatch{
		ID:                      command.ID,
		Text:                    command.Text,
		DueAt:                   command.DueAt,
		DueAtSet:                command.DueAtSet,
		Done:                    command.Done,
		ExpectedHostRevision:    command.ExpectedHostRevision,
		ExpectedHostRevisionSet: command.ExpectedHostRevisionSet,
	})
	if err != nil {
		return model.ReaderTodo{}, mapReaderError(err)
	}
	return *item, nil
}

func (s *ReaderTodoApplication) DeleteTodo(ctx context.Context, id uuid.UUID) error {
	return mapReaderError(s.todos.DeleteTodo(ctx, id))
}
