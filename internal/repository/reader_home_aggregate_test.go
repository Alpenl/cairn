package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/readertext"
)

func readerHomeTodoSourceColumns() []string {
	return []string{"host_kind", "host_id", "host_revision", "body", "source_kind", "source_id", "link_id"}
}

func readerHomeTodoSourceRow(thoughtID string, revision int64, body string) []any {
	return []any{"thought", thoughtID, revision, body, "thought", thoughtID, (*uuid.UUID)(nil)}
}

func readerHomeTodoRowForTest(id uuid.UUID, originKind string, done bool, hostRevision int64, dueAt, completedAt *time.Time) []any {
	row := readerTodoRowForTest(id, originKind, done, hostRevision)
	row[2] = dueAt
	row[9] = completedAt
	return row
}

func TestReaderHomeFreshnessHasStableRawStates(t *testing.T) {
	tests := []struct {
		name  string
		state ReaderHomeFreshness
		want  string
	}{
		{name: "fresh", state: ReaderHomeFreshnessFresh, want: "fresh"},
		{name: "partial", state: ReaderHomeFreshnessPartial, want: "partial"},
		{name: "stale", state: ReaderHomeFreshnessStale, want: "stale"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.state) != tt.want {
				t.Fatalf("freshness state = %q, want %q", tt.state, tt.want)
			}
		})
	}
}

func TestHomeChecklistTodosPreservesStableProjectionIdentity(t *testing.T) {
	sources := []readerTodoHostSource{{
		originKind:   "note",
		hostID:       "note-1",
		hostRevision: 8,
		body:         "# Plan\n- [ ] same\ncontext\n- [x] same\ncontext\n",
		sourceKind:   "note",
		sourceID:     "note-1",
		live:         true,
	}}

	items := homeChecklistTodos(sources)
	if len(items) != 2 {
		t.Fatalf("homeChecklistTodos() returned %d items, want 2", len(items))
	}
	blocks := readertext.List(sources[0].body)
	wantOccurrences := []string{"1", "2"}
	for index, item := range items {
		if item.OriginKind != "note" || item.OriginHostID == nil || *item.OriginHostID != "note-1" || item.HostRevision != 8 {
			t.Fatalf("item[%d] = %#v, want note host and revision", index, item)
		}
		if originBlockRef(item.OriginRef) != blocks[index].BlockRef || originBlockOccurrence(item.OriginRef) != wantOccurrences[index] {
			t.Fatalf("item[%d] origin_ref = %s, want stable block identity", index, item.OriginRef)
		}
	}
}

func TestLoadHomeAggregateUsesOneRepeatableReadSnapshot(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	now := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	linkID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	todoID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeTodoSourcesSQL)).
		WillReturnRows(mock.NewRows(readerHomeTodoSourceColumns()))
	mock.ExpectQuery(readerExistingTodoProjectionsPattern).
		WillReturnRows(mock.NewRows(readerExistingTodoProjectionColumns()))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeListTodosSQL)).
		WillReturnRows(mock.NewRows(readerTodoColumnsForTest()).AddRow(readerTodoRowForTest(todoID, "standalone", false, 0)...))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeCountsSQL)).
		WillReturnRows(mock.NewRows([]string{"pending", "expired", "reading", "sites", "subs", "notes", "todos", "thoughts", "unread"}).AddRow(2, 3, 7, 3, 4, 5, 1, 6, 8))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeContinueReadingSQL)).
		WithArgs(readerHomeContinueReadingLimit).
		WillReturnRows(mock.NewRows([]string{"id", "url", "title", "summary", "read", "read_later", "progress", "last_opened", "created_at"}).
			AddRow(linkID, "https://example.com/read", "Read next", "summary", false, false, float32(0.42), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeRecentThoughtsSQL)).
		WithArgs(readerHomeRecentThoughtsLimit).
		WillReturnRows(mock.NewRows(readerThoughtSyncColumnsForTest()).AddRow(readerThoughtSyncRow("thought-1", 7, false, now)...))
	mock.ExpectCommit()

	aggregate, err := NewPGXReaderVNextRepository(mock).LoadHomeAggregate(context.Background())
	if err != nil {
		t.Fatalf("LoadHomeAggregate() error = %v", err)
	}
	if len(aggregate.Todos) != 1 || aggregate.Todos[0].ID != todoID {
		t.Fatalf("Todos = %#v", aggregate.Todos)
	}
	if aggregate.Counts["pending"] != 2 || aggregate.Counts["todos"] != 1 || aggregate.Counts["unread"] != 8 {
		t.Fatalf("Counts = %#v", aggregate.Counts)
	}
	if aggregate.Freshness != ReaderHomeFreshnessFresh {
		t.Fatalf("Freshness = %q, want %q", aggregate.Freshness, ReaderHomeFreshnessFresh)
	}
	if aggregate.Counts["inbox"] != 2 || aggregate.Counts["inbox_expired"] != 3 || aggregate.Counts["links"] != 7 || aggregate.Counts["subscriptions"] != 4 {
		t.Fatalf("compatibility count aliases = %#v", aggregate.Counts)
	}
	if len(aggregate.ContinueReading) != 1 || aggregate.ContinueReading[0].Key != "link:"+linkID.String() {
		t.Fatalf("ContinueReading = %#v", aggregate.ContinueReading)
	}
	if len(aggregate.RecentThoughts) != 1 || aggregate.RecentThoughts[0].ID != "thought-1" {
		t.Fatalf("RecentThoughts = %#v", aggregate.RecentThoughts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestLoadHomeAggregateReconcilesSourceTodosBeforeReadingCounts(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	body := "- [ ] ship the aggregate\n"
	block := readertext.List(body)[0]
	projection := homeChecklistTodos([]readerTodoHostSource{{originKind: "thought", hostID: "thought-1", hostRevision: 9, body: body, sourceKind: "thought", sourceID: "thought-1", live: true}})[0]
	projectionID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeTodoSourcesSQL)).
		WillReturnRows(mock.NewRows(readerHomeTodoSourceColumns()).AddRow(readerHomeTodoSourceRow("thought-1", 9, body)...))
	mock.ExpectQuery(readerExistingTodoProjectionsPattern).
		WillReturnRows(mock.NewRows(readerExistingTodoProjectionColumns()))
	mock.ExpectQuery("(?s)SELECT id FROM reader_todos.*FOR UPDATE").
		WithArgs("thought", "thought-1", block.BlockRef, "1").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("(?s)INSERT INTO reader_todos.*RETURNING "+regexp.QuoteMeta(readerTodoColumns)).
		WithArgs(projection.Text, projection.DueAt, projection.Done, projection.OriginKind, projection.OriginHostKind, projection.OriginHostID, []byte(projection.OriginRef), projection.HostRevision, projection.CompletedAt).
		WillReturnRows(mock.NewRows(readerTodoColumnsForTest()).AddRow(readerTodoRowForTest(projectionID, "thought", false, 9)...))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeListTodosSQL)).
		WillReturnRows(mock.NewRows(readerTodoColumnsForTest()).AddRow(readerTodoRowForTest(projectionID, "thought", false, 9)...))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeCountsSQL)).
		WillReturnRows(mock.NewRows([]string{"pending", "expired", "reading", "sites", "subs", "notes", "todos", "thoughts", "unread"}).AddRow(0, 0, 0, 0, 0, 0, 1, 1, 0))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeContinueReadingSQL)).
		WithArgs(readerHomeContinueReadingLimit).
		WillReturnRows(mock.NewRows([]string{"id", "url", "title", "summary", "read", "read_later", "progress", "last_opened", "created_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeRecentThoughtsSQL)).
		WithArgs(readerHomeRecentThoughtsLimit).
		WillReturnRows(mock.NewRows(readerThoughtSyncColumnsForTest()))
	mock.ExpectCommit()

	aggregate, err := NewPGXReaderVNextRepository(mock).LoadHomeAggregate(context.Background())
	if err != nil {
		t.Fatalf("LoadHomeAggregate() error = %v", err)
	}
	if len(aggregate.Todos) != 1 || aggregate.Todos[0].ID != projectionID {
		t.Fatalf("Todos = %#v, want reconciled projection %s", aggregate.Todos, projectionID)
	}
	if aggregate.Counts["todos"] != 1 {
		t.Fatalf("todo count = %d, want 1", aggregate.Counts["todos"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestLoadHomeAggregateProjectsCompletedTodoWithCompletionTimestamp(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	body := "- [x] ship the aggregate\n"
	block := readertext.List(body)[0]
	projection := homeChecklistTodos([]readerTodoHostSource{{originKind: "thought", hostID: "thought-1", hostRevision: 9, body: body, sourceKind: "thought", sourceID: "thought-1", live: true}})[0]
	projectionID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	completedAt := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	row := readerHomeTodoRowForTest(projectionID, "thought", true, 9, nil, &completedAt)

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeTodoSourcesSQL)).
		WillReturnRows(mock.NewRows(readerHomeTodoSourceColumns()).AddRow(readerHomeTodoSourceRow("thought-1", 9, body)...))
	mock.ExpectQuery(readerExistingTodoProjectionsPattern).
		WillReturnRows(mock.NewRows(readerExistingTodoProjectionColumns()))
	mock.ExpectQuery("(?s)SELECT id FROM reader_todos.*FOR UPDATE").
		WithArgs("thought", "thought-1", block.BlockRef, "1").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("(?s)INSERT INTO reader_todos.*completed_at.*CASE WHEN \\$3 THEN COALESCE\\(\\$9::timestamptz,NOW\\(\\)\\).*RETURNING "+regexp.QuoteMeta(readerTodoColumns)).
		WithArgs(projection.Text, projection.DueAt, projection.Done, projection.OriginKind, projection.OriginHostKind, projection.OriginHostID, []byte(projection.OriginRef), projection.HostRevision, projection.CompletedAt).
		WillReturnRows(mock.NewRows(readerTodoColumnsForTest()).AddRow(row...))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeListTodosSQL)).
		WillReturnRows(mock.NewRows(readerTodoColumnsForTest()).AddRow(row...))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeCountsSQL)).
		WillReturnRows(mock.NewRows([]string{"pending", "expired", "reading", "sites", "subs", "notes", "todos", "thoughts", "unread"}).AddRow(0, 0, 0, 0, 0, 0, 0, 1, 0))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeContinueReadingSQL)).
		WithArgs(readerHomeContinueReadingLimit).
		WillReturnRows(mock.NewRows([]string{"id", "url", "title", "summary", "read", "read_later", "progress", "last_opened", "created_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeRecentThoughtsSQL)).
		WithArgs(readerHomeRecentThoughtsLimit).
		WillReturnRows(mock.NewRows(readerThoughtSyncColumnsForTest()))
	mock.ExpectCommit()

	aggregate, err := NewPGXReaderVNextRepository(mock).LoadHomeAggregate(context.Background())
	if err != nil {
		t.Fatalf("LoadHomeAggregate() error = %v", err)
	}
	if len(aggregate.Todos) != 1 {
		t.Fatalf("Todos = %#v, want one completed projection", aggregate.Todos)
	}
	todo := aggregate.Todos[0]
	if !todo.Done || todo.CompletedAt == nil || !todo.CompletedAt.Equal(completedAt) {
		t.Fatalf("completed todo lifecycle = %#v, want done with completion timestamp", todo)
	}
	if todo.Expired {
		t.Fatal("completed todo must not be marked expired")
	}
	if aggregate.Counts["todos"] != 0 {
		t.Fatalf("todo count = %d, want no incomplete todos", aggregate.Counts["todos"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestLoadHomeAggregateReopensProjectionAndPreservesDueDate(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	body := "- [ ] reopen the aggregate\n"
	block := readertext.List(body)[0]
	projection := homeChecklistTodos([]readerTodoHostSource{{originKind: "thought", hostID: "thought-1", hostRevision: 9, body: body, sourceKind: "thought", sourceID: "thought-1", live: true}})[0]
	projectionID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	hostID := "thought-1"
	dueAt := time.Now().UTC().Add(-time.Hour)
	row := readerHomeTodoRowForTest(projectionID, "thought", false, 9, &dueAt, nil)

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeTodoSourcesSQL)).
		WillReturnRows(mock.NewRows(readerHomeTodoSourceColumns()).AddRow(readerHomeTodoSourceRow("thought-1", 9, body)...))
	mock.ExpectQuery(readerExistingTodoProjectionsPattern).
		WillReturnRows(mock.NewRows(readerExistingTodoProjectionColumns()).AddRow(projectionID, "thought", &hostID, []byte(projection.OriginRef), nil))
	mock.ExpectQuery("(?s)SELECT id FROM reader_todos.*FOR UPDATE").
		WithArgs("thought", "thought-1", block.BlockRef, "1").
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(projectionID))
	mock.ExpectQuery("(?s)UPDATE reader_todos SET text=.*completed_at=CASE.*RETURNING "+regexp.QuoteMeta(readerTodoColumns)).
		WithArgs(projection.Text, projection.OriginHostKind, []byte(projection.OriginRef), projection.HostRevision, projection.Done, projectionID).
		WillReturnRows(mock.NewRows(readerTodoColumnsForTest()).AddRow(row...))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeListTodosSQL)).
		WillReturnRows(mock.NewRows(readerTodoColumnsForTest()).AddRow(row...))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeCountsSQL)).
		WillReturnRows(mock.NewRows([]string{"pending", "expired", "reading", "sites", "subs", "notes", "todos", "thoughts", "unread"}).AddRow(0, 0, 0, 0, 0, 0, 1, 1, 0))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeContinueReadingSQL)).
		WithArgs(readerHomeContinueReadingLimit).
		WillReturnRows(mock.NewRows([]string{"id", "url", "title", "summary", "read", "read_later", "progress", "last_opened", "created_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeRecentThoughtsSQL)).
		WithArgs(readerHomeRecentThoughtsLimit).
		WillReturnRows(mock.NewRows(readerThoughtSyncColumnsForTest()))
	mock.ExpectCommit()

	aggregate, err := NewPGXReaderVNextRepository(mock).LoadHomeAggregate(context.Background())
	if err != nil {
		t.Fatalf("LoadHomeAggregate() error = %v", err)
	}
	if len(aggregate.Todos) != 1 {
		t.Fatalf("Todos = %#v, want one reopened projection", aggregate.Todos)
	}
	todo := aggregate.Todos[0]
	if todo.Done || todo.CompletedAt != nil || todo.DueAt == nil || !todo.DueAt.Equal(dueAt) || !todo.Expired {
		t.Fatalf("reopened todo lifecycle = %#v, want due, expired, and no completion timestamp", todo)
	}
	if todo.HostRevision != 9 {
		t.Fatalf("host revision = %d, want 9", todo.HostRevision)
	}
	if aggregate.Counts["todos"] != 1 {
		t.Fatalf("todo count = %d, want one incomplete todo", aggregate.Counts["todos"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestLoadHomeAggregateDismissesStaleProjectionBeforeReading(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	projectionID := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	hostID := "thought-1"
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeTodoSourcesSQL)).
		WillReturnRows(mock.NewRows(readerHomeTodoSourceColumns()))
	mock.ExpectQuery(readerExistingTodoProjectionsPattern).
		WillReturnRows(mock.NewRows(readerExistingTodoProjectionColumns()).AddRow(projectionID, "thought", &hostID, []byte(`{"block_ref":"task:stale","occurrence":1}`), nil))
	mock.ExpectExec("UPDATE reader_todos SET deleted_at=COALESCE\\(deleted_at,NOW\\(\\)\\),updated_at=NOW\\(\\) WHERE id=\\$1 AND deleted_at IS NULL").
		WithArgs(projectionID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeListTodosSQL)).
		WillReturnRows(mock.NewRows(readerTodoColumnsForTest()))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeCountsSQL)).
		WillReturnRows(mock.NewRows([]string{"pending", "expired", "reading", "sites", "subs", "notes", "todos", "thoughts", "unread"}).AddRow(0, 0, 0, 0, 0, 0, 0, 0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeContinueReadingSQL)).
		WithArgs(readerHomeContinueReadingLimit).
		WillReturnRows(mock.NewRows([]string{"id", "url", "title", "summary", "read", "read_later", "progress", "last_opened", "created_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeRecentThoughtsSQL)).
		WithArgs(readerHomeRecentThoughtsLimit).
		WillReturnRows(mock.NewRows(readerThoughtSyncColumnsForTest()))
	mock.ExpectCommit()

	aggregate, err := NewPGXReaderVNextRepository(mock).LoadHomeAggregate(context.Background())
	if err != nil {
		t.Fatalf("LoadHomeAggregate() error = %v", err)
	}
	if len(aggregate.Todos) != 0 || aggregate.Counts["todos"] != 0 {
		t.Fatalf("stale projection remained visible: todos=%#v counts=%#v", aggregate.Todos, aggregate.Counts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestLoadHomeAggregateRollsBackInsteadOfReturningPartialData(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	failure := errors.New("todo list failed")
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeTodoSourcesSQL)).
		WillReturnRows(mock.NewRows(readerHomeTodoSourceColumns()))
	mock.ExpectQuery(readerExistingTodoProjectionsPattern).
		WillReturnRows(mock.NewRows(readerExistingTodoProjectionColumns()))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeListTodosSQL)).
		WillReturnError(failure)
	mock.ExpectRollback()

	aggregate, err := NewPGXReaderVNextRepository(mock).LoadHomeAggregate(context.Background())
	if !errors.Is(err, failure) {
		t.Fatalf("LoadHomeAggregate() error = %v, want %v", err, failure)
	}
	if aggregate.Counts != nil || aggregate.Todos != nil || aggregate.ContinueReading != nil || aggregate.RecentThoughts != nil {
		t.Fatalf("LoadHomeAggregate() returned partial aggregate: %#v", aggregate)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

// TestLoadHomeAggregateDoesNotResurrectDismissedProjection locks the defect
// that made Home and Todos disagree: Home read only the live projections, so a
// key the user had already dismissed looked missing and was re-inserted on the
// next Home open. Both paths now share one tombstone rule.
func TestLoadHomeAggregateDoesNotResurrectDismissedProjection(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	body := "- [ ] dismissed by the user\n"
	projection := homeChecklistTodos([]readerTodoHostSource{{originKind: "thought", hostID: "thought-1", hostRevision: 3, body: body, sourceKind: "thought", sourceID: "thought-1", live: true}})[0]
	projectionID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	hostID := "thought-1"
	dismissedAt := time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC)

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeTodoSourcesSQL)).
		WillReturnRows(mock.NewRows(readerHomeTodoSourceColumns()).AddRow(readerHomeTodoSourceRow("thought-1", 3, body)...))
	mock.ExpectQuery(readerExistingTodoProjectionsPattern).
		WillReturnRows(mock.NewRows(readerExistingTodoProjectionColumns()).
			AddRow(projectionID, "thought", &hostID, []byte(projection.OriginRef), dismissedAt))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeListTodosSQL)).
		WillReturnRows(mock.NewRows(readerTodoColumnsForTest()))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeCountsSQL)).
		WillReturnRows(mock.NewRows([]string{"pending", "expired", "reading", "sites", "subs", "notes", "todos", "thoughts", "unread"}).AddRow(0, 0, 0, 0, 0, 0, 0, 1, 0))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeContinueReadingSQL)).
		WithArgs(readerHomeContinueReadingLimit).
		WillReturnRows(mock.NewRows([]string{"id", "url", "title", "summary", "read", "read_later", "progress", "last_opened", "created_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeRecentThoughtsSQL)).
		WithArgs(readerHomeRecentThoughtsLimit).
		WillReturnRows(mock.NewRows(readerThoughtSyncColumnsForTest()))
	mock.ExpectCommit()

	aggregate, err := NewPGXReaderVNextRepository(mock).LoadHomeAggregate(context.Background())
	if err != nil {
		t.Fatalf("LoadHomeAggregate() error = %v", err)
	}
	if len(aggregate.Todos) != 0 {
		t.Fatalf("Todos = %#v, want the dismissed projection to stay dismissed", aggregate.Todos)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("Home resurrected a dismissed projection: %v", err)
	}
}
