package dbintegration

import (
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/repository"
)

// TestReaderRelatedTagsCooccurrenceRunsAgainstPostgres executes the
// co-occurrence branch against a real database.
//
// It exists because the repository's own RelatedTags tests use pgxmock, which
// matches the query text and replays canned rows without ever asking Postgres
// to parse it. That let a query ship with `HAVING candidate <> ALL(...)` —
// invalid, because HAVING is evaluated before SELECT output aliases exist
// (GROUP BY may name the alias, HAVING may not) — and it only surfaced as a
// production 500 on the first request that carried real tags.
//
// The assertions therefore care about two things a mock cannot prove: that the
// statement parses at all, and that the seed tags are excluded from their own
// related set.
func TestReaderRelatedTagsCooccurrenceRunsAgainstPostgres(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)

	// The fixture is shaped so that each assertion below can actually fail:
	//   - `postgres` co-occurs twice, `redis` once, so ORDER BY uses DESC is
	//     observable. With a single surviving candidate the ordering clause
	//     could be reversed without any test noticing.
	//   - `unrelated` shares no tag with the seed, so "co-occurring tags" and
	//     "every tag in the installation" are
	//     distinguishable. Without it, dropping the `tags && $2` filter would
	//     go unnoticed.
	seed := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/related-seed", "Related seed", "Related seed body", "Related seed summary")
	neighbour := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/related-neighbour", "Related neighbour", "Related neighbour body", "Related neighbour summary")
	second := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/related-second", "Related second", "Related second body", "Related second summary")
	unrelated := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/related-unrelated", "Related unrelated", "Related unrelated body", "Related unrelated summary")

	setTags(t, pool, seed, []string{"ai", "golang"})
	setTags(t, pool, neighbour, []string{"ai", "postgres", "redis"})
	setTags(t, pool, second, []string{"ai", "postgres"})
	setTags(t, pool, unrelated, []string{"unrelated"})

	tags, model, degraded, err := reader.RelatedTags(ctx, &seed, 12)
	if err != nil {
		t.Fatalf("RelatedTags() error = %v", err)
	}
	if model != "cooccurrence-v1" || !degraded {
		t.Fatalf("RelatedTags() = model %q degraded %v, want the degraded cooccurrence path (no embedding configured)", model, degraded)
	}
	// postgres co-occurs twice and redis once, so the more frequent tag must
	// come first. This is the only assertion guarding ORDER BY uses DESC.
	wantOrder := []string{"postgres", "redis"}
	if !slices.Equal(tags, wantOrder) {
		t.Fatalf("RelatedTags() = %v, want exactly %v ordered by co-occurrence count", tags, wantOrder)
	}
	for _, seeded := range []string{"ai", "golang"} {
		if slices.Contains(tags, seeded) {
			t.Fatalf("RelatedTags() = %v, want the link's own tag %q excluded", tags, seeded)
		}
	}
	// Sharing no tag with the seed: reaching it would mean the query stopped
	// restricting to co-occurring links.
	if slices.Contains(tags, "unrelated") {
		t.Fatalf("RelatedTags() = %v, want %q excluded: it shares no tag with the seed", tags, "unrelated")
	}
}

// TestReaderRelatedTagsWithoutSeedTagsRunsAgainstPostgres covers the other
// branch of the same method — the no-seed query, which has a different shape
// (no exclusion list, no && filter).
//
// It is reached only with a nil link: a link that merely happens to carry no
// tags returns early with an empty set rather than falling through to the
// installation's most-used tags.
func TestReaderRelatedTagsWithoutSeedTagsRunsAgainstPostgres(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)

	other := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/related-popular", "Popular", "Popular body", "Popular summary")
	setTags(t, pool, other, []string{"popular"})

	tags, _, _, err := reader.RelatedTags(ctx, nil, 12)
	if err != nil {
		t.Fatalf("RelatedTags() error = %v", err)
	}
	if !slices.Contains(tags, "popular") {
		t.Fatalf("RelatedTags() = %v, want the installation's existing tag %q", tags, "popular")
	}
}

// setTags overwrites a seeded link's tags. seedReaderVNextSavedLink always
// inserts an empty array, and the co-occurrence query only considers links that
// actually carry tags.
func setTags(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, tags []string) {
	t.Helper()
	tag, err := pool.Exec(t.Context(), `UPDATE links SET tags=$2 WHERE id=$1`, id, tags)
	if err != nil {
		t.Fatalf("set tags on link %s: %v", id, err)
	}
	// A no-op UPDATE (wrong id) would leave
	// the fixture empty and send the real assertions chasing the wrong cause.
	if n := tag.RowsAffected(); n != 1 {
		t.Fatalf("set tags on link %s: updated %d rows, want 1", id, n)
	}
}
