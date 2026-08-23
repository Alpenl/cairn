package handler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/service"
)

type readerNoteDraftApplicationStore struct {
	service.ReaderNoteStore
	commands []model.ReaderNoteDraftCommand
}

func (s *readerNoteDraftApplicationStore) SaveNoteDraft(_ context.Context, command model.ReaderNoteDraftCommand) (*model.ReaderNote, error) {
	s.commands = append(s.commands, command)
	return &model.ReaderNote{}, nil
}

type readerNotePublishApplicationStore struct {
	service.ReaderNoteStore
	commands []model.ReaderNotePublishCommand
}

func (s *readerNotePublishApplicationStore) PublishNote(_ context.Context, command model.ReaderNotePublishCommand) (*model.ReaderNote, error) {
	s.commands = append(s.commands, command)
	return &model.ReaderNote{ID: command.NoteID}, nil
}

func TestReaderNoteRoutesRejectInvalidIDBeforeApplicationStore(t *testing.T) {
	t.Parallel()

	store := &readerNoteDraftApplicationStore{}
	applications := readerTestApplications(store)
	routes := NewReaderNoteRoutes(applications.Notes)

	_, err := routes.SaveNoteDraft(context.Background(), "not-a-uuid", dto.ReaderNoteDraftRequest{Content: "draft"})
	if err == nil {
		t.Fatal("SaveNoteDraft() error = nil for invalid id")
	}
	if len(store.commands) != 0 {
		t.Fatalf("SaveNoteDraft() store calls = %d, want 0", len(store.commands))
	}
}

func TestReaderNoteRoutesRejectInvalidReanchorOpsBeforeApplicationStore(t *testing.T) {
	t.Parallel()

	draftRevision, publishedRevision := int64(2), int64(1)
	store := &readerNotePublishApplicationStore{}
	applications := readerTestApplications(store)
	routes := NewReaderNoteRoutes(applications.Notes)

	_, err := routes.PublishNote(context.Background(), "11111111-1111-1111-1111-111111111111", dto.ReaderNotePublishRequest{
		ExpectedDraftRevision:     &draftRevision,
		ExpectedPublishedRevision: &publishedRevision,
		ReanchorOps:               []json.RawMessage{json.RawMessage(`[]`)},
	})
	var status *problem.Error
	if !errors.As(err, &status) || status.Code() != "invalid_reanchor_ops" {
		t.Fatalf("PublishNote() error = %v, want invalid_reanchor_ops", err)
	}
	if len(store.commands) != 0 {
		t.Fatalf("PublishNote() store calls = %d, want 0", len(store.commands))
	}
}
