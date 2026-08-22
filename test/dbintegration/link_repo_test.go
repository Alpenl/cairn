// link_repo_test.go pairs `webtag/internal/repository` against a real
// PostgreSQL container booted via testcontainers-go. The pgxmock suite
// inside `internal/repository` covers SQL string + arg-order invariants
// but cannot catch the categories that only a live planner / row store
// produce: real ON CONFLICT-on-unique-index resolution, NOT NULL /
// CHECK constraint violations, text encoding round-trips through PG's
// catalogs, and migration drift between go-defined schema and the
// runtime DDL.
//
// Each test in this file documents in its preamble exactly which
// "pgxmock can't catch this" failure mode it exercises so a future
// reader can decide whether a new test belongs here or in the cheaper
// pgxmock surface in the main module.
package dbintegration

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/migrate"
	"webtag/internal/model"
	"webtag/internal/repository"
)

// TestLinkRepositorySubmitTxRealDBReusesDuplicateSourceKey verifies the
// repository's identity decision against PostgreSQL's real unique index.
func TestLinkRepositorySubmitTxRealDBReusesDuplicateSourceKey(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)
	ctx := t.Context()

	first, firstAttempt, err := submitLinkForTest(ctx, pool, repo, repository.CreateLinkParams{
		URL:        "https://example.com/a",
		SourceKind: "url",
		SourceKey:  "src-a",
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitTx initial: %v", err)
	}
	if first == nil || firstAttempt == nil || firstAttempt.LinkID != first.ID {
		t.Fatalf("SubmitTx returned Link/attempt = %#v/%#v", first, firstAttempt)
	}

	duplicate, duplicateAttempt, err := submitLinkForTest(ctx, pool, repo, repository.CreateLinkParams{
		URL:        "https://example.com/different-url",
		SourceKind: "url",
		SourceKey:  "src-a", // duplicate
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitTx duplicate: %v", err)
	}
	if duplicate == nil || duplicate.ID != first.ID || duplicate.URL != first.URL {
		t.Fatalf("duplicate result = %#v, want existing link %#v", duplicate, first)
	}
	if duplicateAttempt != nil {
		t.Fatalf("duplicate attempt = %#v, want nil (no implicit parse retry)", duplicateAttempt)
	}

	var linkCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM links`).Scan(&linkCount); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if linkCount != 1 {
		t.Errorf("links count after duplicate = %d, want 1", linkCount)
	}
}

func TestLinkRepository_CompleteParsePersistsEmptyTagsArray(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)
	ctx := t.Context()

	link, attemptRef, err := submitLinkForTest(ctx, pool, repo, repository.CreateLinkParams{
		URL:        "https://example.com/empty-analysis-tags",
		SourceKind: "url",
		SourceKey:  "https://example.com/empty-analysis-tags",
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitTx: %v", err)
	}
	attempt, err := requireParseAttempt(link, attemptRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkParseProcessing(ctx, attempt); err != nil {
		t.Fatalf("MarkParseProcessing: %v", err)
	}
	title := "Readable page without usable tags"
	if _, err := repo.CompleteReadingParse(ctx, repository.CompleteReadingParseParams{
		Analysis: repository.UpdateLinkAnalysisParams{
			ID: link.ID, ExpectedParseGeneration: attempt.Generation,
			ExpectedMetadataRevision: attempt.ExpectedMetadataRevision,
			Title:                    &title, Tags: nil, Status: model.LinkStatusDone,
		},
		Classification: repository.UpdateLibraryClassificationParams{
			ID: link.ID, Kind: model.LibraryKindReading,
		},
	}); err != nil {
		t.Fatalf("CompleteReadingParse(Tags=nil): %v", err)
	}

	var tags []string
	if err := pool.QueryRow(ctx, `SELECT tags FROM links WHERE id = $1`, link.ID).Scan(&tags); err != nil {
		t.Fatalf("select persisted tags: %v", err)
	}
	if tags == nil {
		t.Fatal("persisted tags = NULL, want an empty array")
	}
	if len(tags) != 0 {
		t.Fatalf("persisted tags = %#v, want []", tags)
	}
}

// TestLinkRepository_MigrationsUp_AllSchemasApplied re-runs migrate.Up
// against the live database, then queries pg_catalog to confirm every
// schema object the production code depends on is present. pgxmock
// validates the *Go* migration code path, but it cannot tell us
// whether the SQL the code emits is actually accepted by the planner,
// produces the indexes the read queries depend on, or installs the
// foreign keys the cascading-delete tests rely on. Migration drift —
// a step that succeeds in CREATE TABLE IF NOT EXISTS form but never
// actually applies the new column because of a typo in ADD COLUMN —
// only surfaces when something downstream reads the catalog.
func TestLinkRepository_MigrationsUp_AllSchemasApplied(t *testing.T) {
	// This test verifies the default release-gated Up path, so it must use a
	// database that has not been provisioned through the fresh-install tail.
	dsn := isolatedMigrationDatabase(t)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open isolated migration database: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx := t.Context()

	// Re-running Up must be idempotent. If the second pass fails the
	// schema_migrations bookkeeping has drifted from the live DDL.
	if err := migrate.Up(ctx, pool); err != nil {
		t.Fatalf("migrate.Up re-run: %v", err)
	}

	// Tables: the current business surface plus the bookkeeping table. Parse
	// execution history belongs to River now; the old mirror table must stay
	// absent after the simplification migration.
	for _, table := range []string{"links", "schema_migrations"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
			table,
		).Scan(&exists); err != nil {
			t.Fatalf("probe table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("expected table %s to exist after migrations", table)
		}
	}
	var parseJobsExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'parse_jobs')`,
	).Scan(&parseJobsExists); err != nil {
		t.Fatalf("probe removed parse_jobs table: %v", err)
	}
	if parseJobsExists {
		t.Error("parse_jobs must be absent after the parse-state simplification migration")
	}

	// Columns added by later steps must be present; the IF NOT EXISTS
	// guards in ALTER TABLE make a no-op safe but also mask a
	// migration that silently never landed.
	wantColumns := map[string][]string{
		"links": {
			"id", "url", "source_kind", "source_key",
			"parse_generation",
			"input_title", "input_text", "input_html",
			"input_images", "source_metadata",
			"description", "is_low_confidence", "low_confidence_reason",
			"domain", "content_type", "path_depth", "parent_path", "parent_id",
		},
	}
	for table, cols := range wantColumns {
		for _, col := range cols {
			var exists bool
			if err := pool.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
				table, col,
			).Scan(&exists); err != nil {
				t.Fatalf("probe column %s.%s: %v", table, col, err)
			}
			if !exists {
				t.Errorf("expected column %s.%s to exist", table, col)
			}
		}
	}

	var tagsNullable, tagsDefault string
	if err := pool.QueryRow(ctx,
		`SELECT is_nullable, column_default
		 FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'links' AND column_name = 'tags'`,
	).Scan(&tagsNullable, &tagsDefault); err != nil {
		t.Fatalf("probe links.tags constraints: %v", err)
	}
	if tagsNullable != "NO" {
		t.Errorf("links.tags is_nullable = %q, want NO", tagsNullable)
	}
	if !strings.Contains(tagsDefault, "{}") {
		t.Errorf("links.tags default = %q, want empty text array", tagsDefault)
	}

	// The installation-wide source_key unique index is what makes
	// SubmitTx's ON CONFLICT (source_key) reuses an existing link. Without it
	// the upsert path would silently insert duplicates.
	var indexExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'idx_links_source_key_unique')`,
	).Scan(&indexExists); err != nil {
		t.Fatalf("probe unique index: %v", err)
	}
	if !indexExists {
		t.Error("idx_links_source_key_unique missing; SubmitTx conflict path is unprotected")
	}

	// The default path records the single current schema head.
	wantSteps := migrate.Steps()
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()
	recorded := make(map[string]struct{})
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan schema_migrations: %v", err)
		}
		recorded[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema_migrations: %v", err)
	}
	for _, step := range wantSteps {
		if _, ok := recorded[step.ID]; !ok {
			t.Errorf("current migration %s not recorded in schema_migrations", step.ID)
		}
	}
}

// TestLinkRepository_GetByURL_RoundTripPreservesEncoding inserts a link
// with non-ASCII URL, title, summary, and tags, then reads it back
// through every read path (GetByID, GetByURL, GetBySourceKey,
// GetBySourceKeyOrURL) to confirm PG's text encoding round-trips
// without mangling.
//
// pgxmock returns whatever you put into AddRow byte-for-byte, so a
// real bug in the JSONB→text bridge (e.g. forgetting to set client
// encoding, or a CONVERT_FROM mismatch on tags[]) would never surface
// in the mock-based suite. Here we read from the actual cluster and
// assert byte-equality.
func TestLinkRepository_GetByURL_RoundTripPreservesEncoding(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)
	ctx := t.Context()

	// Mix CJK, Cyrillic, emoji, and a percent-encoded path segment in
	// one URL. Tags array also carries CJK + emoji to exercise the
	// text[] codec path independently of plain TEXT columns.
	url := "https://例え.テスト/路径/тест/🎉/page?q=%E4%B8%AD%E6%96%87"
	title := "你好世界 — Привет — 🌏 ready"
	summary := "Mixed: 中文 + Русский + 𝕊𝕡𝕖𝕔𝕚𝕒𝕝 + 🚀"
	tags := []string{"中文", "русский", "🚀rocket", "ASCII"}

	link, err := repo.Create(ctx, repository.CreateLinkParams{
		URL:        url,
		SourceKind: "url",
		SourceKey:  url,
		Status:     model.LinkStatusDone,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Backfill analysis fields so tags/title/summary land alongside
	// the URL. Done in a second step because Create-time params don't
	// expose them (mirrors the real ingest pipeline).
	if err := repo.UpdateAnalysis(ctx, repository.UpdateLinkAnalysisParams{
		ID:         link.ID,
		SourceKind: "url",
		SourceKey:  url,
		Title:      &title,
		Summary:    &summary,
		Tags:       tags,
		Status:     model.LinkStatusDone,
	}); err != nil {
		t.Fatalf("UpdateAnalysis: %v", err)
	}

	// Read back through every path. Each must observe identical bytes.
	cases := []struct {
		name string
		get  func() (*model.Link, error)
	}{
		{"GetByID", func() (*model.Link, error) { return repo.GetByID(ctx, link.ID) }},
		{"GetByURL", func() (*model.Link, error) { return repo.GetByURL(ctx, url) }},
		{"GetBySourceKey", func() (*model.Link, error) { return repo.GetBySourceKey(ctx, url) }},
		{"GetBySourceKeyOrURL", func() (*model.Link, error) { return repo.GetBySourceKeyOrURL(ctx, url, url) }},
	}
	for _, tc := range cases {
		got, err := tc.get()
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got == nil {
			t.Errorf("%s: nil link", tc.name)
			continue
		}
		if got.URL != url {
			t.Errorf("%s: URL mismatch\n got: %q\nwant: %q", tc.name, got.URL, url)
		}
		if got.Title == nil || *got.Title != title {
			t.Errorf("%s: Title mismatch\n got: %v\nwant: %q", tc.name, got.Title, title)
		}
		if got.Summary == nil || *got.Summary != summary {
			t.Errorf("%s: Summary mismatch\n got: %v\nwant: %q", tc.name, got.Summary, summary)
		}
		if len(got.Tags) != len(tags) {
			t.Errorf("%s: Tags length = %d, want %d", tc.name, len(got.Tags), len(tags))
			continue
		}
		for i := range tags {
			if got.Tags[i] != tags[i] {
				t.Errorf("%s: Tags[%d] mismatch\n got: %q\nwant: %q", tc.name, i, got.Tags[i], tags[i])
			}
		}
	}
}
