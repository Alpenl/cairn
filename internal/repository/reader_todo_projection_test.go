package repository

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/readertext"
)

func readerThoughtTodoSourceColumns() []string {
	return []string{"body", "last_sequence", "host_kind", "host_id", "link_id"}
}

func TestReplaceHostTodoProjectionsInsertsWhatTheHostEmits(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)

	body := "- [ ] write it down\n"
	block := readertext.List(body)[0]
	linkID := uuid.New()
	projectionID := uuid.New()
	projection := readerChecklistTodos(readerTodoHostSource{
		originKind: "thought", hostID: "thought-1", hostRevision: 11, body: body,
		sourceKind: "link", sourceID: linkID.String(), linkID: &linkID, live: true,
	})[0]

	mock.ExpectQuery(regexp.QuoteMeta(readerThoughtTodoSourceSQL)).
		WithArgs("thought-1").
		WillReturnRows(mock.NewRows(readerThoughtTodoSourceColumns()).AddRow(body, int64(11), "link", linkID.String(), &linkID))
	mock.ExpectQuery(regexp.QuoteMeta(readerExistingHostTodoProjectionsSQL)).
		WithArgs("thought", "thought-1").
		WillReturnRows(mock.NewRows(readerExistingTodoProjectionColumns()))
	mock.ExpectQuery("(?s)SELECT id FROM reader_todos.*FOR UPDATE").
		WithArgs("thought", "thought-1", block.BlockRef, "1").
		WillReturnError(pgxNoRows())
	mock.ExpectQuery("(?s)INSERT INTO reader_todos.*ON CONFLICT.*RETURNING "+regexp.QuoteMeta(readerTodoColumns)).
		WithArgs(projection.Text, projection.DueAt, projection.Done, projection.OriginKind, projection.OriginHostKind,
			projection.OriginHostID, []byte(projection.OriginRef), projection.HostRevision, projection.CompletedAt).
		WillReturnRows(mock.NewRows(readerTodoColumnsForTest()).AddRow(readerTodoRowForTest(projectionID, "thought", false, 11)...))

	if err := repo.replaceHostTodoProjectionsOn(context.Background(), mock, "thought", "thought-1"); err != nil {
		t.Fatalf("replaceHostTodoProjectionsOn() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceHostTodoProjectionsDismissesWhenTheHostIsGone(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)

	projectionID := uuid.New()
	hostID := "thought-1"

	mock.ExpectQuery(regexp.QuoteMeta(readerThoughtTodoSourceSQL)).
		WithArgs(hostID).
		WillReturnError(pgxNoRows())
	mock.ExpectQuery(regexp.QuoteMeta(readerExistingHostTodoProjectionsSQL)).
		WithArgs("thought", hostID).
		WillReturnRows(mock.NewRows(readerExistingTodoProjectionColumns()).
			AddRow(projectionID, "thought", &hostID, []byte(`{"block_ref":"task:gone","occurrence":1}`), nil))
	mock.ExpectExec("UPDATE reader_todos SET deleted_at=COALESCE\\(deleted_at,NOW\\(\\)\\)").
		WithArgs(projectionID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := repo.replaceHostTodoProjectionsOn(context.Background(), mock, "thought", hostID); err != nil {
		t.Fatalf("replaceHostTodoProjectionsOn() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceHostTodoProjectionsHonoursTombstones(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)

	body := "- [ ] already dismissed\n"
	source := readerTodoHostSource{
		originKind: "thought", hostID: "thought-1", hostRevision: 5, body: body,
		sourceKind: "thought", sourceID: "thought-1", live: true,
	}
	projection := readerChecklistTodos(source)[0]
	projectionID := uuid.New()
	hostID := "thought-1"

	mock.ExpectQuery(regexp.QuoteMeta(readerThoughtTodoSourceSQL)).
		WithArgs(hostID).
		WillReturnRows(mock.NewRows(readerThoughtTodoSourceColumns()).AddRow(body, int64(5), "thought", hostID, (*uuid.UUID)(nil)))
	mock.ExpectQuery(regexp.QuoteMeta(readerExistingHostTodoProjectionsSQL)).
		WithArgs("thought", hostID).
		WillReturnRows(mock.NewRows(readerExistingTodoProjectionColumns()).
			AddRow(projectionID, "thought", &hostID, []byte(projection.OriginRef), testReaderTime))

	if err := repo.replaceHostTodoProjectionsOn(context.Background(), mock, "thought", hostID); err != nil {
		t.Fatalf("replaceHostTodoProjectionsOn() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a dismissed projection was rewritten: %v", err)
	}
}

// TestReplaceHostTodoProjectionsReadsOnlyPublishedNoteContent pins the rule
// that a Note draft is not a TODO source. The source query names
// published_content and nothing else, so an unpublished edit cannot leak a
// half-written checklist into the TODO list.
func TestReplaceHostTodoProjectionsReadsOnlyPublishedNoteContent(t *testing.T) {
	t.Parallel()

	if !regexp.MustCompile(`published_content`).MatchString(readerNoteTodoSourceSQL) {
		t.Fatalf("note todo source query = %q, want published content", readerNoteTodoSourceSQL)
	}
	if regexp.MustCompile(`draft`).MatchString(readerNoteTodoSourceSQL) {
		t.Fatalf("note todo source query = %q, want no draft column", readerNoteTodoSourceSQL)
	}

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	noteID := uuid.New()

	mock.ExpectQuery(regexp.QuoteMeta(readerNoteTodoSourceSQL)).
		WithArgs(noteID).
		WillReturnRows(mock.NewRows([]string{"published_content", "published_revision"}).AddRow("", int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta(readerExistingHostTodoProjectionsSQL)).
		WithArgs("note", noteID.String()).
		WillReturnRows(mock.NewRows(readerExistingTodoProjectionColumns()))

	if err := repo.replaceHostTodoProjectionsOn(context.Background(), mock, "note", noteID.String()); err != nil {
		t.Fatalf("replaceHostTodoProjectionsOn() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestReaderChecklistTodosCarriesSourcePointer keeps the projection payload
// identical to the one the reconcile pass writes, so a Home read and a write
// path cannot flip a TODO's "jump to source" link back and forth.
func TestReaderChecklistTodosCarriesSourcePointer(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	items := readerChecklistTodos(readerTodoHostSource{
		originKind: "thought", hostID: "thought-1", hostRevision: 2,
		body:       "- [ ] first\ncontext\n- [x] second\n",
		sourceKind: "link", sourceID: linkID.String(), linkID: &linkID, live: true,
	})
	if len(items) != 2 {
		t.Fatalf("readerChecklistTodos() = %#v, want two blocks", items)
	}
	var ref map[string]any
	if err := json.Unmarshal(items[0].OriginRef, &ref); err != nil {
		t.Fatalf("decode origin_ref: %v", err)
	}
	if ref["source_kind"] != "link" || ref["source_id"] != linkID.String() || ref["link_id"] != linkID.String() {
		t.Fatalf("origin_ref = %v, want the source pointer", ref)
	}
	if !items[1].Done || items[1].HostRevision != 2 {
		t.Fatalf("second block = %#v, want done at revision 2", items[1])
	}
	if items := readerChecklistTodos(readerTodoHostSource{originKind: "thought", hostID: "gone"}); len(items) != 0 {
		t.Fatalf("readerChecklistTodos(dead host) = %#v, want nothing", items)
	}
}

func pgxNoRows() error { return pgx.ErrNoRows }

// expectThoughtTodoProjectionRefresh queues the two reads the write-time
// projection refresh performs for one Thought host. The stubbed source has no
// checklist block, so the refresh is observable but writes nothing — which is
// exactly what a test about some other invariant wants from it.
func expectThoughtTodoProjectionRefresh(mock pgxmock.PgxPoolIface, thoughtID string) {
	mock.ExpectQuery(regexp.QuoteMeta(readerThoughtTodoSourceSQL)).
		WithArgs(thoughtID).
		WillReturnRows(mock.NewRows(readerThoughtTodoSourceColumns()).
			AddRow("", int64(0), "thought", thoughtID, (*uuid.UUID)(nil)))
	mock.ExpectQuery(regexp.QuoteMeta(readerExistingHostTodoProjectionsSQL)).
		WithArgs(readerTodoHostThought, thoughtID).
		WillReturnRows(mock.NewRows(readerExistingTodoProjectionColumns()))
}

func expectNoteTodoProjectionRefresh(mock pgxmock.PgxPoolIface, noteID uuid.UUID) {
	mock.ExpectQuery(regexp.QuoteMeta(readerNoteTodoSourceSQL)).
		WithArgs(noteID).
		WillReturnRows(mock.NewRows([]string{"published_content", "published_revision"}).AddRow("", int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta(readerExistingHostTodoProjectionsSQL)).
		WithArgs(readerTodoHostNote, noteID.String()).
		WillReturnRows(mock.NewRows(readerExistingTodoProjectionColumns()))
}

func expectLinkThoughtTodoProjectionRefresh(mock pgxmock.PgxPoolIface, linkID uuid.UUID, thoughtIDs ...string) {
	rows := mock.NewRows([]string{"id"})
	for _, thoughtID := range thoughtIDs {
		rows = rows.AddRow(thoughtID)
	}
	mock.ExpectQuery(regexp.QuoteMeta(readerLinkHostedThoughtsSQL)).WithArgs(linkID).WillReturnRows(rows)
	for _, thoughtID := range thoughtIDs {
		expectThoughtTodoProjectionRefresh(mock, thoughtID)
	}
}
