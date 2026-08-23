package handler

import (
	"context"
	"encoding/json"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/service"
)

type readerNoteApplicationRoutes struct {
	application *service.ReaderNoteApplication
}

func NewReaderNoteRoutes(application *service.ReaderNoteApplication) ReaderNoteRoutes {
	if application == nil {
		return nil
	}
	return &readerNoteApplicationRoutes{application: application}
}

func (r *readerNoteApplicationRoutes) CreateNote(ctx context.Context, request dto.ReaderNoteCreateRequest) (dto.ReaderNoteResponse, error) {
	note, err := r.application.CreateNote(ctx, service.ReaderNoteCreateCommand{Title: request.Title, Content: request.Content})
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	return readerNoteResponse(note), nil
}

func (r *readerNoteApplicationRoutes) ListNotes(ctx context.Context, after string, limit int) (dto.ReaderNotesResponse, error) {
	page, err := r.application.ListNotes(ctx, after, limit)
	if err != nil {
		return dto.ReaderNotesResponse{}, err
	}
	response := dto.ReaderNotesResponse{Items: make([]dto.ReaderNoteResponse, 0, len(page.Items)), Count: page.Count, NextCursor: page.NextCursor}
	for _, item := range page.Items {
		response.Items = append(response.Items, readerNoteResponse(item))
	}
	return response, nil
}

func (r *readerNoteApplicationRoutes) GetNote(ctx context.Context, rawID string) (dto.ReaderNoteResponse, error) {
	id, err := parseReaderUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	note, err := r.application.GetNote(ctx, id)
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	return readerNoteResponse(note), nil
}

func (r *readerNoteApplicationRoutes) SaveNoteDraft(ctx context.Context, rawID string, request dto.ReaderNoteDraftRequest) (dto.ReaderNoteResponse, error) {
	id, err := parseReaderUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	note, err := r.application.SaveNoteDraft(ctx, model.ReaderNoteDraftCommand{
		NoteID: id, Content: request.Content, ExpectedDraftRevision: request.ExpectedDraftRevision,
	})
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	return readerNoteResponse(note), nil
}

func (r *readerNoteApplicationRoutes) DiscardNoteDraft(ctx context.Context, rawID string, expectedRevision int64) error {
	id, err := parseReaderUUID(rawID, "note_id")
	if err != nil {
		return err
	}
	return r.application.DiscardNoteDraft(ctx, model.ReaderNoteDiscardDraftCommand{NoteID: id, ExpectedDraftRevision: expectedRevision})
}

func (r *readerNoteApplicationRoutes) PublishNote(ctx context.Context, rawID string, request dto.ReaderNotePublishRequest) (dto.ReaderNoteResponse, error) {
	id, err := parseReaderUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	if request.ExpectedDraftRevision == nil || request.ExpectedPublishedRevision == nil {
		return dto.ReaderNoteResponse{}, problem.NewWithCode(problem.Invalid, "note_revision_required", "draft and published revisions are required")
	}
	if err := validateReaderReanchorOps(request.ReanchorOps); err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	note, err := r.application.PublishNote(ctx, model.ReaderNotePublishCommand{
		NoteID: id, ExpectedDraftRevision: *request.ExpectedDraftRevision,
		ExpectedPublishedRevision: *request.ExpectedPublishedRevision, ReanchorOps: request.ReanchorOps,
	})
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	return readerNoteResponse(note), nil
}

func (r *readerNoteApplicationRoutes) DeleteNote(ctx context.Context, rawID string) (dto.ReaderHostLifecycleResponse, error) {
	id, err := parseReaderUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	result, err := r.application.DeleteNote(ctx, id)
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	return readerHostLifecycleResponse(result), nil
}

func (r *readerNoteApplicationRoutes) RestoreNote(ctx context.Context, rawID string) (dto.ReaderHostLifecycleResponse, error) {
	id, err := parseReaderUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	result, err := r.application.RestoreNote(ctx, id)
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	return readerHostLifecycleResponse(result), nil
}

func (r *readerNoteApplicationRoutes) ListNoteHistory(ctx context.Context, rawID string, limit int) ([]dto.ReaderNoteHistoryResponse, error) {
	id, err := parseReaderUUID(rawID, "note_id")
	if err != nil {
		return nil, err
	}
	items, err := r.application.ListNoteHistory(ctx, id, limit)
	if err != nil {
		return nil, err
	}
	response := make([]dto.ReaderNoteHistoryResponse, 0, len(items))
	for _, item := range items {
		response = append(response, dto.ReaderNoteHistoryResponse{
			ID: item.ID, Revision: item.Revision, Title: item.Title, Content: item.Content,
			ReanchorOps: item.ReanchorOps, CreatedAt: item.CreatedAt,
		})
	}
	return response, nil
}

func (r *readerNoteApplicationRoutes) RestoreNoteRevision(ctx context.Context, rawID string, revision int64, request dto.ReaderNoteRestoreRequest) (dto.ReaderNoteResponse, error) {
	id, err := parseReaderUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	if request.ExpectedDraftRevision == nil || request.ExpectedPublishedRevision == nil {
		return dto.ReaderNoteResponse{}, problem.NewWithCode(problem.Invalid, "note_revision_required", "draft and published revisions are required")
	}
	if err := validateReaderReanchorOps(request.ReanchorOps); err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	note, err := r.application.RestoreNoteRevision(ctx, model.ReaderNoteRestoreCommand{
		NoteID: id, Revision: revision, ExpectedDraftRevision: *request.ExpectedDraftRevision,
		ExpectedPublishedRevision: *request.ExpectedPublishedRevision, ReanchorOps: request.ReanchorOps,
	})
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	return readerNoteResponse(note), nil
}

func validateReaderReanchorOps(ops []json.RawMessage) error {
	if len(ops) > 500 {
		return problem.NewWithCode(problem.Invalid, "invalid_reanchor_ops", "too many reanchor operations")
	}
	for _, raw := range ops {
		if len(raw) == 0 || len(raw) > 128*1024 || !json.Valid(raw) {
			return problem.NewWithCode(problem.Invalid, "invalid_reanchor_ops", "reanchor operation must be valid bounded JSON")
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return problem.NewWithCode(problem.Invalid, "invalid_reanchor_ops", "reanchor operation must be a JSON object")
		}
	}
	return nil
}

func readerNoteResponse(note model.ReaderNote) dto.ReaderNoteResponse {
	dirty := note.DraftContent != nil && *note.DraftContent != note.PublishedContent
	return dto.ReaderNoteResponse{
		ID: note.ID.String(), Title: note.Title, PublishedContent: note.PublishedContent,
		PublishedRevision: note.PublishedRevision, DraftContent: note.DraftContent,
		DraftRevision: note.DraftRevision, DraftUpdatedAt: note.DraftUpdatedAt,
		DeletedAt: note.DeletedAt, CreatedAt: note.CreatedAt, UpdatedAt: note.UpdatedAt, Dirty: dirty,
	}
}

var _ ReaderNoteRoutes = (*readerNoteApplicationRoutes)(nil)
