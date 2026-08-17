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

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/readertext"
)

var testReaderTime = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

func readerTodoColumnsForTest() []string {
	return []string{
		"id", "text", "due_at", "done", "origin_kind", "origin_host_kind", "origin_host_id",
		"origin_ref", "host_revision", "completed_at", "created_at", "updated_at",
	}
}

func readerTodoRowForTest(id uuid.UUID, originKind string, done bool, hostRevision int64) []any {
	var nilString *string
	var nilTime *time.Time
	return []any{id, "task", nilTime, done, originKind, nilString, nilString, []byte(`{"block_ref":"task:123"}`), hostRevision, nilTime, testReaderTime, testReaderTime}
}

func TestListTodosReturnsStableKeysetCursor(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	first, second := uuid.New(), uuid.New()
	rows := mock.NewRows(readerTodoColumnsForTest()).
		AddRow(readerTodoRowForTest(first, "standalone", false, 0)...).
		AddRow(readerTodoRowForTest(second, "standalone", false, 0)...)
	mock.ExpectQuery("(?s)SELECT .* FROM reader_todos.*ORDER BY done ASC.*LIMIT \\$7").
		WithArgs(false, 0, 1, (*time.Time)(nil), time.Time{}, uuid.Nil, 2).
		WillReturnRows(rows)

	page, err := repo.ListTodos(context.Background(), "", 1)
	if err != nil {
		t.Fatalf("ListTodos() error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != first || page.Next == "" {
		t.Fatalf("ListTodos() = %#v", page)
	}
	cursor, err := parseReaderTodoCursor(page.Next)
	if err != nil || cursor.ID != first || cursor.CreatedAt != testReaderTime {
		t.Fatalf("cursor = %#v, error = %v", cursor, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListTodosRejectsInvalidCursorAndLimitBeforeQuery(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	if _, err := repo.ListTodos(context.Background(), "not-base64!", 50); !errors.Is(err, ErrInvalidReaderCursor) {
		t.Fatalf("invalid cursor error = %v", err)
	}
	if _, err := repo.ListTodos(context.Background(), "", 201); !errors.Is(err, ErrInvalidReaderCursor) {
		t.Fatalf("invalid limit error = %v", err)
	}
}

func TestPatchTodoProjectedRequiresMatchingHostRevision(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	id := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT origin_kind,host_revision FROM reader_todos WHERE id=$1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(id).
		WillReturnRows(mock.NewRows([]string{"origin_kind", "host_revision"}).AddRow("note", int64(7)))
	mock.ExpectRollback()

	wrong := int64(6)
	_, err = repo.PatchTodo(context.Background(), model.ReaderTodoPatch{ID: id, Done: boolPtrForReaderTodoTest(true), ExpectedHostRevision: &wrong})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("PatchTodo() error = %v, want ErrRevisionConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPatchTodoProjectedWithoutExpectedRevisionConflicts(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	id := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT origin_kind,host_revision FROM reader_todos WHERE id=$1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(id).
		WillReturnRows(mock.NewRows([]string{"origin_kind", "host_revision"}).AddRow("thought", int64(3)))
	mock.ExpectRollback()

	_, err = repo.PatchTodo(context.Background(), model.ReaderTodoPatch{ID: id, Text: stringPtrForReaderTodoTest("changed")})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("PatchTodo() error = %v, want ErrRevisionConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPatchTodoStandaloneDoesNotRequireHostRevision(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	id := uuid.New()
	changed := "changed"
	var dueAt *time.Time
	var done *bool

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT origin_kind,host_revision FROM reader_todos WHERE id=$1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(id).
		WillReturnRows(mock.NewRows([]string{"origin_kind", "host_revision"}).AddRow("standalone", int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE reader_todos SET")).
		WithArgs(&changed, false, dueAt, done, id).
		WillReturnRows(mock.NewRows(readerTodoColumnsForTest()).AddRow(readerTodoRowForTest(id, "standalone", false, 0)...))
	mock.ExpectCommit()

	item, err := repo.PatchTodo(context.Background(), model.ReaderTodoPatch{ID: id, Text: stringPtrForReaderTodoTest("changed")})
	if err != nil {
		t.Fatalf("PatchTodo() error = %v", err)
	}
	if item == nil || item.ID != id || item.OriginKind != "standalone" {
		t.Fatalf("PatchTodo() = %+v", item)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPatchTodoHostRevisionApplicabilityMatrix(t *testing.T) {
	t.Parallel()

	checkedSource := "- [x] matching projected task"
	blocks := readertext.List(checkedSource)
	if len(blocks) != 1 {
		t.Fatalf("readertext.List(%q) = %#v, want one block", checkedSource, blocks)
	}
	negative, zero, stale, matching, positive := int64(-1), int64(0), int64(6), int64(7), int64(99)
	tests := []struct {
		name                    string
		originKind              string
		expectedHostRevision    *int64
		expectedHostRevisionSet bool
		matchingProjection      bool
		wantErr                 error
	}{
		{name: "standalone omitted", originKind: "standalone"},
		{name: "standalone explicit negative", originKind: "standalone", expectedHostRevision: &negative, wantErr: ErrReaderTodoHostRevisionNotApplicable},
		{name: "standalone explicit zero", originKind: "standalone", expectedHostRevision: &zero, wantErr: ErrReaderTodoHostRevisionNotApplicable},
		{name: "standalone explicit positive", originKind: "standalone", expectedHostRevision: &positive, wantErr: ErrReaderTodoHostRevisionNotApplicable},
		{name: "standalone explicit null", originKind: "standalone", expectedHostRevisionSet: true, wantErr: ErrReaderTodoHostRevisionNotApplicable},
		{name: "projected omitted", originKind: "note", wantErr: ErrRevisionConflict},
		{name: "projected negative", originKind: "note", expectedHostRevision: &negative, wantErr: ErrRevisionConflict},
		{name: "projected stale", originKind: "note", expectedHostRevision: &stale, wantErr: ErrRevisionConflict},
		{name: "projected matching", originKind: "note", expectedHostRevision: &matching, matchingProjection: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatal(err)
			}
			defer mock.Close()
			repo := NewPGXReaderVNextRepository(mock)
			id := uuid.New()
			done := true

			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT origin_kind,host_revision FROM reader_todos WHERE id=$1 AND deleted_at IS NULL FOR UPDATE")).
				WithArgs(id).
				WillReturnRows(mock.NewRows([]string{"origin_kind", "host_revision"}).AddRow(tt.originKind, int64(7)))

			switch {
			case tt.matchingProjection:
				noteID := uuid.New()
				originRef := []byte(`{"block_ref":"` + blocks[0].BlockRef + `","occurrence":1}`)
				mock.ExpectQuery("(?s)SELECT origin_kind,origin_host_kind,origin_host_id,origin_ref.*FROM reader_todos.*FOR UPDATE").
					WithArgs(id).
					WillReturnRows(mock.NewRows([]string{"origin_kind", "origin_host_kind", "origin_host_id", "origin_ref"}).AddRow("note", "note", noteID.String(), originRef))
				mock.ExpectQuery("(?s)SELECT title,published_content,published_revision,draft_content.*FROM reader_notes.*FOR UPDATE").
					WithArgs(noteID).
					WillReturnRows(mock.NewRows([]string{"title", "published_content", "published_revision", "draft_content"}).AddRow("note", checkedSource, int64(7), nil))
				mock.ExpectQuery("(?s)UPDATE reader_todos SET.*RETURNING "+regexp.QuoteMeta(readerTodoColumns)).
					WithArgs(blocks[0].Text, done, int64(7), id).
					WillReturnRows(mock.NewRows(readerTodoColumnsForTest()).AddRow(readerTodoRowForTest(id, "note", true, 7)...))
				mock.ExpectCommit()
			case tt.wantErr != nil:
				mock.ExpectRollback()
			default:
				var text *string
				var dueAt *time.Time
				mock.ExpectQuery(regexp.QuoteMeta("UPDATE reader_todos SET")).
					WithArgs(text, false, dueAt, &done, id).
					WillReturnRows(mock.NewRows(readerTodoColumnsForTest()).AddRow(readerTodoRowForTest(id, "standalone", true, 7)...))
				mock.ExpectCommit()
			}

			item, err := repo.PatchTodo(context.Background(), model.ReaderTodoPatch{
				ID:                      id,
				Done:                    &done,
				ExpectedHostRevision:    tt.expectedHostRevision,
				ExpectedHostRevisionSet: tt.expectedHostRevisionSet,
			})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("PatchTodo() error = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil || item == nil {
				t.Fatalf("PatchTodo() = %#v, %v", item, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReaderTodoPatchRequestPreservesFieldPresence(t *testing.T) {
	var omitted, explicitNullDueAt, explicitNegativeRevision, explicitRevision, explicitNullRevision dto.ReaderTodoPatchRequest
	if err := json.Unmarshal([]byte(`{"text":"keep"}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"due_at":null}`), &explicitNullDueAt); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"expected_host_revision":-1}`), &explicitNegativeRevision); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"expected_host_revision":0}`), &explicitRevision); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"expected_host_revision":null}`), &explicitNullRevision); err != nil {
		t.Fatal(err)
	}
	if omitted.DueAtSet {
		t.Fatal("omitted due_at marked as present")
	}
	if !explicitNullDueAt.DueAtSet || explicitNullDueAt.DueAt != nil {
		t.Fatalf("explicit null due_at = %#v, want present nil", explicitNullDueAt)
	}
	if !explicitNegativeRevision.ExpectedHostRevisionSet || explicitNegativeRevision.ExpectedHostRevision == nil || *explicitNegativeRevision.ExpectedHostRevision != -1 {
		t.Fatalf("explicit negative expected_host_revision = %#v, want present -1", explicitNegativeRevision)
	}
	if !explicitRevision.ExpectedHostRevisionSet || explicitRevision.ExpectedHostRevision == nil || *explicitRevision.ExpectedHostRevision != 0 {
		t.Fatalf("explicit zero expected_host_revision = %#v, want present zero", explicitRevision)
	}
	if !explicitNullRevision.ExpectedHostRevisionSet || explicitNullRevision.ExpectedHostRevision != nil {
		t.Fatalf("explicit null expected_host_revision = %#v, want present nil", explicitNullRevision)
	}
}

func TestPatchTodoStandaloneExplicitNullClearsDueAt(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	id := uuid.New()
	var text *string

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT origin_kind,host_revision FROM reader_todos WHERE id=$1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(id).
		WillReturnRows(mock.NewRows([]string{"origin_kind", "host_revision"}).AddRow("standalone", int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE reader_todos SET")).
		WithArgs(text, true, (*time.Time)(nil), (*bool)(nil), id).
		WillReturnRows(mock.NewRows(readerTodoColumnsForTest()).AddRow(readerTodoRowForTest(id, "standalone", false, 0)...))
	mock.ExpectCommit()

	item, err := repo.PatchTodo(context.Background(), model.ReaderTodoPatch{ID: id, DueAtSet: true})
	if err != nil {
		t.Fatalf("PatchTodo() error = %v", err)
	}
	if item == nil || item.ID != id {
		t.Fatalf("PatchTodo() = %+v", item)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteTodoRejectsProjectedWithoutSoftDelete(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	id := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT origin_kind FROM reader_todos WHERE id=$1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(id).
		WillReturnRows(mock.NewRows([]string{"origin_kind"}).AddRow("thought"))
	mock.ExpectRollback()

	err = repo.DeleteTodo(context.Background(), id)
	if !errors.Is(err, ErrReaderTodoProjectionImmutable) {
		t.Fatalf("DeleteTodo() error = %v, want ErrReaderTodoProjectionImmutable", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteTodoStandaloneSoftDeletesInTransaction(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	id := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT origin_kind FROM reader_todos WHERE id=$1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(id).
		WillReturnRows(mock.NewRows([]string{"origin_kind"}).AddRow("standalone"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE reader_todos SET deleted_at=NOW(),updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL")).
		WithArgs(id).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if err := repo.DeleteTodo(context.Background(), id); err != nil {
		t.Fatalf("DeleteTodo() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileTodoProjectionsDoesNotRecreateDeletedProjection(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	id := uuid.New()
	hostID := "thought-1"
	deletedAt := testReaderTime
	projection := model.ReaderTodo{
		Text: "stale task", OriginKind: "thought", OriginHostKind: stringPtrForReaderTodoTest("thought"),
		OriginHostID: &hostID, OriginRef: []byte(`{"block_ref":"task:stale","occurrence":1}`), HostRevision: 4,
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,origin_kind,origin_host_id,origin_ref,deleted_at FROM reader_todos WHERE origin_kind <> 'standalone' FOR UPDATE")).
		WillReturnRows(mock.NewRows([]string{"id", "origin_kind", "origin_host_id", "origin_ref", "deleted_at"}).AddRow(id, "thought", &hostID, []byte(projection.OriginRef), deletedAt))
	mock.ExpectCommit()

	if err := repo.ReconcileTodoProjections(context.Background(), []model.ReaderTodo{projection}); err != nil {
		t.Fatalf("ReconcileTodoProjections() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTodoThoughtWritebackOpIDIsStableForRetries(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	first := readerTodoThoughtWritebackOpID(id, 7, "task:stable", 2, true)
	second := readerTodoThoughtWritebackOpID(id, 7, "task:stable", 2, true)
	if first == "" || first != second {
		t.Fatalf("writeback op ids = %q / %q, want the same non-empty id", first, second)
	}
	if first == readerTodoThoughtWritebackOpID(id, 7, "task:stable", 2, false) {
		t.Fatal("changing desired done state must change the writeback op id")
	}
	if first == readerTodoThoughtWritebackOpID(id, 8, "task:stable", 2, true) {
		t.Fatal("changing host revision must change the writeback op id")
	}
}

func TestPatchTodoProjectedRejectsExplicitDueAtNull(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	id := uuid.New()
	done := true

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT origin_kind,host_revision FROM reader_todos WHERE id=$1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(id).
		WillReturnRows(mock.NewRows([]string{"origin_kind", "host_revision"}).AddRow("note", int64(7)))
	mock.ExpectRollback()

	_, err = repo.PatchTodo(context.Background(), model.ReaderTodoPatch{ID: id, DueAtSet: true, Done: &done, ExpectedHostRevision: int64PointerForReaderTodoTest(7)})
	if !errors.Is(err, ErrReaderTodoProjectionImmutable) {
		t.Fatalf("PatchTodo() error = %v, want ErrReaderTodoProjectionImmutable", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileTodoProjectionsSetsCompletedAtForNewCompletedProjection(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	hostID := "thought-completed"
	projection := model.ReaderTodo{
		Text: "already done", Done: true, OriginKind: "thought", OriginHostKind: stringPtrForReaderTodoTest("thought"),
		OriginHostID: &hostID, OriginRef: []byte(`{"block_ref":"task:done","occurrence":1}`), HostRevision: 4,
	}
	row := readerTodoRowForTest(uuid.New(), "thought", true, 4)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,origin_kind,origin_host_id,origin_ref,deleted_at FROM reader_todos WHERE origin_kind <> 'standalone' FOR UPDATE")).
		WillReturnRows(mock.NewRows([]string{"id", "origin_kind", "origin_host_id", "origin_ref", "deleted_at"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM reader_todos")).
		WithArgs("thought", hostID, "task:done", "1").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("(?s)INSERT INTO reader_todos.*VALUES.*CASE WHEN \\$3 THEN COALESCE\\(\\$9::timestamptz,NOW\\(\\)\\) ELSE NULL END.*RETURNING "+regexp.QuoteMeta(readerTodoColumns)).
		WithArgs(projection.Text, (*time.Time)(nil), projection.Done, projection.OriginKind, projection.OriginHostKind, projection.OriginHostID, []byte(projection.OriginRef), projection.HostRevision, projection.CompletedAt).
		WillReturnRows(mock.NewRows(readerTodoColumnsForTest()).AddRow(row...))
	mock.ExpectCommit()

	items := []model.ReaderTodo{projection}
	if err := repo.ReconcileTodoProjections(context.Background(), items); err != nil {
		t.Fatalf("ReconcileTodoProjections() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPatchTodoReturnsNotFoundForMissingRowBeforeRevisionApplicability(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	id := uuid.New()
	negative := int64(-1)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT origin_kind,host_revision FROM reader_todos WHERE id=$1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(id).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	_, err = repo.PatchTodo(context.Background(), model.ReaderTodoPatch{
		ID:                   id,
		Done:                 boolPtrForReaderTodoTest(true),
		ExpectedHostRevision: &negative,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("PatchTodo() error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func stringPtrForReaderTodoTest(value string) *string { return &value }

func boolPtrForReaderTodoTest(value bool) *bool { return &value }

func int64PointerForReaderTodoTest(value int64) *int64 { return &value }

// readerExistingTodoProjectionsPattern matches the one projection inventory
// every reconcile path now shares. It deliberately includes deleted_at: a
// reconcile that cannot see the tombstoned keys cannot honour them.
const readerExistingTodoProjectionsPattern = `(?s)SELECT id,origin_kind,origin_host_id,origin_ref,deleted_at.*FROM reader_todos.*WHERE origin_kind <> 'standalone'.*FOR UPDATE`

func readerExistingTodoProjectionColumns() []string {
	return []string{"id", "origin_kind", "origin_host_id", "origin_ref", "deleted_at"}
}

func TestReconcileTodoProjectionsKeepsDismissedProjectionDismissed(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)

	body := "- [ ] resurrect me\n"
	block := readertext.List(body)[0]
	projection := homeChecklistTodos([]homeTodoSource{{hostKind: "thought", hostID: "thought-1", hostRevision: 4, body: body}})[0]
	projectionID := uuid.New()
	hostID := "thought-1"
	dismissedAt := testReaderTime

	mock.ExpectBegin()
	mock.ExpectQuery(readerExistingTodoProjectionsPattern).
		WillReturnRows(mock.NewRows(readerExistingTodoProjectionColumns()).
			AddRow(projectionID, "thought", &hostID, []byte(projection.OriginRef), dismissedAt))
	mock.ExpectCommit()

	if err := repo.ReconcileTodoProjections(context.Background(), []model.ReaderTodo{projection}); err != nil {
		t.Fatalf("ReconcileTodoProjections() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("dismissed projection was rewritten: %v; block=%s", err, block.BlockRef)
	}
}
