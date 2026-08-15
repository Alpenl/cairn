package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestSaveNoteDraftClassifiesMissingAndStaleInOneStatement(t *testing.T) {
	now := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	noteID := uuid.New()
	columns := []string{"outcome", "id", "title", "published_content", "published_revision", "draft_content", "draft_revision", "draft_updated_at", "deleted_at", "created_at", "updated_at"}

	tests := []struct {
		name    string
		outcome string
		row     []any
		wantErr error
	}{
		{name: "missing", outcome: "missing", row: []any{"missing", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil}, wantErr: ErrNotFound},
		{name: "stale", outcome: "stale", row: []any{"stale", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil}, wantErr: ErrRevisionConflict},
		{name: "matching revision", outcome: "updated", row: []any{"updated", noteID.String(), "Draft", "Published", int64(1), "new draft", int64(2), now, nil, now, now}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock.NewPool() error = %v", err)
			}
			defer mock.Close()

			// One query is intentional: no zero-row UPDATE may be followed by a
			// separately observed existence probe.
			mock.ExpectQuery("(?s)WITH updated AS.*live AS.*WHERE NOT EXISTS \\(SELECT 1 FROM updated\\)").
				WithArgs("new draft", noteID, int64(1)).
				WillReturnRows(mock.NewRows(columns).AddRow(test.row...))

			repo := NewPGXReaderVNextRepository(mock)
			note, err := repo.SaveNoteDraft(context.Background(), model.ReaderNoteDraftCommand{
				NoteID: noteID, Content: "new draft", ExpectedDraftRevision: 1,
			})
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("SaveNoteDraft() error = %v, want %v", err, test.wantErr)
				}
				if note != nil {
					t.Fatalf("SaveNoteDraft() note = %#v, want nil on error", note)
				}
			} else if err != nil || note == nil || note.DraftRevision != 2 || note.DraftContent == nil || *note.DraftContent != "new draft" {
				t.Fatalf("SaveNoteDraft() = %#v, %v; want updated revision 2", note, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("pgxmock expectations: %v", err)
			}
		})
	}
}
