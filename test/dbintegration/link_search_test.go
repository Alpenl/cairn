package dbintegration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
)

func seedSearchLink(t *testing.T, pool *pgxpool.Pool, title, summary, domain string, tags []string, kind model.LibraryKind) uuid.UUID {
	t.Helper()
	if tags == nil {
		tags = []string{}
	}
	var storedSummary any = summary
	if kind == model.LibraryKindSite {
		storedSummary = nil
	}
	var id uuid.UUID
	url := "https://" + domain + "/" + uuid.NewString()
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO links (
			url, source_key, status, title, summary, tags, domain, content_type,
			library_kind, library_kind_locked, first_collected_at
		) VALUES ($1, $1, 'done', $2, $3, $4, $5, 'article', $6, true, NOW())
		RETURNING id`,
		url, title, storedSummary, tags, domain, kind,
	).Scan(&id); err != nil {
		t.Fatalf("seed search link %q: %v", title, err)
	}
	return id
}

func searchResultIDs(links []model.Link) map[uuid.UUID]struct{} {
	ids := make(map[uuid.UUID]struct{}, len(links))
	for _, link := range links {
		ids[link.ID] = struct{}{}
	}
	return ids
}

func stringPointer(value string) *string { return &value }

func TestKeywordSearchMatchesTitleSummaryAndTags(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)

	title := seedSearchLink(t, pool, "needle in title", "plain", "title.example", []string{"other"}, model.LibraryKindReading)
	summary := seedSearchLink(t, pool, "plain", "needle in summary", "summary.example", []string{"other"}, model.LibraryKindReading)
	tag := seedSearchLink(t, pool, "plain", "plain", "tag.example", []string{"needle-tag"}, model.LibraryKindReading)
	noise := seedSearchLink(t, pool, "plain", "plain", "noise.example", []string{"other"}, model.LibraryKindReading)

	links, total, err := repo.ListDone(context.Background(), repository.ListLinksFilter{Query: stringPointer("needle"), Limit: 50})
	if err != nil {
		t.Fatalf("ListDone keyword search: %v", err)
	}
	ids := searchResultIDs(links)
	for _, id := range []uuid.UUID{title, summary, tag} {
		if _, ok := ids[id]; !ok {
			t.Fatalf("matching link %s missing from results", id)
		}
	}
	if _, ok := ids[noise]; ok {
		t.Fatalf("non-matching link %s appeared in results", noise)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
}

func TestKeywordSearchAppliesCollectionDomainAndLimit(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)

	for range 3 {
		seedSearchLink(t, pool, "matching", "", "keep.example", nil, model.LibraryKindReading)
	}
	seedSearchLink(t, pool, "matching", "", "keep.example", nil, model.LibraryKindSite)
	seedSearchLink(t, pool, "matching", "", "drop.example", nil, model.LibraryKindReading)

	kind := model.LibraryKindReading
	links, total, err := repo.ListDone(context.Background(), repository.ListLinksFilter{
		Query:       stringPointer("matching"),
		Domain:      stringPointer("keep.example"),
		LibraryKind: &kind,
		Limit:       2,
	})
	if err != nil {
		t.Fatalf("ListDone filtered keyword search: %v", err)
	}
	if len(links) != 2 || total != 2 {
		t.Fatalf("filtered result count = %d, total = %d, want 2 / 2", len(links), total)
	}
	for _, link := range links {
		if link.Domain == nil || *link.Domain != "keep.example" || link.LibraryKind == nil || *link.LibraryKind != model.LibraryKindReading {
			t.Fatalf("filter leaked link: %+v", link)
		}
	}
}

func TestKeywordSearchTreatsLikeMetacharactersLiterally(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)

	wanted := seedSearchLink(t, pool, "100%_safe", "", "literal.example", nil, model.LibraryKindReading)
	seedSearchLink(t, pool, "100XXsafe", "", "wildcard.example", nil, model.LibraryKindReading)

	links, total, err := repo.ListDone(context.Background(), repository.ListLinksFilter{Query: stringPointer("%_"), Limit: 50})
	if err != nil {
		t.Fatalf("ListDone literal keyword search: %v", err)
	}
	ids := searchResultIDs(links)
	if _, ok := ids[wanted]; !ok || total != 1 {
		t.Fatalf("literal search returned ids=%v total=%d, want only %s", ids, total, wanted)
	}
}

func TestURLExactMatchReturnsZeroOrOne(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)

	url := "https://github.com/astral-sh/uv"
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO links (url, source_key, status, title, domain, content_type, first_collected_at)
		 VALUES ($1, $1, 'done', 'uv', 'github.com', 'article', NOW())`,
		url,
	); err != nil {
		t.Fatalf("seed url link: %v", err)
	}

	got, err := repo.GetByURL(context.Background(), url)
	if err != nil {
		t.Fatalf("GetByURL hit: %v", err)
	}
	if got == nil || got.URL != url {
		t.Fatalf("GetByURL hit = %+v, want %q", got, url)
	}

	miss, err := repo.GetByURL(context.Background(), "https://github.com/not/stored")
	if err != nil {
		t.Fatalf("GetByURL miss: %v", err)
	}
	if miss != nil {
		t.Fatalf("GetByURL miss = %+v, want nil", miss)
	}
}
