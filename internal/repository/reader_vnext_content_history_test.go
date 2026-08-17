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

func TestCurrentSavedContentRestoreThoughtEligibility(t *testing.T) {
	linkID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	target := func(value string) json.RawMessage { return json.RawMessage(value) }
	for _, testCase := range []struct {
		name string
		item model.ReaderThought
		want bool
	}{
		{
			name: "current saved-content anchor",
			item: model.ReaderThought{HostKind: "link", HostID: linkID.String(), Target: target(`{"kind":"saved-content","host_id":"cccccccc-cccc-cccc-cccc-cccccccccccc","version":{"content_revision":4}}`)},
			want: true,
		},
		{
			name: "summary target",
			item: model.ReaderThought{HostKind: "link", HostID: linkID.String(), Target: target(`{"kind":"summary","host_id":"cccccccc-cccc-cccc-cccc-cccccccccccc","version":{"content_revision":4}}`)},
		},
		{
			name: "legacy stale target without revision",
			item: model.ReaderThought{HostKind: "link", HostID: linkID.String(), Target: target(`{"kind":"saved-content","host_id":"cccccccc-cccc-cccc-cccc-cccccccccccc","version":{}}`)},
		},
		{
			name: "previous saved-content revision",
			item: model.ReaderThought{HostKind: "link", HostID: linkID.String(), Target: target(`{"kind":"saved-content","host_id":"cccccccc-cccc-cccc-cccc-cccccccccccc","version":{"content_revision":3}}`)},
		},
		{
			name: "other saved-content host",
			item: model.ReaderThought{HostKind: "link", HostID: linkID.String(), Target: target(`{"kind":"saved-content","host_id":"dddddddd-dddd-dddd-dddd-dddddddddddd","version":{"content_revision":4}}`)},
		},
		{
			name: "deleted current saved-content thought",
			item: model.ReaderThought{HostKind: "link", HostID: linkID.String(), Deleted: true, Target: target(`{"kind":"saved-content","host_id":"cccccccc-cccc-cccc-cccc-cccccccccccc","version":{"content_revision":4}}`)},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isCurrentSavedContentRestoreThought(testCase.item, linkID, 4); got != testCase.want {
				t.Fatalf("isCurrentSavedContentRestoreThought() = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestRestoreContentHistoryInvalidatesDerivedContentInOneTransaction(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	linkID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery("(?s)UPDATE links l SET.*embedding=NULL,embedding_model=NULL.*FROM reader_content_history h.*RETURNING l.content_revision").
		WithArgs(linkID, int64(19), int64(4)).
		WillReturnRows(mock.NewRows([]string{"content_revision", "content"}).AddRow(int64(5), "restored content"))
	mock.ExpectQuery("(?s)SELECT "+regexp.QuoteMeta(readerThoughtColumns)+" FROM reader_thoughts.*reader_thoughts\\.host_kind=\\$1.*reader_thoughts\\.host_id=\\$2.*tombstone\\.thought_id IS NULL.*target #>> '\\{version,content_revision\\}'=\\$3.*FOR UPDATE").
		WithArgs("link", linkID.String(), "4").
		WillReturnRows(mock.NewRows(readerThoughtSyncColumnsForTest()))
	mock.ExpectCommit()

	revision, err := repo.RestoreContentHistory(context.Background(), linkID, 19, 4)
	if err != nil {
		t.Fatalf("RestoreContentHistory() error = %v", err)
	}
	if revision != 5 {
		t.Fatalf("revision = %d, want 5", revision)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreContentHistoryReanchorsUniqueThought(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	linkID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	thoughtID := "thought-content-unique"
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	target := json.RawMessage(`{"kind":"saved-content","host_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","version":{"content_revision":4}}`)
	quote := []byte(`{"exact":"durable phrase","prefix":"before ","suffix":" after","start":7,"end":21}`)

	mock.ExpectBegin()
	mock.ExpectQuery("(?s)UPDATE links l SET.*embedding=NULL,embedding_model=NULL.*FROM reader_content_history h.*RETURNING l.content_revision").
		WithArgs(linkID, int64(19), int64(4)).
		WillReturnRows(mock.NewRows([]string{"content_revision", "content"}).AddRow(int64(5), "before durable phrase after"))
	mock.ExpectQuery("(?s)SELECT "+regexp.QuoteMeta(readerThoughtColumns)+" FROM reader_thoughts.*reader_thoughts\\.host_kind=\\$1.*reader_thoughts\\.host_id=\\$2.*tombstone\\.thought_id IS NULL.*target #>> '\\{version,content_revision\\}'=\\$3.*FOR UPDATE").
		WithArgs("link", linkID.String(), "4").
		WillReturnRows(mock.NewRows(readerThoughtSyncColumnsForTest()).AddRow(
			thoughtID, "link", linkID.String(), &linkID, target, quote,
			"keep this thought", "user", false, int64(7), int64(7), "device-test", "op-test", at, at,
		))
	opID := "content-restore-" + linkID.String() + "-5-" + thoughtID
	expectDerivedThoughtClock(mock, thoughtID, opID, 7)
	mock.ExpectQuery("(?s)INSERT INTO reader_thought_ops.*RETURNING sequence").
		WithArgs(
			opID,
			"reader-content-restore",
			"update",
			thoughtID,
			"link",
			linkID.String(),
			pgxmock.AnyArg(),
			pgxmock.AnyArg(),
			int64(8),
		).
		WillReturnRows(mock.NewRows([]string{"sequence", "created_at"}).AddRow(int64(8), readerThoughtOperationCreatedAt))
	expectThoughtEventPreviousWinner(mock, thoughtID, "link", linkID.String(), 7, 7)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM links WHERE id=$1 AND deleted_at IS NULL)")).
		WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("(?s)INSERT INTO reader_thoughts.*winner_logical_clock.*ON CONFLICT").
		WithArgs(
			thoughtID,
			"link",
			linkID.String(),
			pgxmock.AnyArg(),
			pgxmock.AnyArg(),
			pgxmock.AnyArg(),
			"keep this thought",
			"user",
			false,
			int64(8),
			int64(8),
			"reader-content-restore",
			opID,
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	expectThoughtTodoProjectionRefresh(mock, thoughtID)
	expectThoughtSupersessionEvent(mock, thoughtID, 7, 8)
	mock.ExpectCommit()

	revision, err := repo.RestoreContentHistory(context.Background(), linkID, 19, 4)
	if err != nil {
		t.Fatalf("RestoreContentHistory() error = %v", err)
	}
	if revision != 5 {
		t.Fatalf("revision = %d, want 5", revision)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreContentHistoryKeepsAmbiguousThoughtHistorical(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	linkID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	thoughtID := "thought-content-ambiguous"
	at := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	target := json.RawMessage(`{"kind":"saved-content","host_id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","version":{"content_revision":4}}`)
	quote := []byte(`{"exact":"same phrase","prefix":"","suffix":""}`)

	mock.ExpectBegin()
	mock.ExpectQuery("(?s)UPDATE links l SET.*embedding=NULL,embedding_model=NULL.*FROM reader_content_history h.*RETURNING l.content_revision").
		WithArgs(linkID, int64(19), int64(4)).
		WillReturnRows(mock.NewRows([]string{"content_revision", "content"}).AddRow(int64(5), "same phrase appears; same phrase appears"))
	thoughtRow := []any{
		thoughtID, "link", linkID.String(), &linkID, target, quote,
		"keep historical", "user", false, int64(9), int64(9), "device-test", "op-test", at, at,
	}
	mock.ExpectQuery("(?s)SELECT "+regexp.QuoteMeta(readerThoughtColumns)+" FROM reader_thoughts.*reader_thoughts\\.host_kind=\\$1.*reader_thoughts\\.host_id=\\$2.*tombstone\\.thought_id IS NULL.*target #>> '\\{version,content_revision\\}'=\\$3.*FOR UPDATE").
		WithArgs("link", linkID.String(), "4").
		WillReturnRows(mock.NewRows(readerThoughtSyncColumnsForTest()).AddRow(thoughtRow...))
	expectMarkThoughtLifecycle(
		mock,
		thoughtID,
		thoughtRow,
		"link",
		linkID.String(),
		"content_restored",
		9,
		10,
	)
	mock.ExpectExec("(?s)INSERT INTO reader_thought_tombstones.*FROM reader_thoughts.*id=\\$1").
		WithArgs(thoughtID, "content_restored", int64(10)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	expectThoughtTodoProjectionRefresh(mock, thoughtID)
	mock.ExpectCommit()

	revision, err := repo.RestoreContentHistory(context.Background(), linkID, 19, 4)
	if err != nil {
		t.Fatalf("RestoreContentHistory() error = %v", err)
	}
	if revision != 5 {
		t.Fatalf("revision = %d, want 5", revision)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreContentHistoryRollsBackWhenEligibleThoughtWriteFails(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	linkID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	thoughtID := "thought-content-write-failure"
	at := time.Date(2026, 8, 10, 13, 0, 0, 0, time.UTC)
	target := json.RawMessage(`{"kind":"saved-content","host_id":"eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee","version":{"content_revision":4}}`)
	quote := []byte(`{"exact":"durable phrase","prefix":"before ","suffix":" after","start":7,"end":21}`)

	mock.ExpectBegin()
	mock.ExpectQuery("(?s)UPDATE links l SET.*embedding=NULL,embedding_model=NULL.*FROM reader_content_history h.*RETURNING l.content_revision").
		WithArgs(linkID, int64(19), int64(4)).
		WillReturnRows(mock.NewRows([]string{"content_revision", "content"}).AddRow(int64(5), "before durable phrase after"))
	mock.ExpectQuery("(?s)SELECT "+regexp.QuoteMeta(readerThoughtColumns)+" FROM reader_thoughts.*reader_thoughts\\.host_kind=\\$1.*reader_thoughts\\.host_id=\\$2.*tombstone\\.thought_id IS NULL.*target #>> '\\{version,content_revision\\}'=\\$3.*FOR UPDATE").
		WithArgs("link", linkID.String(), "4").
		WillReturnRows(mock.NewRows(readerThoughtSyncColumnsForTest()).AddRow(
			thoughtID, "link", linkID.String(), &linkID, target, quote,
			"keep this thought", "user", false, int64(7), int64(7), "device-test", "op-test", at, at,
		))
	opID := "content-restore-" + linkID.String() + "-5-" + thoughtID
	expectDerivedThoughtClock(mock, thoughtID, opID, 7)
	mock.ExpectQuery("(?s)INSERT INTO reader_thought_ops.*RETURNING sequence").
		WithArgs(
			opID, "reader-content-restore", "update", thoughtID,
			"link", linkID.String(), pgxmock.AnyArg(), pgxmock.AnyArg(), int64(8),
		).
		WillReturnError(errors.New("injected thought operation failure"))
	mock.ExpectRollback()

	_, err = repo.RestoreContentHistory(context.Background(), linkID, 19, 4)
	if err == nil {
		t.Fatal("RestoreContentHistory() error = nil, want injected thought operation failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreContentHistoryConflictRollsBackBeforeInvalidation(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderVNextRepository(mock)
	linkID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery("(?s)UPDATE links l SET.*FROM reader_content_history h.*RETURNING l.content_revision").
		WithArgs(linkID, int64(19), int64(4)).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	_, err = repo.RestoreContentHistory(context.Background(), linkID, 19, 4)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("RestoreContentHistory() error = %v, want ErrRevisionConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
