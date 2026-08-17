package repository

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestConvertReadingToSiteClearsAllFieldsRequiredBySiteConstraint(t *testing.T) {
	t.Parallel()
	for _, fragment := range []string{
		"summary=NULL",
		"content=NULL",
		"content_document=NULL",
	} {
		if !strings.Contains(convertReadingToSiteSQL, fragment) {
			t.Fatalf("convertReadingToSiteSQL must include %q to satisfy chk_links_site_has_no_content", fragment)
		}
	}
}

func TestConversionQueriesDoNotDependOnTenantIdentity(t *testing.T) {
	t.Parallel()
	for _, query := range []string{lockConvertibleLinkSQL, lockConvertibleLinkWithSummarySQL, deleteConversionContentSQL, convertReadingToSiteSQL, convertSiteToReadingSQL, lockEntryByLinkSQL, deleteEntryByLinkSQL, advanceSiteForConversionSQL, appendConversionNoteSQL, appendNewConversionNoteSQL, copyLinkTagsToSiteSQL} {
		if strings.Contains(strings.ToLower(query), "tenant") {
			t.Fatalf("conversion query retains tenant identity: %s", query)
		}
	}
}

func expectSiteToReadingConversion(mock pgxmock.PgxPoolIface, linkID, siteID, entryID, jobID uuid.UUID, revision int64) {
	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockConvertibleLinkWithSummarySQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"url", "title", "summary", "status", "library_kind", "content_revision"}).
			AddRow("https://example.com/docs", "Docs", "", string(model.LinkStatusDone), string(model.LibraryKindSite), revision))
	mock.ExpectQuery(regexp.QuoteMeta(lockEntryByLinkSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"site_id", "id"}).AddRow(siteID, entryID))
	mock.ExpectQuery(regexp.QuoteMeta(lockSiteForManagementSQL)).WithArgs(siteID).
		WillReturnRows(mock.NewRows([]string{"revision", "primary_entry_id"}).AddRow(revision, entryID.String()))
	mock.ExpectQuery(regexp.QuoteMeta(countSiteEntriesSQL)).WithArgs(siteID).
		WillReturnRows(mock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta(clearManagedPrimaryEntrySQL)).WithArgs(nil, siteID, revision).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(deleteEntryByLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(regexp.QuoteMeta(deleteManagedSiteSQL)).WithArgs(siteID).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(regexp.QuoteMeta(convertSiteToReadingSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(insertJobSQL)).WithArgs(linkID, parseJobsPerLinkRetention).
		WillReturnRows(mock.NewRows(jobColumns()).
			AddRow(jobID, linkID, string(model.JobStatusPending), nil, now, now, int64(1)))
}

func expectNoThoughtSnapshotsForConversion(mock pgxmock.PgxPoolIface, linkID uuid.UUID) {
	mock.ExpectQuery("(?s)SELECT thought.id.*FROM reader_thoughts thought.*host_kind='link'.*host_id=\\$1.*NOT EXISTS.*reader_thought_tombstones.*ORDER BY thought.id.*FOR UPDATE").
		WithArgs(linkID.String()).
		WillReturnRows(mock.NewRows([]string{"id"}))
}

func TestConvertSiteToReadingCommitsLinkSiteAndJobTogether(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	linkID, siteID, entryID, jobID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	expectSiteToReadingConversion(mock, linkID, siteID, entryID, jobID, 4)
	mock.ExpectCommit()
	result, err := NewPGXLinkRepository(mock).ConvertLink(context.Background(), ConvertLinkParams{LinkID: linkID, TargetKind: model.LibraryKindReading, ExpectedContentRevision: 4, ExpectedSiteRevision: int64Ptr(4)})
	if err != nil {
		t.Fatalf("ConvertLink() error = %v", err)
	}
	if result.Kind != model.LibraryKindReading || result.Status != model.LinkStatusPending || result.ParseJobID == nil || *result.ParseJobID != jobID || result.ContentRevision != 5 {
		t.Fatalf("ConvertLink() result = %#v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestConvertReadingToSiteRollsBackWhenTagCopyFails(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	linkID, siteID, entryID := uuid.New(), uuid.New(), uuid.New()
	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockConvertibleLinkWithSummarySQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"url", "title", "summary", "status", "library_kind", "content_revision"}).
			AddRow("https://example.com/docs", "Docs", "Useful tool", string(model.LinkStatusDone), string(model.LibraryKindReading), int64(3)))
	expectNoThoughtSnapshotsForConversion(mock, linkID)
	mock.ExpectExec(regexp.QuoteMeta(deleteConversionContentSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("DELETE", 2))
	mock.ExpectExec(regexp.QuoteMeta(convertReadingToSiteSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(regexp.QuoteMeta(lockSiteForManagementSQL)).WithArgs(siteID).
		WillReturnRows(mock.NewRows([]string{"revision", "primary_entry_id"}).AddRow(int64(5), nil))
	mock.ExpectQuery(regexp.QuoteMeta(findSiteEntryByNormalizedURLSQL)).WithArgs(siteID, "https://example.com/docs", linkID).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteEntrySQL)).WithArgs(siteID, linkID, "Docs", "", "https://example.com/docs").
		WillReturnRows(mock.NewRows([]string{"id", "site_id", "created"}).AddRow(entryID, siteID, true))
	mock.ExpectExec(regexp.QuoteMeta(setPrimarySiteEntrySQL)).WithArgs(entryID, siteID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(advanceSiteForConversionSQL)).WithArgs(siteID, int64(5)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	copyErr := errors.New("tag copy failed")
	mock.ExpectExec(regexp.QuoteMeta(copyLinkTagsToSiteSQL)).WithArgs(siteID, linkID).
		WillReturnError(copyErr)
	mock.ExpectRollback()

	_, err = NewPGXLinkRepository(mock).ConvertLink(context.Background(), ConvertLinkParams{LinkID: linkID, TargetKind: model.LibraryKindSite, ExpectedContentRevision: 3, TargetSiteID: &siteID, ExpectedSiteRevision: int64Ptr(5)})
	if !errors.Is(err, copyErr) {
		t.Fatalf("ConvertLink() error = %v, want tag-copy error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestConvertReadingToSiteUsesExistingSummaryAsNewSiteIntro(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	linkID, siteID, entryID := uuid.New(), uuid.New(), uuid.New()
	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockConvertibleLinkWithSummarySQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"url", "title", "summary", "status", "library_kind", "content_revision"}).
			AddRow("https://example.com/docs", "Docs", "Useful tool", string(model.LinkStatusDone), string(model.LibraryKindReading), int64(3)))
	expectNoThoughtSnapshotsForConversion(mock, linkID)
	mock.ExpectExec(regexp.QuoteMeta(deleteConversionContentSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(regexp.QuoteMeta(convertReadingToSiteSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(regexp.QuoteMeta(findSiteIdentityForUpdateSQL)).WithArgs("v1:host:example.com").WillReturnError(pgx.ErrNoRows)
	var nilText *string
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteSQL)).WithArgs("v1:host:example.com", "Docs", "Useful tool", nilText, nilText).
		WillReturnRows(mock.NewRows([]string{"id", "created"}).AddRow(siteID, true))
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteIdentitySQL)).WithArgs("v1:host:example.com", siteID).
		WillReturnRows(mock.NewRows([]string{"site_id"}).AddRow(siteID))
	mock.ExpectQuery(regexp.QuoteMeta(findSiteEntryByNormalizedURLSQL)).WithArgs(siteID, "https://example.com/docs", linkID).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteEntrySQL)).WithArgs(siteID, linkID, "Docs", "", "https://example.com/docs").
		WillReturnRows(mock.NewRows([]string{"id", "site_id", "created"}).AddRow(entryID, siteID, true))
	mock.ExpectExec(regexp.QuoteMeta(setPrimarySiteEntrySQL)).WithArgs(entryID, siteID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT revision FROM sites WHERE id=$1")).WithArgs(siteID).
		WillReturnRows(mock.NewRows([]string{"revision"}).AddRow(int64(1)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM site_entries WHERE link_id=$1")).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(entryID))
	copyErr := errors.New("tag copy failed")
	mock.ExpectExec(regexp.QuoteMeta(copyLinkTagsToSiteSQL)).WithArgs(siteID, linkID).WillReturnError(copyErr)
	mock.ExpectRollback()

	_, err = NewPGXLinkRepository(mock).ConvertLink(context.Background(), ConvertLinkParams{LinkID: linkID, TargetKind: model.LibraryKindSite, ExpectedContentRevision: 3})
	if !errors.Is(err, copyErr) {
		t.Fatalf("ConvertLink() error = %v, want tag-copy error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReadingToSiteSnapshotLocksEveryActiveThoughtAndFailsAtomically(t *testing.T) {
	t.Parallel()
	linkID := uuid.New()
	for _, testCase := range []struct {
		name        string
		snapshotErr error
		wantErr     bool
	}{
		{name: "writes one lifecycle operation and immutable snapshot per active thought"},
		{name: "snapshot write failure rolls back lifecycle operations and conversion transaction", snapshotErr: errors.New("snapshot write failed"), wantErr: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatal(err)
			}
			defer mock.Close()

			mock.ExpectBegin()
			mock.ExpectQuery("(?s)SELECT thought.id.*FROM reader_thoughts thought.*host_kind='link'.*host_id=\\$1.*NOT EXISTS.*reader_thought_tombstones.*ORDER BY thought.id.*FOR UPDATE").
				WithArgs(linkID.String()).
				WillReturnRows(mock.NewRows([]string{"id"}).AddRow("thought-a").AddRow("thought-b"))

			expectConversionThoughtLifecycle(mock, linkID, "thought-a", 3, 21, nil)
			if testCase.snapshotErr != nil {
				expectConversionThoughtLifecycle(mock, linkID, "thought-b", 7, 22, testCase.snapshotErr)
				mock.ExpectRollback()
			} else {
				expectConversionThoughtLifecycle(mock, linkID, "thought-b", 7, 22, nil)
				mock.ExpectCommit()
			}

			tx, err := mock.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			err = tombstoneReadingThoughtsForSiteConversion(context.Background(), tx, linkID)
			if testCase.wantErr {
				if !errors.Is(err, testCase.snapshotErr) {
					t.Fatalf("tombstoneReadingThoughtsForSiteConversion() error = %v, want %v", err, testCase.snapshotErr)
				}
				_ = tx.Rollback(context.Background())
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func expectConversionThoughtLifecycle(mock pgxmock.PgxPoolIface, linkID uuid.UUID, thoughtID string, winnerClock, sequence int64, snapshotErr error) {
	at := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	row := readerThoughtSyncRow(thoughtID, winnerClock, false, at)
	row[2] = linkID.String()
	row[3] = &linkID
	row[4] = []byte(`{"kind":"saved-content","host_id":"` + linkID.String() + `","version":{"content_revision":3}}`)
	row[5] = []byte(`{"exact":"frozen quote","prefix":"before ","suffix":" after"}`)
	row[6] = "frozen conversion body"
	row[7] = "conversion-source"
	expectMarkThoughtLifecycle(mock, thoughtID, row, "link", linkID.String(), "link_converted_to_site", winnerClock, sequence)
	snapshot := mock.ExpectExec("(?s)INSERT INTO reader_thought_tombstones.*original_host_snapshot.*original_host_identity.*ON CONFLICT.*DO NOTHING").
		WithArgs(thoughtID, "link_converted_to_site", sequence)
	if snapshotErr != nil {
		snapshot.WillReturnError(snapshotErr)
		return
	}
	snapshot.WillReturnResult(pgxmock.NewResult("INSERT", 1))
	expectThoughtTodoProjectionRefresh(mock, thoughtID)
}

func int64Ptr(value int64) *int64 { return &value }
