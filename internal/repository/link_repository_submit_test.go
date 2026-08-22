package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func submittedLinkArgs(url string) []any {
	var (
		nilString *string
		nilInt    *int
		nilKind   *model.LibraryKind
	)
	return []any{
		url, "url", url,
		nilString, nilString, nilString,
		nil, nil, nilString,
		model.LinkStatusPending,
		nilString, nilString,
		nilKind, false,
		nilInt, nilString, nil,
	}
}

func submittedLinkRow(id uuid.UUID, url string, status model.LinkStatus, createdAt time.Time) []any {
	return []any{
		id, url, "url", url,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, false, nil,
		status, nil, nil, nil, nil,
		nil, false, int64(1), int64(1), int64(1),
		string(model.ContentSourceFetched), false, 0, 0,
		createdAt, nil, nil, nil,
		nil, nil, nil,
		createdAt, createdAt,
	}
}

func submittedLinkColumns() []string {
	return append(append([]string(nil), linkColumns()...), "trashed")
}

func submittedExistingRow(id uuid.UUID, url string, status model.LinkStatus, createdAt time.Time, trashed bool) []any {
	return append(submittedLinkRow(id, url, status, createdAt), trashed)
}

func submitTxForTest(t *testing.T, mock pgxmock.PgxPoolIface, repo *PGXLinkRepository, params CreateLinkParams) LinkSubmitResult {
	t.Helper()
	tx, err := mock.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result, err := repo.SubmitTx(t.Context(), tx, params)
	if err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("SubmitTx() error = %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestLinkRepositorySubmitTxInsertsFreshLinkAndAttempt(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	linkID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	url := "https://example.com/fresh"
	now := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmittedLinkSQL)).
		WithArgs(url, url).
		WillReturnRows(mock.NewRows(submittedLinkColumns()))
	mock.ExpectQuery(regexp.QuoteMeta(insertSubmittedLinkSQL)).
		WithArgs(submittedLinkArgs(url)...).
		WillReturnRows(mock.NewRows(linkColumns()).AddRow(submittedLinkRow(linkID, url, model.LinkStatusPending, now)...))
	mock.ExpectCommit()

	result := submitTxForTest(t, mock, NewPGXLinkRepository(mock), CreateLinkParams{URL: url, Status: model.LinkStatusPending})
	if result.Link == nil || result.Link.ID != linkID || result.Attempt == nil {
		t.Fatalf("SubmitTx() = %#v, want fresh Link and attempt", result)
	}
	if result.Attempt.LinkID != linkID || result.Attempt.Generation != 1 || result.Attempt.ExpectedMetadataRevision != 1 {
		t.Fatalf("attempt = %#v, want generation/revision 1", result.Attempt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLinkRepositorySubmitTxReusesExistingIdentityWithoutAttempt(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	linkID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	url := "https://example.com/existing"
	now := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmittedLinkSQL)).
		WithArgs(url, url).
		WillReturnRows(mock.NewRows(submittedLinkColumns()).AddRow(submittedExistingRow(linkID, url, model.LinkStatusDone, now, false)...))
	mock.ExpectExec(regexp.QuoteMeta(adoptSubmittedLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectCommit()

	result := submitTxForTest(t, mock, NewPGXLinkRepository(mock), CreateLinkParams{URL: url, Status: model.LinkStatusPending})
	if result.Link == nil || result.Link.ID != linkID || result.Attempt != nil || result.Restored {
		t.Fatalf("SubmitTx() = %#v, want existing Link without attempt", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLinkRepositorySubmitTxRestoresTerminalTrashWithoutReparse(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	linkID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	url := "https://example.com/trashed-done"
	now := time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmittedLinkSQL)).
		WithArgs(url, url).
		WillReturnRows(mock.NewRows(submittedLinkColumns()).AddRow(submittedExistingRow(linkID, url, model.LinkStatusDone, now, true)...))
	mock.ExpectQuery(regexp.QuoteMeta(lockLinkForRestoreSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"status", "deleted_at", "body", "content_revision"}).AddRow(model.LinkStatusDone, now, "body", int64(3)))
	mock.ExpectExec("UPDATE links SET deleted_at=NULL").WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	expectEmptyLinkThoughtRestore(mock, linkID)
	mock.ExpectExec(regexp.QuoteMeta(adoptSubmittedLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectCommit()

	result := submitTxForTest(t, mock, NewPGXLinkRepository(mock), CreateLinkParams{URL: url, Status: model.LinkStatusPending})
	if result.Link == nil || result.Link.ID != linkID || !result.Restored || result.Attempt != nil {
		t.Fatalf("SubmitTx() = %#v, want restored terminal Link without attempt", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLinkRepositorySubmitTxRestartsRestoredInflightLink(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	linkID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	url := "https://example.com/trashed-pending"
	now := time.Date(2026, 8, 21, 11, 30, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmittedLinkSQL)).
		WithArgs(url, url).
		WillReturnRows(mock.NewRows(submittedLinkColumns()).AddRow(submittedExistingRow(linkID, url, model.LinkStatusPending, now, true)...))
	mock.ExpectQuery(regexp.QuoteMeta(lockLinkForRestoreSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"status", "deleted_at", "body", "content_revision"}).AddRow(model.LinkStatusPending, now, "", int64(1)))
	mock.ExpectExec("UPDATE links SET deleted_at=NULL").WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	expectEmptyLinkThoughtRestore(mock, linkID)
	mock.ExpectExec(regexp.QuoteMeta(adoptSubmittedLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectQuery(regexp.QuoteMeta(restartRestoredLinkSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"parse_generation", "metadata_revision"}).AddRow(int64(2), int64(1)))
	mock.ExpectCommit()

	result := submitTxForTest(t, mock, NewPGXLinkRepository(mock), CreateLinkParams{URL: url, Status: model.LinkStatusPending})
	if result.Link == nil || !result.Restored || result.Attempt == nil || result.Attempt.Generation != 2 {
		t.Fatalf("SubmitTx() = %#v, want restored Link with replacement attempt", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
