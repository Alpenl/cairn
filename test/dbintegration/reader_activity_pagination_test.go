package dbintegration

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"webtag/internal/model"
	"webtag/internal/repository"
)

func TestReaderActivityDirectQueryScopesAggregationToReading(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)

	readingID := mustCreateDoneLink(t, links, ctx, "https://reading.example/article", "reading-only", "reading.example")
	siteID := mustCreateDoneLink(t, links, ctx, "https://site.example/app", "site-only", "site.example")
	if _, err := pool.Exec(t.Context(), `
		UPDATE links SET library_kind = 'reading', library_kind_locked = true
		WHERE id = $1`, readingID); err != nil {
		t.Fatalf("mark reading activity fixture: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE links SET library_kind = 'site', library_kind_locked = true
		WHERE id = $1`, siteID); err != nil {
		t.Fatalf("mark site activity fixture: %v", err)
	}

	repo := repository.NewPGXReaderVNextRepository(pool)
	page, err := repo.ListActivity(ctx, model.ReaderActivityQuery{Kind: model.ReaderActivityKindAll, Limit: 10})
	if err != nil {
		t.Fatalf("ListActivity() error = %v", err)
	}
	got := make(map[string]struct{}, len(page.Items))
	for _, item := range page.Items {
		got[item.Kind+":"+item.Key] = struct{}{}
	}
	for _, key := range []string{"tag:reading-only", "domain:reading.example"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("reading activity %q missing from %#v", key, page.Items)
		}
	}
	for _, key := range []string{"tag:site-only", "domain:site.example"} {
		if _, ok := got[key]; ok {
			t.Fatalf("site activity %q leaked into %#v", key, page.Items)
		}
	}
	if page.HasMore {
		t.Fatalf("scoped activity unexpectedly has another page: %#v", page)
	}
}

// TestReaderActivityCursorPaginationRunsAgainstPostgres freezes the seek
// predicate against PostgreSQL itself. pgxmock cannot prove that the UNION,
// timestamp direction and normalized-key tie breakers compose without a skip.
func TestReaderActivityCursorPaginationRunsAgainstPostgres(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)
	pageTime := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	bulkTags := make([]string, 0, 102)
	for index := 0; index < 102; index++ {
		bulkTags = append(bulkTags, fmt.Sprintf("tag-%03d", index))
	}
	bulkID := mustCreateDoneLink(t, links, ctx, "https://activity.example/bulk", "placeholder", "bulk.example")
	if _, err := pool.Exec(ctx, `
		UPDATE links
		SET tags=$2,library_kind='reading',library_kind_locked=true,
			created_at=$3,first_collected_at=$3,last_recollected_at=NULL
		WHERE id=$1`, bulkID, bulkTags, pageTime.Add(-time.Second)); err != nil {
		t.Fatalf("prepare bulk activity fixture: %v", err)
	}
	for _, fixture := range []struct {
		url    string
		tag    string
		domain string
	}{
		{url: "https://z.example/tie", tag: "Zulu", domain: "z.example"},
		{url: "https://a.example/tie", tag: "alpha", domain: "A.example"},
	} {
		linkID := mustCreateDoneLink(t, links, ctx, fixture.url, fixture.tag, fixture.domain)
		if _, err := pool.Exec(ctx, `
			UPDATE links
			SET library_kind='reading',library_kind_locked=true,
				created_at=$2,first_collected_at=$2,last_recollected_at=NULL
			WHERE id=$1`, linkID, pageTime); err != nil {
			t.Fatalf("prepare tie activity fixture %s: %v", fixture.tag, err)
		}
	}
	repo := repository.NewPGXReaderVNextRepository(pool)
	tiePage, err := repo.ListActivity(ctx, model.ReaderActivityQuery{Kind: "all", Limit: 4})
	if err != nil {
		t.Fatalf("ListActivity(ties) error = %v", err)
	}
	gotTies := make([]string, 0, len(tiePage.Items))
	for _, item := range tiePage.Items {
		gotTies = append(gotTies, item.Kind+":"+item.Key)
	}
	wantTies := []string{"domain:A.example", "domain:z.example", "tag:alpha", "tag:Zulu"}
	if !slices.Equal(gotTies, wantTies) {
		t.Fatalf("same-time activity order = %v, want %v", gotTies, wantTies)
	}

	var all []string
	query := model.ReaderActivityQuery{Kind: "tag", Limit: 100}
	for {
		page, err := repo.ListActivity(ctx, query)
		if err != nil {
			t.Fatalf("ListActivity(tag page %d) error = %v", len(all)/100+1, err)
		}
		for _, item := range page.Items {
			if item.Kind != "tag" {
				t.Fatalf("kind leak in tag page: %#v", item)
			}
			all = append(all, item.Key)
		}
		if !page.HasMore {
			break
		}
		last := page.Items[len(page.Items)-1]
		query.After = &model.ReaderActivityCursor{
			LastAt: last.LastAt, Kind: last.Kind, NormalizedKey: last.NormalizedKey, Key: last.Key,
		}
	}
	if len(all) != 104 {
		t.Fatalf("tag activity rows across pages = %d, want 104", len(all))
	}
	seen := make(map[string]struct{}, len(all))
	for _, key := range all {
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("duplicate tag %q across cursor pages", key)
		}
		seen[key] = struct{}{}
	}
	for _, key := range []string{"tag-098", "tag-099", "tag-100", "tag-101"} {
		if _, ok := seen[key]; !ok {
			t.Fatalf("cursor pages skipped %q", key)
		}
	}
}
