package repository

import (
	"context"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"
)

func TestSearchSitesReportsEntryMatchedOnlyByPurpose(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	siteID, entryID := uuid.New(), uuid.New()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(DISTINCT s.id) FROM sites s WHERE lower(s.name) LIKE $1 OR lower(s.intro) LIKE $1 OR EXISTS (SELECT 1 FROM site_tags st WHERE st.site_id=s.id AND lower(st.tag) LIKE $1) OR EXISTS (SELECT 1 FROM site_entries se WHERE se.site_id=s.id AND (lower(se.entry_name) LIKE $1 OR lower(se.purpose) LIKE $1 OR lower(se.normalized_url) LIKE $1))`)).
		WithArgs("%integrate%").WillReturnRows(mock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`WITH matching_sites AS`).WithArgs("%integrate%", 10).
		WillReturnRows(mock.NewRows([]string{"id", "name", "entry_id", "entry_name", "purpose", "normalized_url"}).AddRow(siteID.String(), "Example", entryID.String(), "API", "Integrate your system", "https://example.com/api").AddRow(siteID.String(), "Example", nil, nil, nil, nil))

	items, total, err := NewPGXSiteRepository(mock).SearchSites(context.Background(), "integrate", 10)
	if err != nil || total != 1 || len(items) != 1 || len(items[0].MatchedEntries) != 1 || items[0].MatchedEntries[0].ID != entryID {
		t.Fatalf("SearchSites() = %#v, total=%d, err=%v", items, total, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
