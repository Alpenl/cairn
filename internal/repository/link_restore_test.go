package repository

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func expectEmptyLinkThoughtRestore(mock pgxmock.PgxPoolIface, linkID uuid.UUID) {
	mock.ExpectQuery("(?s)SELECT .*FROM reader_thought_tombstones tt.*tt.host_kind=\\$1.*tt.host_id=\\$2.*tt.reason=\\$3.*FOR UPDATE OF reader_thoughts").
		WithArgs(model.ReaderHostLink, linkID.String(), "link_deleted").
		WillReturnRows(mock.NewRows([]string{"id"}))
}

func TestRestoreHostLinkCommitsLinkAndThoughtLifecycleTogether(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	linkID := uuid.New()
	deletedAt := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockLinkForRestoreSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"status", "deleted_at", "body", "content_revision", "feed_managed"}).
			AddRow(model.LinkStatusDone, deletedAt, "stable body", int64(7), true))
	mock.ExpectExec("UPDATE links SET deleted_at=NULL").WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	expectEmptyLinkThoughtRestore(mock, linkID)
	mock.ExpectExec(regexp.QuoteMeta(adoptSubmittedLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	result, err := NewPGXReaderVNextRepository(mock).RestoreHost(t.Context(), model.ReaderHostLink, linkID)
	if err != nil || !result.Changed || result.State != model.ReaderHostLive {
		t.Fatalf("RestoreHost() = %+v, %v", result, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreHostLinkRollsBackWhenThoughtRestoreFails(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	linkID := uuid.New()
	wantErr := errors.New("thought restore failed")
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockLinkForRestoreSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"status", "deleted_at", "body", "content_revision", "feed_managed"}).
			AddRow(model.LinkStatusDone, time.Now(), "stable body", int64(2), false))
	mock.ExpectExec("UPDATE links SET deleted_at=NULL").WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery("(?s)SELECT .*FROM reader_thought_tombstones tt").
		WithArgs(model.ReaderHostLink, linkID.String(), "link_deleted").
		WillReturnError(wantErr)
	mock.ExpectRollback()

	_, err = NewPGXReaderVNextRepository(mock).RestoreHost(t.Context(), model.ReaderHostLink, linkID)
	if !errors.Is(err, wantErr) {
		t.Fatalf("RestoreHost() error = %v, want %v", err, wantErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
