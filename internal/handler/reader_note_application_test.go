package handler

import (
	"context"
	"testing"

	"webtag/internal/dto"
	"webtag/internal/model"
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

func TestReaderNoteRoutesRejectInvalidIDBeforeApplicationStore(t *testing.T) {
	t.Parallel()

	store := &readerNoteDraftApplicationStore{}
	applications := service.NewReaderApplications(readerServiceTestStores(store), nil)
	routes := NewReaderNoteRoutes(applications.Notes)

	_, err := routes.SaveNoteDraft(context.Background(), "not-a-uuid", dto.ReaderNoteDraftRequest{Content: "draft"})
	if err == nil {
		t.Fatal("SaveNoteDraft() error = nil for invalid id")
	}
	if len(store.commands) != 0 {
		t.Fatalf("SaveNoteDraft() store calls = %d, want 0", len(store.commands))
	}
}
