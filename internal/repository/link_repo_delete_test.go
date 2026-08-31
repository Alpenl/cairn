package repository

import (
	"errors"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"
)

func TestDeleteLifecycleTerminalizesTranslationsBeforeSoftDelete(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	linkID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockLinkForDeleteSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(linkID))
	expectNoLinkThoughtHostTombstones(mock, linkID)
	mock.ExpectExec(regexp.QuoteMeta(terminalizeDeletedTranslationAttemptsSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(deleteLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	expectLinkThoughtTodoProjectionRefresh(mock, linkID)
	mock.ExpectCommit()

	if err := NewPGXLinkRepository(mock).DeleteLifecycle(t.Context(), linkID); err != nil {
		t.Fatalf("DeleteLifecycle() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteLifecycleRollsBackWhenTranslationTerminalizationFails(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	linkID := uuid.New()
	wantErr := errors.New("translation attempt write failed")
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockLinkForDeleteSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(linkID))
	expectNoLinkThoughtHostTombstones(mock, linkID)
	mock.ExpectExec(regexp.QuoteMeta(terminalizeDeletedTranslationAttemptsSQL)).WithArgs(linkID).
		WillReturnError(wantErr)
	mock.ExpectRollback()

	if err := NewPGXLinkRepository(mock).DeleteLifecycle(t.Context(), linkID); !errors.Is(err, wantErr) {
		t.Fatalf("DeleteLifecycle() error = %v, want %v", err, wantErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
