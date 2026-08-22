package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"
)

func TestListSitesRecentAppliesInclusiveCutoffToCountAndStableList(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXSiteRepository(mock)
	cutoff := time.Date(2026, 7, 11, 2, 15, 30, 123000000, time.FixedZone("CST", 8*60*60))
	utcCutoff := cutoff.UTC()
	newerID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	olderID := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*) FROM sites s WHERE TRUE AND s.last_collected_at >= $1")).
		WithArgs(utcCutoff).
		WillReturnRows(mock.NewRows([]string{"count"}).AddRow(2))
	listPattern := `SELECT .* FROM sites s .* WHERE TRUE AND s\.last_collected_at >= \$1 GROUP BY s\.id, pe\.normalized_url ORDER BY s\.last_collected_at DESC, s\.id DESC LIMIT \$2 OFFSET \$3`
	rows := mock.NewRows([]string{"id", "site_key", "name", "intro", "homepage_url", "icon_url", "pinned", "revision", "first_collected_at", "last_collected_at", "primary_entry_id", "normalized_url", "entry_count", "tags"}).
		AddRow(newerID, "v1:host:newer.test", "Newer", "", nil, nil, false, int64(1), utcCutoff, utcCutoff.Add(time.Millisecond), nil, nil, int64(0), []string{}).
		AddRow(olderID, "v1:host:older.test", "Older", "", nil, nil, false, int64(1), utcCutoff, utcCutoff, nil, nil, int64(0), []string{})
	mock.ExpectQuery(listPattern).WithArgs(utcCutoff, 2, 4).WillReturnRows(rows)

	items, total, err := repo.ListSites(context.Background(), SiteListFilter{View: "recent", RecentCutoff: &cutoff, Limit: 2, Offset: 4})
	if err != nil {
		t.Fatalf("ListSites() error = %v", err)
	}
	if total != 2 || len(items) != 2 || items[0].ID != newerID || items[1].ID != olderID {
		t.Fatalf("total=%d items=%#v", total, items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListSitesRecentRequiresCutoff(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	_, _, err = NewPGXSiteRepository(mock).ListSites(context.Background(), SiteListFilter{View: "recent", Limit: 30})
	if err == nil {
		t.Fatal("recent list without cutoff succeeded")
	}
}
