package service

import (
	"context"
	"encoding/json"
	"strings"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/notetitle"
	"webtag/internal/problem"
)

func deriveNoteTitle(content string) string {
	return notetitle.Derive(content)
}

func (s *ReaderVNextService) CreateNote(ctx context.Context, request dto.ReaderNoteCreateRequest) (dto.ReaderNoteResponse, error) {
	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = deriveNoteTitle(request.Content)
	}
	note := model.ReaderNote{Title: title, PublishedContent: ""}
	if request.Content != "" {
		note.DraftContent = &request.Content
	}
	created, err := s.notes.CreateNote(ctx, note)
	if err != nil {
		return dto.ReaderNoteResponse{}, mapReaderError(err)
	}
	return noteResponse(*created), nil
}

func (s *ReaderVNextService) ListNotes(ctx context.Context, after string, limit int) (dto.ReaderNotesResponse, error) {
	items, count, next, err := s.notes.ListNotes(ctx, after, limit)
	if err != nil {
		return dto.ReaderNotesResponse{}, mapReaderError(err)
	}
	out := dto.ReaderNotesResponse{Items: make([]dto.ReaderNoteResponse, 0, len(items)), Count: count, NextCursor: next}
	for _, item := range items {
		out.Items = append(out.Items, noteResponse(item))
	}
	return out, nil
}

func (s *ReaderVNextService) GetNote(ctx context.Context, rawID string) (dto.ReaderNoteResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	note, err := s.notes.GetNote(ctx, id)
	if err != nil {
		return dto.ReaderNoteResponse{}, mapReaderError(err)
	}
	return noteResponse(*note), nil
}

func noteResponse(note model.ReaderNote) dto.ReaderNoteResponse {
	dirty := note.DraftContent != nil && *note.DraftContent != note.PublishedContent
	return dto.ReaderNoteResponse{ID: note.ID.String(), Title: note.Title, PublishedContent: note.PublishedContent, PublishedRevision: note.PublishedRevision, DraftContent: note.DraftContent, DraftRevision: note.DraftRevision, DraftUpdatedAt: note.DraftUpdatedAt, DeletedAt: note.DeletedAt, CreatedAt: note.CreatedAt, UpdatedAt: note.UpdatedAt, Dirty: dirty}
}

func (s *ReaderVNextService) SaveNoteDraft(ctx context.Context, rawID string, request dto.ReaderNoteDraftRequest) (dto.ReaderNoteResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	note, err := s.notes.SaveNoteDraft(ctx, model.ReaderNoteDraftCommand{NoteID: id, Content: request.Content, ExpectedDraftRevision: request.ExpectedDraftRevision})
	if err != nil {
		return dto.ReaderNoteResponse{}, mapReaderError(err)
	}
	return noteResponse(*note), nil
}

func (s *ReaderVNextService) DiscardNoteDraft(ctx context.Context, rawID string, expectedRevision int64) error {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return err
	}
	if expectedRevision < 1 {
		return problem.NewWithCode(problem.Invalid, "invalid_draft_revision", "draft revision must be positive")
	}
	return mapReaderError(s.notes.DiscardNoteDraft(ctx, model.ReaderNoteDiscardDraftCommand{NoteID: id, ExpectedDraftRevision: expectedRevision}))
}

func (s *ReaderVNextService) PublishNote(ctx context.Context, rawID string, request dto.ReaderNotePublishRequest) (dto.ReaderNoteResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	if request.ExpectedDraftRevision == nil || request.ExpectedPublishedRevision == nil {
		return dto.ReaderNoteResponse{}, problem.NewWithCode(problem.Invalid, "note_revision_required", "draft and published revisions are required")
	}
	if err := validateReaderReanchorOps(request.ReanchorOps); err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	note, err := s.notes.PublishNote(ctx, model.ReaderNotePublishCommand{NoteID: id, ExpectedDraftRevision: *request.ExpectedDraftRevision, ExpectedPublishedRevision: *request.ExpectedPublishedRevision, ReanchorOps: request.ReanchorOps})
	if err != nil {
		return dto.ReaderNoteResponse{}, mapReaderError(err)
	}
	return noteResponse(*note), nil
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

func (s *ReaderVNextService) DeleteNote(ctx context.Context, rawID string) (dto.ReaderHostLifecycleResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	result, err := s.hosts.SoftDeleteHost(ctx, model.ReaderHostNote, id)
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, mapReaderError(err)
	}
	return readerHostLifecycleResponse(result), nil
}

func (s *ReaderVNextService) RestoreNote(ctx context.Context, rawID string) (dto.ReaderHostLifecycleResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	result, err := s.hosts.RestoreHost(ctx, model.ReaderHostNote, id)
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, mapReaderError(err)
	}
	return readerHostLifecycleResponse(result), nil
}

func (s *ReaderVNextService) ListNoteHistory(ctx context.Context, rawID string, limit int) ([]dto.ReaderNoteHistoryResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return nil, err
	}
	items, err := s.notes.ListNoteHistory(ctx, id, limit)
	if err != nil {
		return nil, mapReaderError(err)
	}
	out := make([]dto.ReaderNoteHistoryResponse, 0, len(items))
	for _, item := range items {
		out = append(out, dto.ReaderNoteHistoryResponse{ID: item.ID, Revision: item.Revision, Title: item.Title, Content: item.Content, ReanchorOps: item.ReanchorOps, CreatedAt: item.CreatedAt})
	}
	return out, nil
}

func (s *ReaderVNextService) RestoreNoteRevision(ctx context.Context, rawID string, revision int64, request dto.ReaderNoteRestoreRequest) (dto.ReaderNoteResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	if request.ExpectedDraftRevision == nil || request.ExpectedPublishedRevision == nil {
		return dto.ReaderNoteResponse{}, problem.NewWithCode(problem.Invalid, "note_revision_required", "draft and published revisions are required")
	}
	if err := validateReaderReanchorOps(request.ReanchorOps); err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	note, err := s.notes.RestoreNoteRevision(ctx, model.ReaderNoteRestoreCommand{NoteID: id, Revision: revision, ExpectedDraftRevision: *request.ExpectedDraftRevision, ExpectedPublishedRevision: *request.ExpectedPublishedRevision, ReanchorOps: request.ReanchorOps})
	if err != nil {
		return dto.ReaderNoteResponse{}, mapReaderError(err)
	}
	return noteResponse(*note), nil
}
