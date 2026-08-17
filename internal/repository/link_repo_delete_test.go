package repository

import (
	"errors"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"
)

func TestDeleteLifecyclePrelocksLibraryAndFeedBeforeLink(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	prelockErr := errors.New("library/feed prelock rejected")
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT lock_library_feed_revisions()")).
		WillReturnError(prelockErr)
	mock.ExpectRollback()

	repo := NewPGXLinkRepository(mock)
	if err := repo.DeleteLifecycle(t.Context(), uuid.New()); !errors.Is(err, prelockErr) {
		t.Fatalf("DeleteLifecycle() error = %v, want prelock error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestDeleteLifecycleTerminalizesAttemptsBeforeSoftDelete(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	linkID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(lockLibraryFeedRevisionsSQL)).WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectQuery(regexp.QuoteMeta(lockLinkForDeleteSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(linkID))
	mock.ExpectExec(regexp.QuoteMeta(terminalizeDeletedParseAttemptsSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	mock.ExpectExec(regexp.QuoteMeta(terminalizeDeletedTranslationAttemptsSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(deleteLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	expectLinkThoughtTodoProjectionRefresh(mock, linkID, "thought-on-deleted-link")
	mock.ExpectCommit()

	if err := NewPGXLinkRepository(mock).DeleteLifecycle(t.Context(), linkID); err != nil {
		t.Fatalf("DeleteLifecycle() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteLifecycleRollsBackWhenAttemptTerminalizationFails(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	linkID := uuid.New()
	wantErr := errors.New("parse attempt write failed")
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(lockLibraryFeedRevisionsSQL)).WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectQuery(regexp.QuoteMeta(lockLinkForDeleteSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(linkID))
	mock.ExpectExec(regexp.QuoteMeta(terminalizeDeletedParseAttemptsSQL)).WithArgs(linkID).
		WillReturnError(wantErr)
	mock.ExpectRollback()

	if err := NewPGXLinkRepository(mock).DeleteLifecycle(t.Context(), linkID); !errors.Is(err, wantErr) {
		t.Fatalf("DeleteLifecycle() error = %v, want %v", err, wantErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
