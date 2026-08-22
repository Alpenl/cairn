package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/notetitle"
	"webtag/internal/problem"
)

type ReaderNoteCreateCommand struct {
	Title   string
	Content string
}

type ReaderNotePage struct {
	Items      []model.ReaderNote
	Count      int
	NextCursor string
}

func deriveNoteTitle(content string) string {
	return notetitle.Derive(content)
}

func (s *ReaderNoteApplication) CreateNote(ctx context.Context, command ReaderNoteCreateCommand) (model.ReaderNote, error) {
	title := strings.TrimSpace(command.Title)
	if title == "" {
		title = deriveNoteTitle(command.Content)
	}
	note := model.ReaderNote{Title: title, PublishedContent: ""}
	if command.Content != "" {
		note.DraftContent = &command.Content
	}
	created, err := s.notes.CreateNote(ctx, note)
	if err != nil {
		return model.ReaderNote{}, mapReaderError(err)
	}
	return *created, nil
}

func (s *ReaderNoteApplication) ListNotes(ctx context.Context, after string, limit int) (ReaderNotePage, error) {
	items, count, next, err := s.notes.ListNotes(ctx, after, limit)
	if err != nil {
		return ReaderNotePage{}, mapReaderError(err)
	}
	return ReaderNotePage{Items: items, Count: count, NextCursor: next}, nil
}

func (s *ReaderNoteApplication) GetNote(ctx context.Context, id uuid.UUID) (model.ReaderNote, error) {
	note, err := s.notes.GetNote(ctx, id)
	if err != nil {
		return model.ReaderNote{}, mapReaderError(err)
	}
	return *note, nil
}

func (s *ReaderNoteApplication) SaveNoteDraft(ctx context.Context, command model.ReaderNoteDraftCommand) (model.ReaderNote, error) {
	note, err := s.notes.SaveNoteDraft(ctx, command)
	if err != nil {
		return model.ReaderNote{}, mapReaderError(err)
	}
	return *note, nil
}

func (s *ReaderNoteApplication) DiscardNoteDraft(ctx context.Context, command model.ReaderNoteDiscardDraftCommand) error {
	if command.ExpectedDraftRevision < 1 {
		return problem.NewWithCode(problem.Invalid, "invalid_draft_revision", "draft revision must be positive")
	}
	return mapReaderError(s.notes.DiscardNoteDraft(ctx, command))
}

func (s *ReaderNoteApplication) PublishNote(ctx context.Context, command model.ReaderNotePublishCommand) (model.ReaderNote, error) {
	if err := validateReaderReanchorOps(command.ReanchorOps); err != nil {
		return model.ReaderNote{}, err
	}
	note, err := s.notes.PublishNote(ctx, command)
	if err != nil {
		return model.ReaderNote{}, mapReaderError(err)
	}
	return *note, nil
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

func (s *ReaderNoteApplication) DeleteNote(ctx context.Context, id uuid.UUID) (model.ReaderHostLifecycleResult, error) {
	result, err := s.hosts.SoftDeleteHost(ctx, model.ReaderHostNote, id)
	if err != nil {
		return model.ReaderHostLifecycleResult{}, mapReaderError(err)
	}
	return result, nil
}

func (s *ReaderNoteApplication) RestoreNote(ctx context.Context, id uuid.UUID) (model.ReaderHostLifecycleResult, error) {
	if s.hostRestores == nil {
		return model.ReaderHostLifecycleResult{}, errors.New("reader host restore commands are not configured")
	}
	result, err := s.hostRestores.RestoreHost(ctx, model.ReaderHostNote, id)
	if err != nil {
		return model.ReaderHostLifecycleResult{}, mapReaderError(err)
	}
	return result, nil
}

func (s *ReaderNoteApplication) ListNoteHistory(ctx context.Context, id uuid.UUID, limit int) ([]model.ReaderNoteHistory, error) {
	items, err := s.notes.ListNoteHistory(ctx, id, limit)
	if err != nil {
		return nil, mapReaderError(err)
	}
	return items, nil
}

func (s *ReaderNoteApplication) RestoreNoteRevision(ctx context.Context, command model.ReaderNoteRestoreCommand) (model.ReaderNote, error) {
	if err := validateReaderReanchorOps(command.ReanchorOps); err != nil {
		return model.ReaderNote{}, err
	}
	note, err := s.notes.RestoreNoteRevision(ctx, command)
	if err != nil {
		return model.ReaderNote{}, mapReaderError(err)
	}
	return *note, nil
}
