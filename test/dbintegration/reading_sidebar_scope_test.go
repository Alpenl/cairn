package dbintegration

import (
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

func TestReadingSidebarAggregatesExcludeSiteLinksAndCountDomainlessRows(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)

	readingID := mustCreateDoneLink(t, links, ctx, "https://shared.example/reading", "shared-tag", "shared.example")
	domainlessID := mustCreateDoneLink(t, links, ctx, "https://domainless.example/reading", "reading-only", "domainless.example")
	deletedID := mustCreateDoneLink(t, links, ctx, "https://deleted.example/reading", "deleted-only", "deleted.example")
	siteID := mustCreateDoneLink(t, links, ctx, "https://shared.example/site", "shared-tag", "shared.example")
	siteOnlyID := mustCreateDoneLink(t, links, ctx, "https://site-only.example/app", "site-only", "site-only.example")

	if _, err := pool.Exec(t.Context(), `UPDATE links SET library_kind = 'reading', library_kind_source = 'user' WHERE id = ANY($1::uuid[])`, []uuid.UUID{readingID, domainlessID, deletedID}); err != nil {
		t.Fatalf("mark reading fixtures: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE links SET library_kind = 'site', library_kind_source = 'user' WHERE id = ANY($1::uuid[])`, []uuid.UUID{siteID, siteOnlyID}); err != nil {
		t.Fatalf("mark site fixtures: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE links SET domain = NULL WHERE id = $1`, domainlessID); err != nil {
		t.Fatalf("clear reading domain: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE links SET deleted_at = NOW() WHERE id = $1`, deletedID); err != nil {
		t.Fatalf("soft-delete reading fixture: %v", err)
	}

	tree := repository.NewPGXTreeRepository(pool)
	readingDomains, err := tree.ListDomainsScoped(ctx, model.LibraryKindReading)
	if err != nil {
		t.Fatalf("ListDomainsScoped(reading): %v", err)
	}
	if readingDomains.Total != 2 {
		t.Fatalf("reading total = %d, want 2 including domainless row", readingDomains.Total)
	}
	if len(readingDomains.Domains) != 1 || readingDomains.Domains[0].Domain != "shared.example" || readingDomains.Domains[0].Count != 1 {
		t.Fatalf("reading domains = %#v, want shared.example/1 only", readingDomains.Domains)
	}

	legacyDomains, err := tree.ListDomains(ctx)
	if err != nil {
		t.Fatalf("ListDomains(legacy): %v", err)
	}
	if legacyDomains.Total != 4 {
		t.Fatalf("legacy total = %d, want all 4 done links", legacyDomains.Total)
	}

	tags := repository.NewPGXTagRepository(pool)
	readingTags, err := tags.ListScopedCounts(ctx, "reading")
	if err != nil {
		t.Fatalf("ListScopedCounts(reading): %v", err)
	}
	if hasScopedTag(readingTags, "site-only") || hasScopedTag(readingTags, "deleted-only") || !hasScopedTag(readingTags, "shared-tag") || !hasScopedTag(readingTags, "reading-only") {
		t.Fatalf("reading tags = %#v, want active reading tags without site-only or deleted-only", readingTags)
	}
}

func hasScopedTag(rows []repository.ScopedTagCount, tag string) bool {
	for _, row := range rows {
		if row.Tag == tag {
			return true
		}
	}
	return false
}
