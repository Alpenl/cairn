package service

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
)

func (s *ReaderVNextService) CreateTodo(ctx context.Context, request dto.ReaderTodoCreateRequest) (dto.ReaderTodoResponse, error) {
	text := strings.TrimSpace(request.Text)
	if text == "" {
		return dto.ReaderTodoResponse{}, problem.NewWithCode(problem.Invalid, "todo_text_required", "todo text is required")
	}
	todo, err := s.todos.CreateTodo(ctx, model.ReaderTodo{Text: text, DueAt: request.DueAt, OriginKind: "standalone"})
	if err != nil {
		return dto.ReaderTodoResponse{}, mapReaderError(err)
	}
	return todoResponse(*todo), nil
}

// ListTodos pages the stored projection and nothing else. It used to parse
// every Thought and published Note first and reconcile the whole projection,
// which made a read scale with the installation and write on every GET.
// Thought and Note commands now maintain the projection as they commit, so the
// stored rows are already the answer.
func (s *ReaderVNextService) ListTodos(ctx context.Context, after string, limit int) (dto.ReaderTodosResponse, error) {
	page, err := s.todos.ListTodos(ctx, after, limit)
	if err != nil {
		return dto.ReaderTodosResponse{}, mapReaderError(err)
	}
	out := dto.ReaderTodosResponse{Items: make([]dto.ReaderTodoResponse, 0, len(page.Items)), NextAfter: page.Next}
	for _, item := range page.Items {
		out.Items = append(out.Items, todoResponse(item))
	}
	return out, nil
}

func todoResponse(item model.ReaderTodo) dto.ReaderTodoResponse {
	return dto.ReaderTodoResponse{ID: item.ID.String(), Text: item.Text, DueAt: item.DueAt, Done: item.Done, OriginKind: item.OriginKind, OriginHostKind: item.OriginHostKind, OriginHostID: item.OriginHostID, OriginRef: item.OriginRef, HostRevision: item.HostRevision, CompletedAt: item.CompletedAt, Expired: item.Expired, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, SourceHref: readerTodoSourceHref(item)}
}

func readerTodoSourceHref(item model.ReaderTodo) *string {
	if item.OriginKind == "standalone" {
		return nil
	}
	var ref map[string]any
	_ = json.Unmarshal(item.OriginRef, &ref)
	stringRef := func(key string) string {
		value, _ := ref[key].(string)
		return strings.TrimSpace(value)
	}
	kind, id := stringRef("source_kind"), stringRef("source_id")
	if kind == "" && item.OriginHostKind != nil {
		kind = strings.TrimSpace(*item.OriginHostKind)
	}
	if id == "" && item.OriginHostID != nil {
		id = strings.TrimSpace(*item.OriginHostID)
	}
	var href string
	switch kind {
	case "link":
		href = "/?view=reading&link_id=" + url.QueryEscape(id)
	case "note":
		href = "/?view=notes&note_id=" + url.QueryEscape(id)
	case "inbox":
		href = "/?view=pending&inbox_id=" + url.QueryEscape(id)
	case "thought":
		href = "/?tool=history&thought_view=live"
	}
	if href == "" || (kind != "thought" && id == "") {
		return nil
	}
	return &href
}

func (s *ReaderVNextService) PatchTodo(ctx context.Context, rawID string, request dto.ReaderTodoPatchRequest) (dto.ReaderTodoResponse, error) {
	id, err := readerUUID(rawID, "todo_id")
	if err != nil {
		return dto.ReaderTodoResponse{}, err
	}
	item, err := s.todos.PatchTodo(ctx, model.ReaderTodoPatch{
		ID:                      id,
		Text:                    request.Text,
		DueAt:                   request.DueAt,
		DueAtSet:                request.DueAtSet || request.DueAt != nil,
		Done:                    request.Done,
		ExpectedHostRevision:    request.ExpectedHostRevision,
		ExpectedHostRevisionSet: request.ExpectedHostRevisionSet || request.ExpectedHostRevision != nil,
	})
	if err != nil {
		return dto.ReaderTodoResponse{}, mapReaderError(err)
	}
	return todoResponse(*item), nil
}

func (s *ReaderVNextService) DeleteTodo(ctx context.Context, rawID string) error {
	id, err := readerUUID(rawID, "todo_id")
	if err != nil {
		return err
	}
	return mapReaderError(s.todos.DeleteTodo(ctx, id))
}
