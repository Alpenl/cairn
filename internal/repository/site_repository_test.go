package repository

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestAggregateSiteCreatesIdentityEntryAndPrimary(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID, siteID, entryID := uuid.New(), uuid.New(), uuid.New()
	params := AggregateSiteParams{LinkID: linkID, IdentityKey: "v1:host:example.com", NormalizedURL: "https://example.com/tool", Name: "Example", Intro: "Useful tool", EntryName: "Tool", Purpose: "Use the tool"}
	var nilText *string

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(findSiteIdentityForUpdateSQL)).WithArgs(params.IdentityKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteSQL)).WithArgs(params.IdentityKey, params.Name, params.Intro, nilText, nilText).WillReturnRows(mock.NewRows([]string{"id", "created"}).AddRow(siteID, true))
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteIdentitySQL)).WithArgs(params.IdentityKey, siteID).WillReturnRows(mock.NewRows([]string{"site_id"}).AddRow(siteID))
	mock.ExpectQuery(regexp.QuoteMeta(findSiteEntryByNormalizedURLSQL)).WithArgs(siteID, params.NormalizedURL, linkID).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteEntrySQL)).WithArgs(siteID, linkID, params.EntryName, params.Purpose, params.NormalizedURL).WillReturnRows(mock.NewRows([]string{"id", "site_id", "created"}).AddRow(entryID, siteID, true))
	mock.ExpectExec(regexp.QuoteMeta(setPrimarySiteEntrySQL)).WithArgs(entryID, siteID).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	got, err := repo.Aggregate(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if got.SiteID != siteID || got.EntryID != entryID || !got.CreatedSite || !got.CreatedEntry {
		t.Fatalf("aggregate result = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAggregateSiteUsesExistingIdentityBinding(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID, siteID, entryID := uuid.New(), uuid.New(), uuid.New()
	params := AggregateSiteParams{LinkID: linkID, IdentityKey: "v1:github:owner/repo", NormalizedURL: "https://github.com/owner/repo", Name: "Ignored candidate", EntryName: "Repository"}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(findSiteIdentityForUpdateSQL)).WithArgs(params.IdentityKey).WillReturnRows(mock.NewRows([]string{"site_id"}).AddRow(siteID))
	mock.ExpectQuery(regexp.QuoteMeta(findSiteEntryByNormalizedURLSQL)).WithArgs(siteID, params.NormalizedURL, linkID).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteEntrySQL)).WithArgs(siteID, linkID, params.EntryName, params.Purpose, params.NormalizedURL).WillReturnRows(mock.NewRows([]string{"id", "site_id", "created"}).AddRow(entryID, siteID, false))
	mock.ExpectExec(regexp.QuoteMeta(setPrimarySiteEntrySQL)).WithArgs(entryID, siteID).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	got, err := repo.Aggregate(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if got.SiteID != siteID || got.EntryID != entryID || got.CreatedSite || got.CreatedEntry {
		t.Fatalf("aggregate result = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAggregateSiteRemovesNewCandidateWhenIdentityWasBoundConcurrently(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID, candidateID, boundSiteID, entryID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	params := AggregateSiteParams{LinkID: linkID, IdentityKey: "v1:host:example.com", NormalizedURL: "https://example.com", Name: "Example", EntryName: "Home"}
	var nilText *string

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(findSiteIdentityForUpdateSQL)).WithArgs(params.IdentityKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteSQL)).WithArgs(params.IdentityKey, params.Name, params.Intro, nilText, nilText).WillReturnRows(mock.NewRows([]string{"id", "created"}).AddRow(candidateID, true))
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteIdentitySQL)).WithArgs(params.IdentityKey, candidateID).WillReturnRows(mock.NewRows([]string{"site_id"}).AddRow(boundSiteID))
	mock.ExpectExec(regexp.QuoteMeta(deleteUnboundSiteSQL)).WithArgs(candidateID).WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectQuery(regexp.QuoteMeta(findSiteEntryByNormalizedURLSQL)).WithArgs(boundSiteID, params.NormalizedURL, linkID).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteEntrySQL)).WithArgs(boundSiteID, linkID, params.EntryName, params.Purpose, params.NormalizedURL).WillReturnRows(mock.NewRows([]string{"id", "site_id", "created"}).AddRow(entryID, boundSiteID, true))
	mock.ExpectExec(regexp.QuoteMeta(setPrimarySiteEntrySQL)).WithArgs(entryID, boundSiteID).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(bumpSiteRevisionSQL)).WithArgs(boundSiteID).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	got, err := repo.Aggregate(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if got.SiteID != boundSiteID || got.CreatedSite {
		t.Fatalf("aggregate result = %#v, want bound site without creation", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteSiteParseClearsReadingAssetsBeforeMakingLinkDone(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID, siteID, entryID := uuid.New(), uuid.New(), uuid.New()
	name, intro, entryName, purpose := "Example", "An example service", "Home", "Use it online"
	params := CompleteSiteParseParams{
		Analysis:       UpdateLinkAnalysisParams{ID: linkID, Title: &entryName, Tags: []string{"tool"}, FetcherType: stringPtr("http"), Domain: stringPtr("example.com"), ContentType: stringPtr("homepage"), ExpectedParseGeneration: 1, ExpectedMetadataRevision: 1},
		Classification: UpdateLibraryClassificationParams{ID: linkID, Kind: "site"},
		Site:           AggregateSiteParams{LinkID: linkID, IdentityKey: "v1:host:example.com", NormalizedURL: "https://example.com", Name: name, Intro: intro, EntryName: entryName, Purpose: purpose},
	}
	var nilText *string
	var nilInt *int
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(completeSiteLinkSQL)).WithArgs(linkID, &entryName, []string{"tool"}, stringPtr("http"), false, nilText, stringPtr("example.com"), stringPtr("homepage"), nilInt, nilText, nil, false, (*model.LibraryKind)(nil), false, int64(1), int64(1)).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(deleteSiteTranslationsSQL)).WithArgs(linkID).WillReturnResult(pgxmock.NewResult("DELETE", 2))
	mock.ExpectQuery(regexp.QuoteMeta(findSiteIdentityForUpdateSQL)).WithArgs(params.Site.IdentityKey).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteSQL)).WithArgs(params.Site.IdentityKey, name, intro, nilText, nilText).WillReturnRows(mock.NewRows([]string{"id", "created"}).AddRow(siteID, true))
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteIdentitySQL)).WithArgs(params.Site.IdentityKey, siteID).WillReturnRows(mock.NewRows([]string{"site_id"}).AddRow(siteID))
	mock.ExpectQuery(regexp.QuoteMeta(findSiteEntryByNormalizedURLSQL)).WithArgs(siteID, params.Site.NormalizedURL, linkID).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(insertSiteEntrySQL)).WithArgs(siteID, linkID, entryName, purpose, params.Site.NormalizedURL).WillReturnRows(mock.NewRows([]string{"id", "site_id", "created"}).AddRow(entryID, siteID, true))
	mock.ExpectExec(regexp.QuoteMeta(setPrimarySiteEntrySQL)).WithArgs(entryID, siteID).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
	got, err := repo.CompleteSiteParse(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if got.SiteID != siteID || got.EntryID != entryID {
		t.Fatalf("result = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteSiteLinkSQLPurgesAllCapturedBodyFields(t *testing.T) {
	t.Parallel()
	for _, fragment := range []string{
		"content=NULL",
		"content_document=NULL",
		"content_format='plain'",
		"content_revision=link.content_revision+1",
		"input_text=NULL",
		"input_html=NULL",
		"input_images=NULL",
		"source_metadata=NULL",
		"payload_purge_due_at=NULL",
		"payload_purged_at=NOW()",
	} {
		if !strings.Contains(completeSiteLinkSQL, fragment) {
			t.Fatalf("completeSiteLinkSQL must include %q: %s", fragment, completeSiteLinkSQL)
		}
	}
}

func TestSiteAggregationQueriesDoNotDependOnTenantIdentity(t *testing.T) {
	t.Parallel()
	for _, query := range []string{findSiteIdentityForUpdateSQL, insertSiteSQL, insertSiteIdentitySQL, deleteUnboundSiteSQL, insertSiteEntrySQL, setPrimarySiteEntrySQL, completeSiteLinkSQL, deleteSiteTranslationsSQL} {
		if strings.Contains(strings.ToLower(query), "tenant") {
			t.Fatalf("site aggregation query retains tenant identity: %s", query)
		}
	}
}

func TestListRelatedReadingsScopesToDoneReadingsAndExcludesSiteEntries(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	readingID := uuid.New()
	mock.ExpectQuery("SELECT l.id, COALESCE\\(l.title, l.url\\), l.url, l.created_at").
		WithArgs([]string{"example.com"}, []string{"go"}, 6).
		WillReturnRows(mock.NewRows([]string{"id", "title", "url", "created_at"}).AddRow(readingID, "Guide", "https://example.com/guide", time.Now().UTC()))
	items, err := NewPGXSiteRepository(mock).ListRelatedReadings(context.Background(), []string{"example.com"}, []string{"go"}, 6)
	if err != nil || len(items) != 1 || items[0].ID != readingID {
		t.Fatalf("ListRelatedReadings() = %#v, %v", items, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"l.library_kind='reading'", "l.status='done'", "NOT EXISTS (SELECT 1 FROM site_entries"} {
		if !strings.Contains(relatedReadingsSQL, required) {
			t.Fatalf("related reading query must include %q", required)
		}
	}
	if strings.Contains(strings.ToLower(relatedReadingsSQL), "tenant") {
		t.Fatalf("related reading query retains tenant identity: %s", relatedReadingsSQL)
	}
}

func TestListRelatedReadingsSkipsDatabaseForNoCandidates(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	items, err := NewPGXSiteRepository(mock).ListRelatedReadings(context.Background(), nil, nil, 6)
	if err != nil || len(items) != 0 {
		t.Fatalf("ListRelatedReadings() = %#v, %v", items, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateSiteProfileUsesInstallScopedRevisionCAS(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXSiteRepository(mock)
	id := uuid.New()
	name, intro := "Example", "Useful"
	pinned := true
	params := UpdateSiteProfileParams{ID: id, Revision: 7, Name: &name, Intro: &intro, Pinned: &pinned}
	mock.ExpectExec(regexp.QuoteMeta(updateSiteProfileSQL)).WithArgs(params.Name, params.Intro, params.HomepageURL, params.IconURL, params.UserNote, params.Pinned, id, int64(7)).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	updated, err := repo.UpdateSiteProfile(context.Background(), params)
	if err != nil || !updated {
		t.Fatalf("UpdateSiteProfile() = %v, %v", updated, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func stringPtr(value string) *string { return &value }
