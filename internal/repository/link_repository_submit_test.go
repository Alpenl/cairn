package repository

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

// expectNoCanonicalURLIdentities stands in for an installation whose backfill found no
// colliding legacy rows: the submit path still asks which link owns each
// normalized identity, and gets nothing back. These pgxmock tests only pin the
// statement shape and ordering; the behaviour of a populated mapping is covered
// against a real PostgreSQL in test/dbintegration/url_identity_test.go.
func expectNoCanonicalURLIdentities(mock pgxmock.PgxPoolIface, normalizedURLs []string) {
	mock.ExpectQuery(regexp.QuoteMeta(selectCanonicalURLIdentitiesSQL)).
		WithArgs(normalizedURLs).
		WillReturnRows(mock.NewRows([]string{"normalized_url", "link_id"}))
}

// TestLinkRepositorySubmitNewCommitsLinkAndJobInOneTransaction locks in the
// M7 contract: a fresh-link submission inserts the links row and the
// initial parse_jobs row inside a single transaction so a partial failure
// (e.g. job INSERT panics) cannot leave an orphan link with no job.
func TestLinkRepositorySubmitNewCommitsLinkAndJobInOneTransaction(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXLinkRepository(mock)

	linkID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	jobID := uuid.MustParse("66666666-7777-8888-9999-aaaaaaaaaaaa")
	createdAt := time.Date(2026, 5, 3, 10, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Minute)

	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	expectNoCanonicalURLIdentities(mock, []string{"https://example.com/tx"})
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmitLinksSQL)).
		WithArgs([]string{"https://example.com/tx"}, []string{"https://example.com/tx"}, []uuid.UUID{}).
		WillReturnRows(mock.NewRows(submitExistingColumns()))
	linksSQL, _ := buildBatchInsertLinksSQL(1)
	mock.ExpectQuery(regexp.QuoteMeta(linksSQL)).
		WithArgs(buildBatchLinkArgs("https://example.com/tx")...).
		WillReturnRows(
			mock.NewRows(linkColumns()).AddRow(buildBatchLinkRow(linkID, "https://example.com/tx", model.LinkStatusPending, createdAt, updatedAt)...),
		)
	mock.ExpectQuery(regexp.QuoteMeta(buildBatchInsertJobsSQL(1))).
		WithArgs(linkID).
		WillReturnRows(
			mock.NewRows(jobColumns()).AddRow(
				jobID, linkID, model.JobStatusPending, nil, createdAt, updatedAt, int64(1),
			),
		)
	mock.ExpectCommit()

	link, job, err := repo.SubmitNew(context.Background(), CreateLinkParams{
		URL:    "https://example.com/tx",
		Status: model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitNew() error = %v", err)
	}
	if link == nil || link.ID != linkID {
		t.Fatalf("SubmitNew link = %#v, want id %s", link, linkID)
	}
	if job == nil || job.ID != jobID || job.LinkID != linkID {
		t.Fatalf("SubmitNew job = %#v, want id %s linkID %s", job, jobID, linkID)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

// TestLinkRepositorySubmitNewRollsBackOnJobInsertFailure verifies that a
// failure on the second INSERT triggers a rollback rather than leaving the
// link committed with no job. ExpectRollback is what guarantees the
// transaction was aborted.
func TestLinkRepositorySubmitNewRollsBackOnJobInsertFailure(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXLinkRepository(mock)
	linkID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	createdAt := time.Date(2026, 5, 3, 11, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	expectNoCanonicalURLIdentities(mock, []string{"https://example.com/rollback"})
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmitLinksSQL)).
		WithArgs([]string{"https://example.com/rollback"}, []string{"https://example.com/rollback"}, []uuid.UUID{}).
		WillReturnRows(mock.NewRows(submitExistingColumns()))
	linksSQL, _ := buildBatchInsertLinksSQL(1)
	mock.ExpectQuery(regexp.QuoteMeta(linksSQL)).
		WithArgs(buildBatchLinkArgs("https://example.com/rollback")...).
		WillReturnRows(
			mock.NewRows(linkColumns()).AddRow(buildBatchLinkRow(linkID, "https://example.com/rollback", model.LinkStatusPending, createdAt, createdAt)...),
		)
	mock.ExpectQuery(regexp.QuoteMeta(buildBatchInsertJobsSQL(1))).
		WithArgs(linkID).
		WillReturnError(errors.New("simulated job insert failure"))
	mock.ExpectRollback()

	_, _, err = repo.SubmitNew(context.Background(), CreateLinkParams{
		URL:    "https://example.com/rollback",
		Status: model.LinkStatusPending,
	})
	if err == nil {
		t.Fatal("SubmitNew() error = nil, want job insert failure")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

// buildBatchLinkArgs returns the typed-nil arg list a single
// SubmitBatch row emits for a URL-only CreateLinkParams. Centralised
// because pgxmock's matcher distinguishes typed nil (*string)(nil) from
// the untyped nil literal, so the tests below would otherwise repeat
// the same typed-nil expectation block throughout the batch tests.
func buildBatchLinkArgs(url string) []any {
	var (
		nilString *string
		nilInt    *int
		nilKind   *model.LibraryKind
		nilSource *model.LibraryKindSource
	)
	return []any{
		url, "url", url,
		nilString, nilString, nilString,
		nil, nil, nilString,
		model.LinkStatusPending,
		nilString, nilString,
		model.RequestedLibraryKindAuto,
		model.RequestedLibraryKindSourceAuto,
		nilKind, nilSource, false,
		nilKind,
		nilInt, nilString, nil,
	}
}

// buildBatchLinkRow returns the pgxmock row payload for a single link returned
// by either SubmitBatch's INSERT RETURNING or its conflict follow-up SELECT.
func buildBatchLinkRow(id uuid.UUID, url string, status model.LinkStatus, createdAt, updatedAt time.Time) []any {
	return []any{
		id, url, "url", url,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, false, nil,
		status, nil, nil, nil, nil,
		string(model.RequestedLibraryKindAuto), string(model.RequestedLibraryKindSourceAuto),
		nil, nil, false, nil, nil, nil, nil, nil, int64(1), int64(1),
		string(model.ContentSourceFetched),
		// PF6：content_revision 之后是 content_source / has_content / content_cjk_chars / content_words。
		false, 0, 0,
		createdAt, nil, nil, nil,
		nil, nil, nil,
		createdAt, updatedAt,
	}
}

func submitExistingColumns() []string {
	return append(append([]string(nil), linkColumns()...), "trashed")
}

func buildSubmitExistingRow(id uuid.UUID, url string, status model.LinkStatus, createdAt, updatedAt time.Time, trashed bool) []any {
	return append(buildBatchLinkRow(id, url, status, createdAt, updatedAt), trashed)
}

// TestLinkRepository_SubmitBatch_InsertsAllNewRowsInSingleTransaction
// is the happy-path: two distinct URLs, both fresh, return Inserted=
// true with a matching parse_jobs row each — all inside one tx with
// exactly two QueryRow round-trips (links + jobs).
func TestLinkRepository_SubmitBatch_InsertsAllNewRowsInSingleTransaction(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXLinkRepository(mock)

	firstID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	secondID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	firstJobID := uuid.MustParse("aaaa1111-1111-1111-1111-111111111111")
	secondJobID := uuid.MustParse("bbbb2222-2222-2222-2222-222222222222")
	createdAt := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Minute)

	expectedLinksSQL, _ := buildBatchInsertLinksSQL(2)
	expectedJobsSQL := buildBatchInsertJobsSQL(2)

	args := append(buildBatchLinkArgs("https://example.com/a"), buildBatchLinkArgs("https://example.com/b")...)

	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	items := []CreateLinkParams{
		{URL: "https://example.com/a", Status: model.LinkStatusPending},
		{URL: "https://example.com/b", Status: model.LinkStatusPending},
	}
	expectNoCanonicalURLIdentities(mock, []string{"https://example.com/a", "https://example.com/b"})
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmitLinksSQL)).
		WithArgs(
			[]string{"https://example.com/a", "https://example.com/b"},
			[]string{"https://example.com/a", "https://example.com/b"},
			[]uuid.UUID{},
		).
		WillReturnRows(mock.NewRows(submitExistingColumns()))
	mock.ExpectQuery(regexp.QuoteMeta(expectedLinksSQL)).
		WithArgs(args...).
		WillReturnRows(
			mock.NewRows(linkColumns()).
				AddRow(buildBatchLinkRow(firstID, "https://example.com/a", model.LinkStatusPending, createdAt, updatedAt)...).
				AddRow(buildBatchLinkRow(secondID, "https://example.com/b", model.LinkStatusPending, createdAt, updatedAt)...),
		)
	mock.ExpectQuery(regexp.QuoteMeta(expectedJobsSQL)).
		WithArgs(firstID, secondID).
		WillReturnRows(
			mock.NewRows(jobColumns()).
				AddRow(firstJobID, firstID, model.JobStatusPending, nil, createdAt, updatedAt, int64(1)).
				AddRow(secondJobID, secondID, model.JobStatusPending, nil, createdAt, updatedAt, int64(1)),
		)
	mock.ExpectCommit()

	results, err := repo.SubmitBatch(context.Background(), items)
	if err != nil {
		t.Fatalf("SubmitBatch() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	for i, res := range results {
		if !res.Inserted {
			t.Fatalf("results[%d].Inserted = false, want true", i)
		}
		if res.Link == nil || res.Job == nil {
			t.Fatalf("results[%d] link=%v job=%v, want both non-nil", i, res.Link, res.Job)
		}
		if res.Job.LinkID != res.Link.ID {
			t.Fatalf("results[%d] job.LinkID = %s, want %s", i, res.Job.LinkID, res.Link.ID)
		}
	}
	if results[0].Link.ID != firstID || results[1].Link.ID != secondID {
		t.Fatalf("link order mismatch: got %s,%s want %s,%s",
			results[0].Link.ID, results[1].Link.ID, firstID, secondID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

// TestLinkRepository_SubmitBatch_HandlesAllExistingRowsWithoutJobInsert
// verifies that conflicts omitted from INSERT RETURNING are fetched in one
// SELECT and the repo skips parse_jobs insertion entirely. This is the load
// case where /api/links/batch re-submits URLs the user already saved.
func TestLinkRepository_SubmitBatch_HandlesAllExistingRowsWithoutJobInsert(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXLinkRepository(mock)

	firstID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	secondID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	createdAt := time.Date(2026, 5, 16, 11, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	items := []CreateLinkParams{
		{URL: "https://example.com/c", Status: model.LinkStatusPending},
		{URL: "https://example.com/d", Status: model.LinkStatusPending},
	}
	expectNoCanonicalURLIdentities(mock, []string{"https://example.com/c", "https://example.com/d"})
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmitLinksSQL)).
		WithArgs(
			[]string{"https://example.com/c", "https://example.com/d"},
			[]string{"https://example.com/c", "https://example.com/d"},
			[]uuid.UUID{},
		).
		WillReturnRows(
			mock.NewRows(submitExistingColumns()).
				AddRow(buildSubmitExistingRow(firstID, "https://example.com/c", model.LinkStatusDone, createdAt, createdAt, false)...).
				AddRow(buildSubmitExistingRow(secondID, "https://example.com/d", model.LinkStatusDone, createdAt, createdAt, false)...),
		)
	mock.ExpectExec(regexp.QuoteMeta(adoptSubmittedLinkSQL)).WithArgs(firstID).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(regexp.QuoteMeta(adoptSubmittedLinkSQL)).WithArgs(secondID).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectCommit()

	results, err := repo.SubmitBatch(context.Background(), items)
	if err != nil {
		t.Fatalf("SubmitBatch() error = %v", err)
	}
	for i, res := range results {
		if res.Inserted {
			t.Fatalf("results[%d].Inserted = true, want false (existing row)", i)
		}
		if res.Job != nil {
			t.Fatalf("results[%d].Job = %#v, want nil for existing row", i, res.Job)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestLinkRepositorySubmitBatchLegacyDuplicateFallbackKeepsLowestID(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	url := "https://example.com/legacy-duplicate"
	lowestID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	higherID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	createdAt := time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC)

	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	expectNoCanonicalURLIdentities(mock, []string{url})
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmitLinksSQL)).
		WithArgs([]string{url}, []string{url}, []uuid.UUID{}).
		WillReturnRows(
			mock.NewRows(submitExistingColumns()).
				AddRow(buildSubmitExistingRow(lowestID, url, model.LinkStatusDone, createdAt, createdAt, false)...).
				AddRow(buildSubmitExistingRow(higherID, url, model.LinkStatusDone, createdAt, createdAt, false)...),
		)
	mock.ExpectExec(regexp.QuoteMeta(adoptSubmittedLinkSQL)).WithArgs(lowestID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectCommit()

	results, err := NewPGXLinkRepository(mock).SubmitBatch(t.Context(), []CreateLinkParams{{
		URL:    url,
		Status: model.LinkStatusPending,
	}})
	if err != nil {
		t.Fatalf("SubmitBatch() error = %v", err)
	}
	if len(results) != 1 || results[0].Link == nil || results[0].Link.ID != lowestID {
		t.Fatalf("SubmitBatch() = %#v, want lowest legacy duplicate %s", results, lowestID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

// TestLinkRepository_SubmitBatch_MixedInsertedAndExistingResultsPreserveInputOrder
// is the regression test for the ORDER guarantee across the INSERT RETURNING
// and conflict SELECT result streams. The repo must rebuild results in input
// order so callers never attribute an outcome to the wrong item.
func TestLinkRepository_SubmitBatch_MixedInsertedAndExistingResultsPreserveInputOrder(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXLinkRepository(mock)

	freshID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	existingID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	freshJobID := uuid.MustParse("cccc5555-5555-5555-5555-555555555555")
	createdAt := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	expectedLinksSQL, _ := buildBatchInsertLinksSQL(1)
	expectedJobsSQL := buildBatchInsertJobsSQL(1)

	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	items := []CreateLinkParams{
		{URL: "https://example.com/fresh", Status: model.LinkStatusPending},
		{URL: "https://example.com/existing", Status: model.LinkStatusPending},
	}
	expectNoCanonicalURLIdentities(mock, []string{"https://example.com/fresh", "https://example.com/existing"})
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmitLinksSQL)).
		WithArgs(
			[]string{"https://example.com/fresh", "https://example.com/existing"},
			[]string{"https://example.com/fresh", "https://example.com/existing"},
			[]uuid.UUID{},
		).
		WillReturnRows(
			mock.NewRows(submitExistingColumns()).
				AddRow(buildSubmitExistingRow(existingID, "https://example.com/existing", model.LinkStatusDone, createdAt, createdAt, false)...),
		)
	mock.ExpectExec(regexp.QuoteMeta(adoptSubmittedLinkSQL)).WithArgs(existingID).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectQuery(regexp.QuoteMeta(expectedLinksSQL)).
		WithArgs(buildBatchLinkArgs("https://example.com/fresh")...).
		WillReturnRows(
			mock.NewRows(linkColumns()).
				AddRow(buildBatchLinkRow(freshID, "https://example.com/fresh", model.LinkStatusPending, createdAt, createdAt)...),
		)
	mock.ExpectQuery(regexp.QuoteMeta(expectedJobsSQL)).
		WithArgs(freshID).
		WillReturnRows(
			mock.NewRows(jobColumns()).
				AddRow(freshJobID, freshID, model.JobStatusPending, nil, createdAt, createdAt, int64(1)),
		)
	mock.ExpectCommit()

	results, err := repo.SubmitBatch(context.Background(), items)
	if err != nil {
		t.Fatalf("SubmitBatch() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Link.ID != freshID || !results[0].Inserted || results[0].Job == nil {
		t.Fatalf("results[0] = %#v, want fresh inserted link with job", results[0])
	}
	if results[1].Link.ID != existingID || results[1].Inserted || results[1].Job != nil {
		t.Fatalf("results[1] = %#v, want existing link without job", results[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestSubmitBatchRestoresTerminalTrashLinkWithoutImplicitReparse(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	linkID := uuid.New()
	url := "https://example.com/trashed-done"
	now := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	expectNoCanonicalURLIdentities(mock, []string{url})
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmitLinksSQL)).
		WithArgs([]string{url}, []string{url}, []uuid.UUID{}).
		WillReturnRows(mock.NewRows(submitExistingColumns()).
			AddRow(buildSubmitExistingRow(linkID, url, model.LinkStatusDone, now, now, true)...))
	mock.ExpectQuery(regexp.QuoteMeta(lockLinkForRestoreSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"status", "deleted_at", "body", "content_revision", "feed_managed"}).
			AddRow(model.LinkStatusDone, now, "restorable content", int64(5), false))
	mock.ExpectExec("UPDATE links SET deleted_at=NULL").WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	expectEmptyLinkThoughtRestore(mock, linkID)
	mock.ExpectExec(regexp.QuoteMeta(adoptSubmittedLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectCommit()

	results, err := NewPGXLinkRepository(mock).SubmitBatch(t.Context(), []CreateLinkParams{{URL: url, Status: model.LinkStatusPending}})
	if err != nil || len(results) != 1 || results[0].Link == nil || results[0].Link.ID != linkID || !results[0].Restored || results[0].Inserted || results[0].Job != nil {
		t.Fatalf("SubmitBatch() = %#v, %v", results, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitBatchRestartsRestoredInflightLinkWithOneReplacementAttempt(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	linkID, jobID := uuid.New(), uuid.New()
	url := "https://example.com/trashed-pending"
	now := time.Date(2026, 8, 14, 8, 30, 0, 0, time.UTC)
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	expectNoCanonicalURLIdentities(mock, []string{url})
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmitLinksSQL)).
		WithArgs([]string{url}, []string{url}, []uuid.UUID{}).
		WillReturnRows(mock.NewRows(submitExistingColumns()).
			AddRow(buildSubmitExistingRow(linkID, url, model.LinkStatusPending, now, now, true)...))
	mock.ExpectQuery(regexp.QuoteMeta(lockLinkForRestoreSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"status", "deleted_at", "body", "content_revision", "feed_managed"}).
			AddRow(model.LinkStatusPending, now, "", int64(1), false))
	mock.ExpectExec("UPDATE links SET deleted_at=NULL").WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	expectEmptyLinkThoughtRestore(mock, linkID)
	mock.ExpectExec(regexp.QuoteMeta(adoptSubmittedLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(regexp.QuoteMeta(supersedeParseJobsSQL)).WithArgs(linkID, linkRestoredAttemptMessage).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(restartRestoredLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(regexp.QuoteMeta(insertJobSQL)).WithArgs(linkID, parseJobsPerLinkRetention).
		WillReturnRows(mock.NewRows(jobColumns()).AddRow(jobID, linkID, model.JobStatusPending, nil, now, now, int64(1)))
	mock.ExpectCommit()

	results, err := NewPGXLinkRepository(mock).SubmitBatch(t.Context(), []CreateLinkParams{{URL: url, Status: model.LinkStatusPending}})
	if err != nil || len(results) != 1 || results[0].Job == nil || results[0].Job.ID != jobID || !results[0].Restored || results[0].Inserted || results[0].Link.Status != model.LinkStatusPending {
		t.Fatalf("SubmitBatch() = %#v, %v", results, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitBatchDuplicateTrashInputsShareOneReplacementAttempt(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	linkID, jobID := uuid.New(), uuid.New()
	url := "https://example.com/trashed-pending-duplicate"
	now := time.Date(2026, 8, 14, 8, 45, 0, 0, time.UTC)
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	expectNoCanonicalURLIdentities(mock, []string{url})
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmitLinksSQL)).
		WithArgs([]string{url, url}, []string{url, url}, []uuid.UUID{}).
		WillReturnRows(mock.NewRows(submitExistingColumns()).
			AddRow(buildSubmitExistingRow(linkID, url, model.LinkStatusPending, now, now, true)...))
	mock.ExpectQuery(regexp.QuoteMeta(lockLinkForRestoreSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"status", "deleted_at", "body", "content_revision", "feed_managed"}).
			AddRow(model.LinkStatusPending, now, "", int64(1), false))
	mock.ExpectExec("UPDATE links SET deleted_at=NULL").WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	expectEmptyLinkThoughtRestore(mock, linkID)
	mock.ExpectExec(regexp.QuoteMeta(adoptSubmittedLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(regexp.QuoteMeta(supersedeParseJobsSQL)).WithArgs(linkID, linkRestoredAttemptMessage).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(restartRestoredLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(regexp.QuoteMeta(insertJobSQL)).WithArgs(linkID, parseJobsPerLinkRetention).
		WillReturnRows(mock.NewRows(jobColumns()).AddRow(jobID, linkID, model.JobStatusPending, nil, now, now, int64(1)))
	mock.ExpectCommit()

	results, err := NewPGXLinkRepository(mock).SubmitBatch(t.Context(), []CreateLinkParams{
		{URL: url, Status: model.LinkStatusPending},
		{URL: url, Status: model.LinkStatusPending},
	})
	if err != nil {
		t.Fatalf("SubmitBatch() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	for i, result := range results {
		if result.Link == nil || result.Link.ID != linkID || result.Job == nil || result.Job.ID != jobID || !result.Restored || result.Inserted {
			t.Fatalf("results[%d] = %#v, want shared restored Link and replacement attempt", i, result)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitBatchRollsBackTrashRestoreWhenAnotherItemFails(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	linkID := uuid.New()
	restoreURL := "https://example.com/trashed-mixed"
	invalidURL := "https://example.com/invalid-metadata"
	now := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	expectNoCanonicalURLIdentities(mock, []string{restoreURL, invalidURL})
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmitLinksSQL)).
		WithArgs([]string{restoreURL, invalidURL}, []string{restoreURL, invalidURL}, []uuid.UUID{}).
		WillReturnRows(mock.NewRows(submitExistingColumns()).
			AddRow(buildSubmitExistingRow(linkID, restoreURL, model.LinkStatusDone, now, now, true)...))
	mock.ExpectQuery(regexp.QuoteMeta(lockLinkForRestoreSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"status", "deleted_at", "body", "content_revision", "feed_managed"}).
			AddRow(model.LinkStatusDone, now, "restorable content", int64(3), false))
	mock.ExpectExec("UPDATE links SET deleted_at=NULL").WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	expectEmptyLinkThoughtRestore(mock, linkID)
	mock.ExpectExec(regexp.QuoteMeta(adoptSubmittedLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()

	_, err = NewPGXLinkRepository(mock).SubmitBatch(t.Context(), []CreateLinkParams{
		{URL: restoreURL, Status: model.LinkStatusPending},
		{URL: invalidURL, Status: model.LinkStatusPending, SourceMetadata: map[string]any{"bad": make(chan struct{})}},
	})
	if err == nil {
		t.Fatal("SubmitBatch() error = nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestLinkRepository_SubmitBatch_EmptyInputIsNoop verifies the early-
// return contract: an empty items slice must not open a tx (the mock
// has NO Expect* set up, so any DB call fails the test). This is the
// hot path for /api/links/batch requests where every item failed
// validateURL upstream — touching the DB for a no-op batch would burn
// a pool connection and a tx slot for nothing.
func TestLinkRepository_SubmitBatch_EmptyInputIsNoop(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXLinkRepository(mock)

	results, err := repo.SubmitBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("SubmitBatch(nil) error = %v, want nil", err)
	}
	if results != nil {
		t.Fatalf("SubmitBatch(nil) results = %#v, want nil", results)
	}

	// No mock expectations set: the verifier asserts zero SQL traffic.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

// TestLinkRepository_SubmitBatch_RollsBackOnJobsInsertFailure verifies
// transactional safety: when the parse_jobs INSERT fails after the
// links INSERT has succeeded, the rollback must fire so neither side
// of the change ends up committed. Without this the caller would
// observe orphan links with no scheduled job (parse pipeline never
// picks them up).
func TestLinkRepository_SubmitBatch_RollsBackOnJobsInsertFailure(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXLinkRepository(mock)

	firstID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	createdAt := time.Date(2026, 5, 16, 13, 0, 0, 0, time.UTC)

	expectedLinksSQL, _ := buildBatchInsertLinksSQL(1)
	expectedJobsSQL := buildBatchInsertJobsSQL(1)

	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	items := []CreateLinkParams{{URL: "https://example.com/fail", Status: model.LinkStatusPending}}
	expectNoCanonicalURLIdentities(mock, []string{"https://example.com/fail"})
	mock.ExpectQuery(regexp.QuoteMeta(selectSubmitLinksSQL)).
		WithArgs([]string{"https://example.com/fail"}, []string{"https://example.com/fail"}, []uuid.UUID{}).
		WillReturnRows(mock.NewRows(submitExistingColumns()))
	mock.ExpectQuery(regexp.QuoteMeta(expectedLinksSQL)).
		WithArgs(buildBatchLinkArgs("https://example.com/fail")...).
		WillReturnRows(
			mock.NewRows(linkColumns()).
				AddRow(buildBatchLinkRow(firstID, "https://example.com/fail", model.LinkStatusPending, createdAt, createdAt)...),
		)
	mock.ExpectQuery(regexp.QuoteMeta(expectedJobsSQL)).
		WithArgs(firstID).
		WillReturnError(errors.New("simulated jobs insert failure"))
	mock.ExpectRollback()

	_, err = repo.SubmitBatch(context.Background(), items)
	if err == nil {
		t.Fatal("SubmitBatch() error = nil, want jobs insert failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

// TestBuildBatchInsertLinksSQLPlaceholderStride locks the SQL template
// to its column-stride and ON CONFLICT contract. The placeholder count
// must equal N * insertLinkBatchColumnCount because every row in
// SubmitBatch's arg slice contributes exactly that many bound values;
// the ON CONFLICT and RETURNING clauses are runtime invariants. Catching drift
// here at compile/test time is cheaper than chasing a
// "row dropped silently" production bug.
func TestBuildBatchInsertLinksSQLPlaceholderStride(t *testing.T) {
	t.Parallel()

	const rowCount = 3
	sql, cols := buildBatchInsertLinksSQL(rowCount)
	if cols != insertLinkBatchColumnCount {
		t.Fatalf("buildBatchInsertLinksSQL columns = %d, want %d", cols, insertLinkBatchColumnCount)
	}

	// Each tuple has the normal bound-value stride plus references to its final
	// and predicted kind placeholders for the site payload deadline expression.
	// The extra occurrences are not extra arguments.
	wantPlaceholders := rowCount * (insertLinkBatchColumnCount + 2)
	gotPlaceholders := strings.Count(sql, "$")
	if gotPlaceholders != wantPlaceholders {
		t.Fatalf("placeholder count = %d, want %d (rowCount=%d, stride=%d)",
			gotPlaceholders, wantPlaceholders, rowCount, insertLinkBatchColumnCount)
	}

	// Column-name count == value-expression count. This is the assertion
	// the old "count the $ placeholders" check could NOT make: that check
	// is tautological (gotPlaceholders is derived from the same stride it
	// is compared against), so it stayed green even when the column NAME
	// list drifted while the VALUES tuple still bound an extra value — the
	// exact Phase 14 regression that made every real-PG batch INSERT fail
	// with "INSERT has more target columns than expressions". Parsing the
	// emitted SQL and asserting names == first-tuple expressions catches
	// that drift at test time instead of in production.
	gotColumns := countInsertColumns(t, sql)
	gotExprs := countFirstTupleExprs(t, sql)
	if gotColumns != gotExprs {
		t.Fatalf("column-name count = %d, value-expression count = %d; the two MUST match or PG rejects the INSERT\n%s",
			gotColumns, gotExprs, sql)
	}
	// And both must equal the bound stride plus the explicit content_source,
	// first_collected_at, deadline, created_at, and updated_at expressions.
	if wantTotal := insertLinkBatchColumnCount + 5; gotColumns != wantTotal {
		t.Fatalf("column/expression count = %d, want %d (stride %d + timestamp expressions)",
			gotColumns, wantTotal, insertLinkBatchColumnCount)
	}
	if strings.Count(sql, "'fetched'") != rowCount {
		t.Fatalf("content_source literals = %d, want %d", strings.Count(sql, "'fetched'"), rowCount)
	}

	if !strings.Contains(sql, "ON CONFLICT (source_key) DO NOTHING") {
		t.Fatalf("SQL missing ON CONFLICT (source_key) DO NOTHING clause:\n%s", sql)
	}
	if strings.Contains(sql, "DO UPDATE") || strings.Contains(sql, "xmax") {
		t.Fatalf("SQL physically rewrites conflicts or relies on system columns:\n%s", sql)
	}
	if !strings.Contains(sql, "RETURNING "+linkSelectColumns) {
		t.Fatalf("SQL missing RETURNING linkSelectColumns:\n%s", sql)
	}
	// Exactly (rowCount - 1) ", (" tuple separators plus one initial " (";
	// double-checking the row count keeps a future refactor that drops
	// a comma from drifting the template silently.
	if got := strings.Count(sql, "), ("); got != rowCount-1 {
		t.Fatalf("VALUES tuple separators = %d, want %d", got, rowCount-1)
	}
}

// countInsertColumns extracts the parenthesised column-name list from the
// emitted "INSERT INTO links (...) VALUES" prefix and returns the number of
// comma-separated names. It deliberately parses the real SQL string rather
// than trusting a constant so a column dropped from (or added to) the NAME
// list is observable to TestBuildBatchInsertLinksSQLPlaceholderStride.
func countInsertColumns(t *testing.T, sql string) int {
	t.Helper()
	const marker = "INSERT INTO links ("
	start := strings.Index(sql, marker)
	if start < 0 {
		t.Fatalf("SQL missing %q prefix:\n%s", marker, sql)
	}
	open := start + len(marker)
	end := strings.Index(sql[open:], ")")
	if end < 0 {
		t.Fatalf("SQL column list has no closing paren:\n%s", sql)
	}
	return countCommaSeparated(sql[open : open+end])
}

// countFirstTupleExprs returns the number of top-level comma-separated value
// expressions in the FIRST VALUES tuple (e.g. "$1, $2, ..., NOW(), NOW()").
// It walks the tuple with a paren-depth counter so the parentheses inside
// NOW() neither terminate the tuple early nor leak their inner commas into
// the count. This is the other half of the names==expressions invariant: a
// per-row tuple that binds one extra/fewer value than the column list
// declares would be caught.
func countFirstTupleExprs(t *testing.T, sql string) int {
	t.Helper()
	const marker = ") VALUES ("
	start := strings.Index(sql, marker)
	if start < 0 {
		t.Fatalf("SQL missing %q marker:\n%s", marker, sql)
	}
	open := start + len(marker)
	depth := 0 // nesting depth relative to the opened tuple
	exprs := 1 // a non-empty tuple has at least one expression
	nonEmpty := false
	for i := open; i < len(sql); i++ {
		switch sql[i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				// Reached the tuple's own closing paren.
				if !nonEmpty {
					return 0 // empty tuple "()"
				}
				return exprs
			}
			depth--
		case ',':
			if depth == 0 {
				exprs++
			}
		default:
			if sql[i] != ' ' {
				nonEmpty = true
			}
		}
	}
	t.Fatalf("SQL first VALUES tuple has no closing paren:\n%s", sql)
	return 0
}

// countCommaSeparated returns the number of comma-separated, non-empty
// trimmed fields in s. Used by countInsertColumns (the column-name list has
// no nested parens, so a flat split is exact there).
func countCommaSeparated(s string) int {
	n := 0
	for _, part := range strings.Split(s, ",") {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	return n
}
