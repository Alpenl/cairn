package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestReaderContentHistoryCleanupRunsOneTransaction(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderContentHistoryCleanupRepository(mock)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT public.reader_cleanup_content_history($1,$2)`)).
		WithArgs(37, 20).
		WillReturnRows(mock.NewRows([]string{"deleted"}).AddRow(12))
	mock.ExpectCommit()

	deleted, err := repo.CleanupContentHistoryBatch(context.Background(), 37)
	if err != nil {
		t.Fatalf("CleanupContentHistoryBatch() error = %v", err)
	}
	if deleted != 12 {
		t.Fatalf("CleanupContentHistoryBatch() deleted = %d, want 12", deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReaderContentHistoryCleanupCapsTransactionAtOneHundred(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXReaderContentHistoryCleanupRepository(mock)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT public.reader_cleanup_content_history($1,$2)`)).
		WithArgs(100, 20).
		WillReturnRows(mock.NewRows([]string{"deleted"}).AddRow(100))
	mock.ExpectCommit()

	deleted, err := repo.CleanupContentHistoryBatch(context.Background(), 1000)
	if err != nil {
		t.Fatalf("CleanupContentHistoryBatch() error = %v", err)
	}
	if deleted != 100 {
		t.Fatalf("CleanupContentHistoryBatch() deleted = %d, want 100", deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReaderContentHistoryCleanupRollsBackFailedBatch(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	wantErr := errors.New("database unavailable")
	repo := NewPGXReaderContentHistoryCleanupRepository(mock)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT public.reader_cleanup_content_history($1,$2)`)).
		WithArgs(100, 20).
		WillReturnError(wantErr)
	mock.ExpectRollback()

	_, err = repo.CleanupContentHistoryBatch(context.Background(), 100)
	if !errors.Is(err, wantErr) {
		t.Fatalf("CleanupContentHistoryBatch() error = %v, want wrapped database error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
