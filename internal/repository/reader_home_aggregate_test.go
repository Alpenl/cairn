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

func TestReaderChecklistTodosPreservesStableProjectionIdentity(t *testing.T) {
	source := readerTodoHostSource{
		originKind:   "note",
		hostID:       "note-1",
		hostRevision: 8,
		body:         "# Plan\n- [ ] same\ncontext\n- [x] same\ncontext\n",
		sourceKind:   "note",
		sourceID:     "note-1",
		live:         true,
	}

	items := readerChecklistTodos(source)
	if len(items) != 2 {
		t.Fatalf("readerChecklistTodos() returned %d items, want 2", len(items))
	}
	blocks := readertext.List(source.body)
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

// expectHomeAggregateReads queues the four section reads a Home aggregate
// performs. There is deliberately no projection maintenance in the list: after
// the move to write-time maintenance, Home only reads, and pgxmock fails the
// test if any statement outside these four runs.
func expectHomeAggregateReads(mock pgxmock.PgxPoolIface, todoRows, thoughtRows *pgxmock.Rows, counts []any, continueReading *pgxmock.Rows) {
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeListTodosSQL)).WillReturnRows(todoRows)
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeCountsSQL)).
		WillReturnRows(mock.NewRows([]string{"pending", "expired", "reading", "sites", "subs", "notes", "todos", "thoughts", "unread"}).AddRow(counts...))
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeContinueReadingSQL)).
		WithArgs(readerHomeContinueReadingLimit).
		WillReturnRows(continueReading)
	mock.ExpectQuery(regexp.QuoteMeta(readerHomeRecentThoughtsSQL)).
		WithArgs(readerHomeRecentThoughtsLimit).
		WillReturnRows(thoughtRows)
}

// TestLoadHomeAggregateReadsOneReadOnlySnapshot states the concurrency claim
// where it can be checked cheaply: Home opens a read-only repeatable-read
// transaction and issues four reads. A read-only transaction cannot take the
// row locks that used to make two concurrent Home/Todos reads race for a
// serialization failure.
func TestLoadHomeAggregateReadsOneReadOnlySnapshot(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	now := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	linkID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	todoID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	expectHomeAggregateReads(
		mock,
		mock.NewRows(readerTodoColumnsForTest()).AddRow(readerTodoRowForTest(todoID, "thought", false, 4)...),
		mock.NewRows(readerThoughtSyncColumnsForTest()).AddRow(readerThoughtSyncRow("thought-1", 7, false, now)...),
		[]any{2, 3, 7, 3, 4, 5, 1, 6, 8},
		mock.NewRows([]string{"id", "url", "title", "summary", "read", "read_later", "progress", "last_opened", "created_at"}).
			AddRow(linkID, "https://example.com/read", "Read next", "summary", false, false, float32(0.42), now, now),
	)
	mock.ExpectCommit()

	aggregate, err := NewPGXReaderVNextRepository(mock).LoadHomeAggregate(context.Background())
	if err != nil {
		t.Fatalf("LoadHomeAggregate() error = %v", err)
	}
	if len(aggregate.Todos) != 1 || aggregate.Todos[0].ID != todoID || aggregate.Todos[0].OriginKind != "thought" {
		t.Fatalf("Todos = %#v, want the stored projection", aggregate.Todos)
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

// TestLoadHomeAggregateReportsStoredProjectionLifecycle keeps the lifecycle
// fields Home used to derive while it still wrote: a completed projection
// keeps its completion timestamp and is not expired, and an open one with a
// past due date is.
func TestLoadHomeAggregateReportsStoredProjectionLifecycle(t *testing.T) {
	completedAt := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	overdueAt := time.Now().UTC().Add(-time.Hour)
	completedID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	overdueID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	expectHomeAggregateReads(
		mock,
		mock.NewRows(readerTodoColumnsForTest()).
			AddRow(readerHomeTodoRowForTest(completedID, "thought", true, 9, nil, &completedAt)...).
			AddRow(readerHomeTodoRowForTest(overdueID, "note", false, 3, &overdueAt, nil)...),
		mock.NewRows(readerThoughtSyncColumnsForTest()),
		[]any{0, 0, 0, 0, 0, 0, 1, 1, 0},
		mock.NewRows([]string{"id", "url", "title", "summary", "read", "read_later", "progress", "last_opened", "created_at"}),
	)
	mock.ExpectCommit()

	aggregate, err := NewPGXReaderVNextRepository(mock).LoadHomeAggregate(context.Background())
	if err != nil {
		t.Fatalf("LoadHomeAggregate() error = %v", err)
	}
	if len(aggregate.Todos) != 2 {
		t.Fatalf("Todos = %#v, want both stored projections", aggregate.Todos)
	}
	completed, overdue := aggregate.Todos[0], aggregate.Todos[1]
	if !completed.Done || completed.CompletedAt == nil || !completed.CompletedAt.Equal(completedAt) || completed.Expired {
		t.Fatalf("completed projection = %#v, want done, timestamped and not expired", completed)
	}
	if overdue.Done || overdue.DueAt == nil || !overdue.DueAt.Equal(overdueAt) || !overdue.Expired {
		t.Fatalf("overdue projection = %#v, want open, due and expired", overdue)
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
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
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
