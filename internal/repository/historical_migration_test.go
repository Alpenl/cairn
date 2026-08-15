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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func historicalSuggestionPayload(t *testing.T, rawURL, title string, contentRevision int64) []byte {
	t.Helper()
	assessment := HistoricalMigrationAssessment{Candidate: HistoricalMigrationCandidate{
		URL: rawURL, Title: title, ContentType: model.ContentTypeHomepage,
		ContentRevision: contentRevision, MetadataRevision: 1,
		RequestedKind: model.RequestedLibraryKindAuto, RequestedSource: model.RequestedLibraryKindSourceAuto,
		LibraryKind: model.LibraryKindReading, Source: model.LibraryKindSourceMigration,
		HasContent: true,
	}, PredictedKind: model.LibraryKindSite, Confidence: .99, Reason: "migration_assets_require_review", Suggest: true}
	payload, err := json.Marshal(newHistoricalMigrationReviewPayload(assessment.Candidate, assessment))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func historicalReviewLinkRows(rawURL, title string, contentRevision int64) *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"url", "title", "status", "active", "content_type", "content_revision", "metadata_revision",
		"requested_library_kind", "requested_library_kind_source", "library_kind", "library_kind_source", "library_kind_locked",
		"predicted_library_kind", "classification_confidence", "classification_reason", "classification_explanation",
		"classifier_version", "has_content", "has_translations",
	}).AddRow(rawURL, title, "done", true, "homepage", contentRevision, int64(1), "auto", "auto", "reading", "migration", false,
		"site", pgtype.Float4{Float32: .99, Valid: true}, "migration_assets_require_review", "", historicalMigrationClassifierVersion, true, false)
}

func TestHistoricalMigrationCandidateScanUsesInstallScopeAndCreationCursor(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	created, id := time.Now().UTC().Add(-time.Hour), uuid.New()
	mock.ExpectQuery(regexp.QuoteMeta(listHistoricalMigrationCandidatesSQL)).WithArgs(time.Time{}, uuid.Nil, 20).
		WillReturnRows(mock.NewRows([]string{
			"id", "url", "title", "content_type", "content_revision", "created_at", "metadata_revision",
			"requested_library_kind", "requested_library_kind_source", "library_kind", "library_kind_source", "library_kind_locked",
			"predicted_library_kind", "classification_confidence", "classification_reason", "classification_explanation",
			"classifier_version", "has_content", "has_translations",
		}).AddRow(id, "https://example.com/", "Example", "homepage", int64(4), created, int64(1), "auto", "auto",
			"reading", "migration", false, "", "", "", "", "", false, false))
	items, err := NewPGXLinkRepository(mock).ListHistoricalMigrationCandidates(context.Background(), HistoricalMigrationCursor{}, 20)
	if err != nil || len(items) != 1 || items[0].ID != id || items[0].HasReadingAssets() {
		t.Fatalf("scan = %#v, %v", items, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMoveHistoricalMigrationToSiteConvertsThenResolvesReviewAtomically(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	reviewID, linkID, siteID, entryID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	var nilText *string
	payload := historicalSuggestionPayload(t, "https://example.com/docs", "Docs", 4)
	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockMigrationReviewSQL)).WithArgs(reviewID, int64(2)).
		WillReturnRows(mock.NewRows([]string{"link_id", "payload"}).AddRow(linkID, payload))
	mock.ExpectQuery(regexp.QuoteMeta(lockHistoricalMigrationReviewLinkSQL)).WithArgs(linkID).
		WillReturnRows(historicalReviewLinkRows("https://example.com/docs", "Docs", 4))
	expectNoThoughtSnapshotsForConversion(mock, linkID)
	mock.ExpectExec(regexp.QuoteMeta(deleteConversionContentSQL)).WithArgs(linkID).WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(regexp.QuoteMeta(convertReadingToSiteSQL)).WithArgs(linkID).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(regexp.QuoteMeta(findSiteIdentityForUpdateSQL)).WithArgs("v1:host:example.com").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteSQL)).WithArgs("v1:host:example.com", "Docs", "", nilText, nilText).WillReturnRows(mock.NewRows([]string{"id", "created"}).AddRow(siteID, true))
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteIdentitySQL)).WithArgs("v1:host:example.com", siteID).WillReturnRows(mock.NewRows([]string{"site_id"}).AddRow(siteID))
	mock.ExpectQuery(regexp.QuoteMeta(findSiteEntryByNormalizedURLSQL)).WithArgs(siteID, "https://example.com/docs", linkID).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteEntrySQL)).WithArgs(siteID, linkID, "Docs", "", "https://example.com/docs").WillReturnRows(mock.NewRows([]string{"id", "site_id", "created"}).AddRow(entryID, siteID, true))
	mock.ExpectExec(regexp.QuoteMeta(setPrimarySiteEntrySQL)).WithArgs(entryID, siteID).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT revision FROM sites WHERE id=$1")).WithArgs(siteID).WillReturnRows(mock.NewRows([]string{"revision"}).AddRow(int64(1)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM site_entries WHERE link_id=$1")).WithArgs(linkID).WillReturnRows(mock.NewRows([]string{"id"}).AddRow(entryID))
	mock.ExpectExec(regexp.QuoteMeta(copyLinkTagsToSiteSQL)).WithArgs(siteID, linkID).WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery(regexp.QuoteMeta(resolveMigrationReviewSQL)).WithArgs(reviewID, int64(2)).
		WillReturnRows(mock.NewRows([]string{"id", "kind", "link_id", "site_id", "payload", "status", "revision", "created_at", "resolved_at"}).AddRow(reviewID.String(), "migration_suggestion", linkID.String(), nil, payload, "applied", int64(3), now, now))
	mock.ExpectCommit()

	item, err := NewPGXLinkRepository(mock).MoveHistoricalMigrationToSite(context.Background(), reviewID, 2)
	if err != nil || item == nil || item.Status != model.LibraryReviewStatusApplied || item.Revision != 3 {
		t.Fatalf("MoveHistoricalMigrationToSite() = %#v, %v", item, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMoveHistoricalMigrationToSiteRollsBackBeforeReviewResolutionOnConversionFailure(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	reviewID, linkID := uuid.New(), uuid.New()
	payload := historicalSuggestionPayload(t, "https://example.com/docs", "Docs", 4)
	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockMigrationReviewSQL)).WithArgs(reviewID, int64(2)).WillReturnRows(mock.NewRows([]string{"link_id", "payload"}).AddRow(linkID, payload))
	mock.ExpectQuery(regexp.QuoteMeta(lockHistoricalMigrationReviewLinkSQL)).WithArgs(linkID).WillReturnRows(historicalReviewLinkRows("https://example.com/docs", "Docs", 4))
	expectNoThoughtSnapshotsForConversion(mock, linkID)
	conversionErr := errors.New("translation delete failed")
	mock.ExpectExec(regexp.QuoteMeta(deleteConversionContentSQL)).WithArgs(linkID).WillReturnError(conversionErr)
	mock.ExpectRollback()
	_, err = NewPGXLinkRepository(mock).MoveHistoricalMigrationToSite(context.Background(), reviewID, 2)
	if !errors.Is(err, conversionErr) {
		t.Fatalf("MoveHistoricalMigrationToSite() error = %v, want conversion error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationReviewActionDismissesLegacyDecisionIdentity(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	reviewID, linkID := uuid.New(), uuid.New()
	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockMigrationReviewSQL)).WithArgs(reviewID, int64(1)).
		WillReturnRows(mock.NewRows([]string{"link_id", "payload"}).AddRow(linkID, []byte(`{"target_kind":"site","content_revision":4}`)))
	mock.ExpectQuery(regexp.QuoteMeta(lockHistoricalMigrationReviewLinkSQL)).WithArgs(linkID).
		WillReturnRows(historicalReviewLinkRows("https://example.com/", "Example", 4))
	mock.ExpectExec(regexp.QuoteMeta(dismissLockedMigrationReviewSQL)).WithArgs(reviewID, int64(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	item, err := NewPGXLinkRepository(mock).KeepHistoricalMigrationReading(context.Background(), reviewID, 1)
	if item != nil || !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("KeepHistoricalMigrationReading() = %#v, %v, want nil/revision conflict", item, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationReviewActionDismissesChangedClassifierState(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	reviewID, linkID := uuid.New(), uuid.New()
	payload := historicalSuggestionPayload(t, "https://example.com/", "Example", 4)
	changed := pgxmock.NewRows([]string{
		"url", "title", "status", "active", "content_type", "content_revision", "metadata_revision",
		"requested_library_kind", "requested_library_kind_source", "library_kind", "library_kind_source", "library_kind_locked",
		"predicted_library_kind", "classification_confidence", "classification_reason", "classification_explanation",
		"classifier_version", "has_content", "has_translations",
	}).AddRow("https://example.com/", "Example", "done", true, "article", int64(4), int64(1), "auto", "auto", "reading", "migration", false,
		"site", pgtype.Float4{Float32: .99, Valid: true}, "new_classifier_decision", "", "classifier-v2", true, false)
	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockMigrationReviewSQL)).WithArgs(reviewID, int64(1)).
		WillReturnRows(mock.NewRows([]string{"link_id", "payload"}).AddRow(linkID, payload))
	mock.ExpectQuery(regexp.QuoteMeta(lockHistoricalMigrationReviewLinkSQL)).WithArgs(linkID).WillReturnRows(changed)
	mock.ExpectExec(regexp.QuoteMeta(dismissLockedMigrationReviewSQL)).WithArgs(reviewID, int64(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	item, err := NewPGXLinkRepository(mock).MoveHistoricalMigrationToSite(context.Background(), reviewID, 1)
	if item != nil || !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("MoveHistoricalMigrationToSite() = %#v, %v, want nil/revision conflict", item, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
