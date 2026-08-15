// link_search_test.go exercises the Phase 9 hybrid search + url= existence
// check against a real pgvector-enabled Postgres. The pure-Go unit tests in
// internal/service cover q/url routing + degradation against in-memory fakes;
// this file is the only place the actual semantic∪ILIKE merge SQL (cosine
// nearest-neighbour, row_number ranking, UNION dedup) and the url= equality
// query run against pgvector. It validates the DEV-PLAN Phase 9 acceptance:
// semantic hits rank before ILIKE-only hits, ILIKE catches what the vector
// misses, filters apply to both legs, and url= returns 0/1.
package dbintegration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"webtag/internal/model"
	"webtag/internal/repository"
)

const searchTestModel = "search-embed-model"

// seedSearchLink inserts a done link with the given title/summary/tags/domain
// and (optionally) a content vector under searchTestModel. A nil vec leaves the
// embedding NULL (the row only participates in the ILIKE leg). Returns the id.
func seedSearchLink(t *testing.T, pool *pgxpool.Pool, title, summary, domain string, tags []string, vec []float32) uuid.UUID {
	t.Helper()
	url := "https://" + domain + "/" + title
	var (
		id  uuid.UUID
		err error
	)
	if vec == nil {
		err = pool.QueryRow(context.Background(),
			`INSERT INTO links (url, source_key, status, title, summary, tags, domain, content_type, first_collected_at)
			 VALUES ($1, $1, 'done', $2, $3, $4, $5, 'article', NOW()) RETURNING id`,
			url, title, summary, tags, domain,
		).Scan(&id)
	} else {
		err = pool.QueryRow(context.Background(),
			`INSERT INTO links (url, source_key, status, title, summary, tags, domain, content_type, embedding, embedding_model, first_collected_at)
			 VALUES ($1, $1, 'done', $2, $3, $4, $5, 'article', $6, $7, NOW()) RETURNING id`,
			url, title, summary, tags, domain, pgvector.NewVector(vec), searchTestModel,
		).Scan(&id)
	}
	if err != nil {
		t.Fatalf("seed search link %q: %v", title, err)
	}
	return id
}

// unitVec returns a 1536-d unit vector along basis component i.
func unitVec(i int) []float32 {
	v := make([]float32, 1536)
	v[i] = 1
	return v
}

func idSet(links []model.Link) map[uuid.UUID]int {
	m := make(map[uuid.UUID]int, len(links))
	for pos, l := range links {
		m[l.ID] = pos
	}
	return m
}

func strptr(s string) *string { return &s }

// TestHybridSearchSemanticRanksBeforeKeyword: a query vector near the "near"
// link's vector but textually matching only the "keyword" link must return
// both, with the semantic hit ranked first.
func TestHybridSearchSemanticRanksBeforeKeyword(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)

	// near: vector == query basis e0, title/summary do NOT contain "pgvector".
	near := seedSearchLink(t, pool, "Approximate nearest neighbour indexes", "HNSW recall tuning", "blog.example", []string{"ann"}, unitVec(0))
	// keyword: vector far from query (basis e5), but title contains "pgvector".
	keyword := seedSearchLink(t, pool, "pgvector quickstart", "getting started", "docs.example", []string{"db"}, unitVec(5))
	// noise: neither semantic-near nor keyword match — must not appear.
	seedSearchLink(t, pool, "unrelated cooking recipe", "pasta", "food.example", []string{"food"}, unitVec(9))

	q := "pgvector"
	filter := repository.ListLinksFilter{
		Query:          strptr(q),
		QueryEmbedding: unitVec(0), // nearest to `near`
		EmbeddingModel: searchTestModel,
	}
	links, total, err := repo.ListDone(context.Background(), filter)
	if err != nil {
		t.Fatalf("ListDone hybrid: %v", err)
	}
	pos := idSet(links)
	if _, ok := pos[near]; !ok {
		t.Fatalf("semantic-near link missing from results (ids=%v)", pos)
	}
	if _, ok := pos[keyword]; !ok {
		t.Fatalf("keyword link missing from results (ids=%v)", pos)
	}
	if pos[near] >= pos[keyword] {
		t.Fatalf("semantic hit must rank before keyword-only hit: near=%d keyword=%d", pos[near], pos[keyword])
	}
	if total != len(links) {
		t.Fatalf("total %d != len(items) %d (q= mode reports item count)", total, len(links))
	}
}

// TestHybridSearchKeywordCatchesVectorlessRow: a row with NO embedding still
// surfaces via the ILIKE leg.
func TestHybridSearchKeywordCatchesVectorlessRow(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)

	// Vector-less row whose tags carry the query term.
	vectorless := seedSearchLink(t, pool, "Rust ownership model", "borrow checker", "rust.example", []string{"rustlang"}, nil)
	// A semantically-near row to confirm the semantic leg still runs.
	near := seedSearchLink(t, pool, "memory safety deep dive", "no GC", "blog.example", []string{"systems"}, unitVec(0))

	filter := repository.ListLinksFilter{
		Query:          strptr("rustlang"),
		QueryEmbedding: unitVec(0),
		EmbeddingModel: searchTestModel,
	}
	links, _, err := repo.ListDone(context.Background(), filter)
	if err != nil {
		t.Fatalf("ListDone hybrid: %v", err)
	}
	pos := idSet(links)
	if _, ok := pos[vectorless]; !ok {
		t.Fatalf("ILIKE leg must surface the vector-less row; ids=%v", pos)
	}
	if _, ok := pos[near]; !ok {
		t.Fatalf("semantic leg row missing; ids=%v", pos)
	}
}

// TestHybridSearchPureILIKEFallback: with no query vector the search degrades to
// pure ILIKE — semantic-only rows (matching neither title/summary/tags) drop
// out, keyword matches remain.
func TestHybridSearchPureILIKEFallback(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)

	keyword := seedSearchLink(t, pool, "pgvector tuning guide", "index params", "docs.example", []string{"db"}, unitVec(0))
	// Semantic-only row: vector present but text does not contain the query.
	semanticOnly := seedSearchLink(t, pool, "unrelated title", "unrelated body", "x.example", []string{"misc"}, unitVec(0))

	filter := repository.ListLinksFilter{
		Query: strptr("pgvector"),
		// QueryEmbedding nil → pure ILIKE.
	}
	links, _, err := repo.ListDone(context.Background(), filter)
	if err != nil {
		t.Fatalf("ListDone ILIKE: %v", err)
	}
	pos := idSet(links)
	if _, ok := pos[keyword]; !ok {
		t.Fatalf("keyword row must appear in pure-ILIKE fallback; ids=%v", pos)
	}
	if _, ok := pos[semanticOnly]; ok {
		t.Fatalf("semantic-only row must NOT appear when there is no query vector; ids=%v", pos)
	}
}

// TestHybridSearchFilterAppliesToBothLegs: a domain filter restricts both the
// semantic and ILIKE legs.
func TestHybridSearchFilterAppliesToBothLegs(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)

	wanted := seedSearchLink(t, pool, "pgvector on keep", "kept", "keep.example", []string{"db"}, unitVec(0))
	// Same query relevance but a different domain — must be filtered out of both legs.
	dropped := seedSearchLink(t, pool, "pgvector on drop", "dropped", "drop.example", []string{"db"}, unitVec(0))

	filter := repository.ListLinksFilter{
		Query:          strptr("pgvector"),
		QueryEmbedding: unitVec(0),
		EmbeddingModel: searchTestModel,
		Domain:         strptr("keep.example"),
	}
	links, _, err := repo.ListDone(context.Background(), filter)
	if err != nil {
		t.Fatalf("ListDone filtered hybrid: %v", err)
	}
	pos := idSet(links)
	if _, ok := pos[wanted]; !ok {
		t.Fatalf("filtered domain row must appear; ids=%v", pos)
	}
	if _, ok := pos[dropped]; ok {
		t.Fatalf("row outside the domain filter must be excluded from both legs; ids=%v", pos)
	}
}

// TestHybridSearchCrossModelVectorsExcludedFromSemanticLeg: a row embedded
// under a different model must not participate in the semantic leg (but can
// still match via ILIKE).
func TestHybridSearchCrossModelVectorsExcludedFromSemanticLeg(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)

	// Row embedded under a stale model, text does NOT match the query → it
	// would only qualify via the semantic leg, which the model guard excludes.
	var staleID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO links (url, source_key, status, title, summary, tags, domain, content_type, embedding, embedding_model, first_collected_at)
		 VALUES ($1, $1, 'done', 'stale model row', 'no keyword here', $2, 'stale.example', 'article', $3, 'OTHER-MODEL', NOW()) RETURNING id`,
		"https://stale.example/x", []string{"misc"}, pgvector.NewVector(unitVec(0)),
	).Scan(&staleID); err != nil {
		t.Fatalf("seed stale-model link: %v", err)
	}

	filter := repository.ListLinksFilter{
		Query:          strptr("nonexistent-keyword-zzz"),
		QueryEmbedding: unitVec(0),
		EmbeddingModel: searchTestModel,
	}
	links, _, err := repo.ListDone(context.Background(), filter)
	if err != nil {
		t.Fatalf("ListDone hybrid: %v", err)
	}
	if pos := idSet(links); func() bool { _, ok := pos[staleID]; return ok }() {
		t.Fatalf("cross-model vector must be excluded from the semantic leg; got it in results")
	}
}

// TestURLExactMatchReturnsZeroOrOne: url= returns the single matching link, and
// an empty result for an unknown URL.
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

	// Hit: GetByURL returns the row.
	got, err := repo.GetByURL(context.Background(), url)
	if err != nil {
		t.Fatalf("GetByURL hit: %v", err)
	}
	if got == nil || got.URL != url {
		t.Fatalf("url= hit should return the stored link; got %+v", got)
	}

	// Miss: an unknown URL returns (nil, nil).
	miss, err := repo.GetByURL(context.Background(), "https://github.com/not/stored")
	if err != nil {
		t.Fatalf("GetByURL miss: %v", err)
	}
	if miss != nil {
		t.Fatalf("url= miss should return nil; got %+v", miss)
	}
}
