package handler

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/service"
)

type readerTodoApplicationRoutes struct {
	application *service.ReaderTodoApplication
}

func NewReaderTodoRoutes(application *service.ReaderTodoApplication) ReaderTodoRoutes {
	if application == nil {
		return nil
	}
	return &readerTodoApplicationRoutes{application: application}
}

func (r *readerTodoApplicationRoutes) CreateTodo(ctx context.Context, request dto.ReaderTodoCreateRequest) (dto.ReaderTodoResponse, error) {
	item, err := r.application.CreateTodo(ctx, service.ReaderTodoCreateCommand{Text: request.Text, DueAt: request.DueAt})
	if err != nil {
		return dto.ReaderTodoResponse{}, err
	}
	return readerTodoResponse(item), nil
}

func (r *readerTodoApplicationRoutes) ListTodos(ctx context.Context, after string, limit int) (dto.ReaderTodosResponse, error) {
	page, err := r.application.ListTodos(ctx, after, limit)
	if err != nil {
		return dto.ReaderTodosResponse{}, err
	}
	response := dto.ReaderTodosResponse{Items: make([]dto.ReaderTodoResponse, 0, len(page.Items)), NextAfter: page.Next}
	for _, item := range page.Items {
		response.Items = append(response.Items, readerTodoResponse(item))
	}
	return response, nil
}

func (r *readerTodoApplicationRoutes) PatchTodo(ctx context.Context, rawID string, request dto.ReaderTodoPatchRequest) (dto.ReaderTodoResponse, error) {
	id, err := parseReaderUUID(rawID, "todo_id")
	if err != nil {
		return dto.ReaderTodoResponse{}, err
	}
	item, err := r.application.PatchTodo(ctx, service.ReaderTodoPatchCommand{
		ID:                      id,
		Text:                    request.Text,
		DueAt:                   request.DueAt,
		DueAtSet:                request.DueAtSet || request.DueAt != nil,
		Done:                    request.Done,
		ExpectedHostRevision:    request.ExpectedHostRevision,
		ExpectedHostRevisionSet: request.ExpectedHostRevisionSet || request.ExpectedHostRevision != nil,
	})
	if err != nil {
		return dto.ReaderTodoResponse{}, err
	}
	return readerTodoResponse(item), nil
}

func (r *readerTodoApplicationRoutes) DeleteTodo(ctx context.Context, rawID string) error {
	id, err := parseReaderUUID(rawID, "todo_id")
	if err != nil {
		return err
	}
	return r.application.DeleteTodo(ctx, id)
}

func parseReaderUUID(raw, field string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, problem.NewWithCode(problem.Invalid, "invalid_"+field, field+" must be a UUID")
	}
	return id, nil
}

func readerTodoResponse(item model.ReaderTodo) dto.ReaderTodoResponse {
	return dto.ReaderTodoResponse{
		ID: item.ID.String(), Text: item.Text, DueAt: item.DueAt, Done: item.Done,
		OriginKind: item.OriginKind, OriginHostKind: item.OriginHostKind, OriginHostID: item.OriginHostID,
		OriginRef: item.OriginRef, HostRevision: item.HostRevision, CompletedAt: item.CompletedAt,
		Expired: item.Expired, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		SourceHref: readerTodoSourceHref(item),
	}
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

var _ ReaderTodoRoutes = (*readerTodoApplicationRoutes)(nil)
