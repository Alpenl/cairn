package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type noteDraftStoreStub struct {
	ReaderNoteStore
	err      error
	note     *model.ReaderNote
	commands []model.ReaderNoteDraftCommand
}

func (s *noteDraftStoreStub) SaveNoteDraft(_ context.Context, command model.ReaderNoteDraftCommand) (*model.ReaderNote, error) {
	s.commands = append(s.commands, command)
	return s.note, s.err
}

func TestSaveNoteDraftMapsMissingBeforeRevisionConflict(t *testing.T) {
	noteID := uuid.New()
	for _, test := range []struct {
		name       string
		storeErr   error
		wantStatus int
		wantCode   string
	}{
		{name: "missing", storeErr: repository.ErrNotFound, wantStatus: http.StatusNotFound, wantCode: "reader_not_found"},
		{name: "stale", storeErr: repository.ErrRevisionConflict, wantStatus: http.StatusConflict, wantCode: "revision_conflict"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &noteDraftStoreStub{err: test.storeErr}
			_, err := newReaderTestFeatureSet(readerTestStores(store), nil).SaveNoteDraft(context.Background(), model.ReaderNoteDraftCommand{NoteID: noteID, Content: "draft", ExpectedDraftRevision: 1})
			carrier, ok := httperr.As(err)
			coder, coded := carrier.(httperr.ErrorCoder)
			if !ok || !coded || carrier.HTTPStatus() != test.wantStatus || coder.HTTPErrorCode() != test.wantCode {
				t.Fatalf("SaveNoteDraft() error = %v, want %d/%s", err, test.wantStatus, test.wantCode)
			}
		})
	}
}
