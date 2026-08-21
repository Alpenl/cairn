package repository

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
	"webtag/internal/readertext"
)

func TestRestoreNoteScansOnlyMatchingLifecycleTombstones(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	noteID := uuid.New()
	deletedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT deleted_at,published_content,published_revision FROM reader_notes WHERE id=$1 FOR UPDATE")).
		WithArgs(noteID).
		WillReturnRows(mock.NewRows([]string{"deleted_at", "published_content", "published_revision"}).AddRow(deletedAt, "published", int64(3)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE reader_notes SET deleted_at=NULL,updated_at=NOW() WHERE id=$1")).
		WithArgs(noteID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery("(?s)SELECT .*reader_thoughts.*tt.created_at.*FROM reader_thought_tombstones tt.*tt.host_kind=\\$1.*tt.host_id=\\$2.*tt.reason=\\$3.*reader_thoughts.deleted=false").
		WithArgs(model.ReaderHostNote, noteID.String(), "note_deleted").
		WillReturnRows(mock.NewRows(append(readerThoughtSyncColumnsForTest(), "created_at")))
	expectNoteTodoProjectionRefresh(mock, noteID)
	mock.ExpectCommit()

	repo := NewPGXReaderVNextRepository(mock)
	if _, err := repo.RestoreHost(context.Background(), model.ReaderHostNote, noteID); err != nil {
		t.Fatalf("RestoreHost(note) error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPatchEngagementReadFalseDoesNotMoveLastOpened(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	linkID := uuid.New()
	updatedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	read := false
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM links WHERE id=$1 AND deleted_at IS NULL)")).
		WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery("(?s)INSERT INTO reader_engagement.*last_opened=CASE WHEN \\$2::boolean IS TRUE OR \\$3::real IS NOT NULL THEN NOW\\(\\) ELSE reader_engagement.last_opened END").
		WithArgs(linkID, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(mock.NewRows([]string{"link_id", "read", "progress", "read_later", "last_opened", "updated_at"}).
			AddRow(linkID, false, float32(0.5), false, pgtype.Timestamptz{Time: updatedAt, Valid: true}, updatedAt))

	repo := NewPGXReaderVNextRepository(mock)
	item, err := repo.PatchEngagement(context.Background(), model.ReaderEngagementPatch{LinkID: linkID, Read: &read})
	if err != nil {
		t.Fatalf("PatchEngagement() error = %v", err)
	}
	if item.LastOpened == nil || !item.LastOpened.Equal(updatedAt) {
		t.Fatalf("LastOpened = %v, want existing timestamp", item.LastOpened)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPatchTodoNoteRejectsDirtyDraftBeforePublishedWrite(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	todoID := uuid.New()
	noteID := uuid.New()
	blocks := readertext.List("- [ ] stable")
	if len(blocks) != 1 {
		t.Fatalf("readertext.List() returned %d blocks, want 1", len(blocks))
	}
	originRef, err := json.Marshal(map[string]any{"block_ref": blocks[0].BlockRef, "occurrence": 1})
	if err != nil {
		t.Fatal(err)
	}
	draft := "- [ ] changed in draft"
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT origin_kind,host_revision FROM reader_todos WHERE id=$1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(todoID).
		WillReturnRows(mock.NewRows([]string{"origin_kind", "host_revision"}).AddRow("note", int64(7)))
	mock.ExpectQuery("(?s)SELECT origin_kind,origin_host_kind,origin_host_id,origin_ref.*FROM reader_todos.*FOR UPDATE").
		WithArgs(todoID).
		WillReturnRows(mock.NewRows([]string{"origin_kind", "origin_host_kind", "origin_host_id", "origin_ref"}).
			AddRow("note", "note", noteID.String(), originRef))
	mock.ExpectQuery("(?s)SELECT title,published_content,published_revision,draft_content.*FROM reader_notes.*FOR UPDATE").
		WithArgs(noteID).
		WillReturnRows(mock.NewRows([]string{"title", "published_content", "published_revision", "draft_content"}).
			AddRow("Note", "- [ ] stable", int64(7), pgtype.Text{String: draft, Valid: true}))
	mock.ExpectRollback()

	repo := NewPGXReaderVNextRepository(mock)
	expectedRevision := int64(7)
	_, err = repo.PatchTodo(context.Background(), model.ReaderTodoPatch{ID: todoID, Done: boolPointer(false), ExpectedHostRevision: &expectedRevision})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("PatchTodo() error = %v, want ErrRevisionConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func boolPointer(value bool) *bool { return &value }
