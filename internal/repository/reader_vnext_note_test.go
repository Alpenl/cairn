package repository

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func readerNoteColumnsForTest() []string {
	return []string{
		"id", "title", "published_content", "published_revision", "draft_content",
		"draft_revision", "draft_updated_at", "deleted_at", "created_at", "updated_at",
	}
}

func readerNoteRowForTest(
	id uuid.UUID,
	title string,
	publishedContent string,
	publishedRevision int64,
	draftContent any,
	draftRevision int64,
	at time.Time,
) []any {
	var nullableDraft any
	switch value := draftContent.(type) {
	case string:
		draft := value
		nullableDraft = &draft
	default:
		nullableDraft = value
	}
	return []any{
		id,
		title,
		publishedContent,
		publishedRevision,
		nullableDraft,
		draftRevision,
		nil,
		nil,
		at,
		at,
	}
}

func readerThoughtRowForNoteTest(
	id string,
	noteID uuid.UUID,
	target json.RawMessage,
	body string,
	sequence int64,
	at time.Time,
) []any {
	return []any{
		id,
		"note",
		noteID.String(),
		nil,
		[]byte(target),
		[]byte(`{"exact":"old quote","start":0,"end":9}`),
		body,
		"user",
		false,
		sequence,
		sequence,
		"device-test",
		"op-" + id,
		at,
		at,
	}
}

func readerNoteReanchorOpForTest(
	thoughtID string,
	noteID uuid.UUID,
	status string,
	reason string,
	previousRevision int64,
	nextRevision int64,
	exact string,
) json.RawMessage {
	var quote map[string]any
	if status == "reanchored" {
		quote = map[string]any{"exact": exact, "prefix": "", "suffix": ""}
	}
	var target any
	if status == "reanchored" {
		target = map[string]any{
			"kind":    "note",
			"host_id": noteID.String(),
			"version": map[string]any{
				"note_revision": nextRevision,
			},
		}
	}
	op := map[string]any{
		"thought_id": thoughtID,
		"status":     status,
		"reason":     reason,
		"target":     target,
		"quote":      quote,
	}
	if status == "reanchored" {
		op["range"] = map[string]any{"start": 0, "end": len(exact)}
		// Keep the previous revision in the target payload used by the thought
		// row; this value is intentionally not sent in the command itself.
		_ = previousRevision
	}
	encoded, err := json.Marshal(op)
	if err != nil {
		panic(err)
	}
	return encoded
}

func expectPublishedNoteLock(mock pgxmock.PgxPoolIface, noteID uuid.UUID, row []any) {
	mock.ExpectQuery("(?s)SELECT " + regexp.QuoteMeta(readerNoteColumns) + " FROM reader_notes.*FOR UPDATE").
		WithArgs(noteID).
		WillReturnRows(mock.NewRows(readerNoteColumnsForTest()).AddRow(row...))
}

func expectPublishedNoteUpdate(mock pgxmock.PgxPoolIface, noteID uuid.UUID, content string, revision int64) {
	mock.ExpectExec("UPDATE reader_notes SET title=\\$1,published_content=\\$2, published_revision=\\$3").
		WithArgs(pgxmock.AnyArg(), content, revision, noteID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
}

func expectPublishedNoteHistory(mock pgxmock.PgxPoolIface, noteID uuid.UUID, content string, revision int64) {
	mock.ExpectExec("(?s)INSERT INTO reader_note_history.*reanchor_ops").
		WithArgs(noteID, revision, pgxmock.AnyArg(), content, pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

func expectNoteReanchorSet(mock pgxmock.PgxPoolIface, noteID uuid.UUID, thoughtIDs ...string) {
	rows := mock.NewRows([]string{"id"})
	for _, thoughtID := range thoughtIDs {
		rows.AddRow(thoughtID)
	}
	mock.ExpectQuery("(?s)SELECT reader_thoughts.id.*FROM reader_thoughts.*host_kind='note'.*host_id=\\$1.*reader_thought_tombstones.*FOR UPDATE").
		WithArgs(noteID.String()).
		WillReturnRows(rows)
}

func expectNoteThoughtLock(mock pgxmock.PgxPoolIface, thoughtID string, row []any) {
	mock.ExpectQuery("(?s)SELECT " + regexp.QuoteMeta(readerThoughtColumns) + " FROM reader_thoughts.*deleted=false FOR UPDATE").
		WithArgs(thoughtID).
		WillReturnRows(mock.NewRows(readerThoughtSyncColumnsForTest()).AddRow(row...))
}

func expectNoteTombstoneInsert(mock pgxmock.PgxPoolIface, thoughtID string, reason string, sequence int64) {
	mock.ExpectExec("(?s)INSERT INTO reader_thought_tombstones.*FROM reader_thoughts.*id=\\$1").
		WithArgs(thoughtID, reason, sequence).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

func expectPublishedNoteResult(mock pgxmock.PgxPoolIface, noteID uuid.UUID, row []any) {
	mock.ExpectQuery("SELECT " + regexp.QuoteMeta(readerNoteColumns) + " FROM reader_notes WHERE id=\\$1").
		WithArgs(noteID).
		WillReturnRows(mock.NewRows(readerNoteColumnsForTest()).AddRow(row...))
}

func TestPublishNoteReanchorsThoughtAndClearsTombstoneAtomically(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	noteID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	thoughtID := "thought-note-1"
	at := time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC)
	oldTarget := json.RawMessage(`{"kind":"note","host_id":"11111111-1111-1111-1111-111111111111","version":{"note_revision":1}}`)
	newContent := "new quote in the published note"

	mock.ExpectBegin()
	expectPublishedNoteLock(mock, noteID, readerNoteRowForTest(noteID, "Reader note", "old content", 1, newContent, 2, at))
	expectNoteReanchorSet(mock, noteID, thoughtID)
	expectPublishedNoteUpdate(mock, noteID, newContent, 2)
	expectPublishedNoteHistory(mock, noteID, newContent, 2)
	expectNoteThoughtLock(mock, thoughtID, readerThoughtRowForNoteTest(thoughtID, noteID, oldTarget, "keep this thought", 7, at))

	opID := "note-reanchor-" + noteID.String() + "-2-" + thoughtID
	expectDerivedThoughtClock(mock, thoughtID, opID, 7)
	mock.ExpectQuery("(?s)INSERT INTO reader_thought_ops.*RETURNING sequence").
		WithArgs(
			opID,
			"reader-note-publish",
			"update",
			thoughtID,
			"note",
			noteID.String(),
			pgxmock.AnyArg(),
			pgxmock.AnyArg(),
			int64(8),
		).
		WillReturnRows(mock.NewRows([]string{"sequence", "created_at"}).AddRow(int64(8), readerThoughtOperationCreatedAt))
	expectThoughtEventPreviousWinner(mock, thoughtID, "note", noteID.String(), 7, 7)
	mock.ExpectExec("(?s)INSERT INTO reader_thoughts.*winner_logical_clock.*ON CONFLICT").
		WithArgs(
			thoughtID,
			"note",
			noteID.String(),
			pgxmock.AnyArg(),
			pgxmock.AnyArg(),
			pgxmock.AnyArg(),
			"keep this thought",
			"user",
			false,
			int64(8),
			int64(8),
			"reader-note-publish",
			opID,
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	expectThoughtSupersessionEvent(mock, thoughtID, 7, 8)
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM reader_thought_tombstones WHERE thought_id=$1")).
		WithArgs(thoughtID).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	expectPublishedNoteResult(mock, noteID, readerNoteRowForTest(noteID, "Reader note", newContent, 2, nil, 3, at))
	mock.ExpectCommit()

	item, err := repo.PublishNote(context.Background(), model.ReaderNotePublishCommand{
		NoteID:                    noteID,
		ExpectedDraftRevision:     2,
		ExpectedPublishedRevision: 1,
		ReanchorOps: []json.RawMessage{
			readerNoteReanchorOpForTest(thoughtID, noteID, "reanchored", "diff-context", 1, 2, "new quote"),
		},
	})
	if err != nil {
		t.Fatalf("PublishNote() error = %v", err)
	}
	if item == nil || item.PublishedRevision != 2 || item.PublishedContent != newContent {
		t.Fatalf("PublishNote() = %+v", item)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishNoteHistoricalThoughtWritesSingleTombstone(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	noteID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	thoughtID := "thought-note-history"
	at := time.Date(2026, 8, 9, 16, 0, 0, 0, time.UTC)
	oldTarget := json.RawMessage(`{"kind":"note","host_id":"22222222-2222-2222-2222-222222222222","version":{"note_revision":4}}`)
	content := "published note without the old quote"

	mock.ExpectBegin()
	expectPublishedNoteLock(mock, noteID, readerNoteRowForTest(noteID, "Reader note", "old", 4, content, 5, at))
	expectNoteReanchorSet(mock, noteID, thoughtID)
	expectPublishedNoteUpdate(mock, noteID, content, 5)
	expectPublishedNoteHistory(mock, noteID, content, 5)
	thoughtRow := readerThoughtRowForNoteTest(thoughtID, noteID, oldTarget, "historical thought", 12, at)
	expectNoteThoughtLock(mock, thoughtID, thoughtRow)
	expectMarkThoughtLifecycle(
		mock,
		thoughtID,
		thoughtRow,
		"note",
		noteID.String(),
		"note_reanchor_not-reanchored",
		12,
		13,
	)
	expectNoteTombstoneInsert(mock, thoughtID, "note_reanchor_not-reanchored", 13)
	expectPublishedNoteResult(mock, noteID, readerNoteRowForTest(noteID, "Reader note", content, 5, nil, 6, at))
	mock.ExpectCommit()

	item, err := repo.PublishNote(context.Background(), model.ReaderNotePublishCommand{
		NoteID:                    noteID,
		ExpectedDraftRevision:     5,
		ExpectedPublishedRevision: 4,
		ReanchorOps: []json.RawMessage{
			readerNoteReanchorOpForTest(thoughtID, noteID, "historical", "not-reanchored", 4, 5, ""),
		},
	})
	if err != nil {
		t.Fatalf("PublishNote() error = %v", err)
	}
	if item == nil || item.PublishedRevision != 5 {
		t.Fatalf("PublishNote() = %+v", item)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishNoteRejectsThoughtFromAnotherNoteAndRollsBack(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	noteID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	thoughtID := "thought-wrong-note"
	at := time.Date(2026, 8, 9, 17, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	expectPublishedNoteLock(mock, noteID, readerNoteRowForTest(noteID, "Reader note", "old", 1, "updated", 2, at))
	expectNoteReanchorSet(mock, noteID)
	mock.ExpectRollback()

	_, err = repo.PublishNote(context.Background(), model.ReaderNotePublishCommand{
		NoteID:                    noteID,
		ExpectedDraftRevision:     2,
		ExpectedPublishedRevision: 1,
		ReanchorOps: []json.RawMessage{
			readerNoteReanchorOpForTest(thoughtID, noteID, "reanchored", "unique-quote", 1, 2, "updated"),
		},
	})
	if !errors.Is(err, ErrReaderNoteReanchorIncomplete) {
		t.Fatalf("PublishNote() error = %v, want ErrReaderNoteReanchorIncomplete", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishNoteRejectsStaleThoughtTargetAndRollsBack(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	noteID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	thoughtID := "thought-stale-target"
	at := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	staleTarget := json.RawMessage(`{"kind":"note","host_id":"55555555-5555-5555-5555-555555555555","version":{"note_revision":3}}`)

	mock.ExpectBegin()
	expectPublishedNoteLock(mock, noteID, readerNoteRowForTest(noteID, "Reader note", "old", 4, "updated", 5, at))
	expectNoteReanchorSet(mock, noteID, thoughtID)
	expectPublishedNoteUpdate(mock, noteID, "updated", 5)
	expectPublishedNoteHistory(mock, noteID, "updated", 5)
	expectNoteThoughtLock(mock, thoughtID, readerThoughtRowForNoteTest(thoughtID, noteID, staleTarget, "stale", 9, at))
	mock.ExpectRollback()

	_, err = repo.PublishNote(context.Background(), model.ReaderNotePublishCommand{
		NoteID:                    noteID,
		ExpectedDraftRevision:     5,
		ExpectedPublishedRevision: 4,
		ReanchorOps: []json.RawMessage{
			readerNoteReanchorOpForTest(thoughtID, noteID, "reanchored", "diff-context", 4, 5, "updated"),
		},
	})
	if !errors.Is(err, ErrInvalidReaderReanchor) {
		t.Fatalf("PublishNote() error = %v, want ErrInvalidReaderReanchor", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishNoteRejectsDuplicateThoughtIDsAndRollsBack(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	noteID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	thoughtID := "thought-duplicate"
	at := time.Date(2026, 8, 9, 19, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	expectPublishedNoteLock(mock, noteID, readerNoteRowForTest(noteID, "Reader note", "old", 1, "updated", 2, at))
	expectNoteReanchorSet(mock, noteID, thoughtID)
	mock.ExpectRollback()

	first := readerNoteReanchorOpForTest(thoughtID, noteID, "historical", "not-reanchored", 1, 2, "")
	second := readerNoteReanchorOpForTest(thoughtID, noteID, "historical", "not-reanchored", 1, 2, "")
	_, err = repo.PublishNote(context.Background(), model.ReaderNotePublishCommand{
		NoteID:                    noteID,
		ExpectedDraftRevision:     2,
		ExpectedPublishedRevision: 1,
		ReanchorOps:               []json.RawMessage{first, second},
	})
	if !errors.Is(err, ErrReaderNoteReanchorIncomplete) {
		t.Fatalf("PublishNote() error = %v, want ErrReaderNoteReanchorIncomplete", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishNoteRejectsQuoteMissingFromNewContentAndRollsBack(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	noteID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	thoughtID := "thought-missing-quote"
	at := time.Date(2026, 8, 9, 20, 0, 0, 0, time.UTC)
	oldTarget := json.RawMessage(`{"kind":"note","host_id":"77777777-7777-7777-7777-777777777777","version":{"note_revision":1}}`)

	mock.ExpectBegin()
	expectPublishedNoteLock(mock, noteID, readerNoteRowForTest(noteID, "Reader note", "old", 1, "updated", 2, at))
	expectNoteReanchorSet(mock, noteID, thoughtID)
	expectPublishedNoteUpdate(mock, noteID, "updated", 2)
	expectPublishedNoteHistory(mock, noteID, "updated", 2)
	expectNoteThoughtLock(mock, thoughtID, readerThoughtRowForNoteTest(thoughtID, noteID, oldTarget, "missing quote", 4, at))
	mock.ExpectRollback()

	_, err = repo.PublishNote(context.Background(), model.ReaderNotePublishCommand{
		NoteID:                    noteID,
		ExpectedDraftRevision:     2,
		ExpectedPublishedRevision: 1,
		ReanchorOps: []json.RawMessage{
			readerNoteReanchorOpForTest(thoughtID, noteID, "reanchored", "unique-quote", 1, 2, "not in content"),
		},
	})
	if !errors.Is(err, ErrInvalidReaderReanchor) {
		t.Fatalf("PublishNote() error = %v, want ErrInvalidReaderReanchor", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishNoteRejectsInvalidReanchorPayloadBeforeWriting(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	noteID := uuid.New()
	at := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	expectPublishedNoteLock(mock, noteID, readerNoteRowForTest(noteID, "old", "old", 0, "new", 0, at))
	mock.ExpectRollback()
	_, err = repo.PublishNote(context.Background(), model.ReaderNotePublishCommand{
		NoteID:      noteID,
		ReanchorOps: []json.RawMessage{json.RawMessage(`{"status":"reanchored"}`)},
	})
	if !errors.Is(err, ErrInvalidReaderReanchor) {
		t.Fatalf("PublishNote() error = %v, want ErrInvalidReaderReanchor", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishNoteNotFoundRollsBackBeforeWritingHistory(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	noteID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery("(?s)SELECT " + regexp.QuoteMeta(readerNoteColumns) + " FROM reader_notes.*FOR UPDATE").
		WithArgs(noteID).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	_, err = repo.PublishNote(context.Background(), model.ReaderNotePublishCommand{NoteID: noteID})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("PublishNote() error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishNoteRejectsCanonicalEmptyBeforeWriting(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	noteID := uuid.New()
	at := time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	expectPublishedNoteLock(mock, noteID, readerNoteRowForTest(noteID, "Existing", "published", 4, " \n\t", 5, at))
	mock.ExpectRollback()

	_, err = repo.PublishNote(context.Background(), model.ReaderNotePublishCommand{NoteID: noteID, ExpectedDraftRevision: 5, ExpectedPublishedRevision: 4})
	if !errors.Is(err, ErrReaderNoteContentEmpty) {
		t.Fatalf("PublishNote() error = %v, want ErrReaderNoteContentEmpty", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReaderNoteContentCanonicalEmptyUsesOnlyDocumentedASCIIWhitespace(t *testing.T) {
	tests := []struct {
		content string
		want    bool
	}{
		{content: " \t\r\n", want: true},
		{content: "\u00a0", want: false},
		{content: "\u2003", want: false},
		{content: "\u00a0\n", want: false},
	}
	for _, test := range tests {
		if got := readerNoteContentCanonicalEmpty(test.content); got != test.want {
			t.Errorf("readerNoteContentCanonicalEmpty(%q) = %v, want %v", test.content, got, test.want)
		}
	}
}

func TestPublishNoteSameContentIsNoOpWithoutReanchorWrites(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	noteID := uuid.New()
	at := time.Date(2026, 8, 11, 2, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	expectPublishedNoteLock(mock, noteID, readerNoteRowForTest(noteID, "published", "published", 4, nil, 5, at))
	mock.ExpectCommit()

	item, err := repo.PublishNote(context.Background(), model.ReaderNotePublishCommand{NoteID: noteID, ExpectedDraftRevision: 5, ExpectedPublishedRevision: 4})
	if err != nil {
		t.Fatalf("PublishNote() error = %v", err)
	}
	if item == nil || item.PublishedRevision != 4 || item.PublishedContent != "published" {
		t.Fatalf("PublishNote() = %+v, want current note", item)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishNoteRejectsMissingReanchorBeforeWriting(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	noteID := uuid.New()
	at := time.Date(2026, 8, 11, 3, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	expectPublishedNoteLock(mock, noteID, readerNoteRowForTest(noteID, "old", "old", 4, "new", 5, at))
	expectNoteReanchorSet(mock, noteID, "active-thought")
	mock.ExpectRollback()

	_, err = repo.PublishNote(context.Background(), model.ReaderNotePublishCommand{NoteID: noteID, ExpectedDraftRevision: 5, ExpectedPublishedRevision: 4})
	if !errors.Is(err, ErrReaderNoteReanchorIncomplete) {
		t.Fatalf("PublishNote() error = %v, want ErrReaderNoteReanchorIncomplete", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreNoteRevisionCreatesNewRevisionWhenTargetIsCurrent(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	noteID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	at := time.Date(2026, 8, 10, 13, 0, 0, 0, time.UTC)
	row := readerNoteRowForTest(noteID, "already restored", "already restored", 5, nil, 6, at)

	mock.ExpectBegin()
	expectPublishedNoteLock(mock, noteID, row)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT content FROM reader_note_history WHERE note_id=$1 AND revision=$2")).
		WithArgs(noteID, int64(5)).
		WillReturnRows(mock.NewRows([]string{"content"}).AddRow("already restored"))
	expectNoteReanchorSet(mock, noteID)
	mock.ExpectQuery("(?s)UPDATE reader_notes.*published_revision=\\$3.*draft_revision=\\$4.*RETURNING").
		WithArgs(pgxmock.AnyArg(), "already restored", int64(6), int64(7), noteID, int64(5), int64(6)).
		WillReturnRows(mock.NewRows(readerNoteColumnsForTest()).AddRow(
			readerNoteRowForTest(noteID, "already restored", "already restored", 6, nil, 7, at)...,
		))
	expectPublishedNoteHistory(mock, noteID, "already restored", 6)
	mock.ExpectCommit()

	item, err := repo.RestoreNoteRevision(context.Background(), model.ReaderNoteRestoreCommand{NoteID: noteID, Revision: 5, ExpectedDraftRevision: 6, ExpectedPublishedRevision: 5})
	if err != nil {
		t.Fatalf("RestoreNoteRevision() error = %v", err)
	}
	if item == nil || item.PublishedRevision != 6 || item.DraftRevision != 7 || item.PublishedContent != "already restored" {
		t.Fatalf("RestoreNoteRevision() = %+v, want a new revision aligned to restored content", item)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreNoteRevisionPreservesNonEmptyDirtyDraft(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	noteID := uuid.New()
	at := time.Date(2026, 8, 11, 4, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	expectPublishedNoteLock(mock, noteID, readerNoteRowForTest(noteID, "published", "published", 5, "dirty draft", 6, at))
	mock.ExpectRollback()

	_, err = repo.RestoreNoteRevision(context.Background(), model.ReaderNoteRestoreCommand{NoteID: noteID, Revision: 4, ExpectedDraftRevision: 6, ExpectedPublishedRevision: 5})
	if !errors.Is(err, ErrReaderNoteDraftDirty) {
		t.Fatalf("RestoreNoteRevision() error = %v, want ErrReaderNoteDraftDirty", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
