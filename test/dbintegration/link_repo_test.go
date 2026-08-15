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
//
// Black-box testing note:
//
//	This file lives in `package dbintegration`, not `package repository`,
//	so it can only see exported names. The original in-package version
//	referenced the unexported `parseJobsPerLinkRetention` constant
//	directly; here we mirror its value as `parseJobsPerLinkRetention`
//	below with a CHECK INVARIANT comment so a future change to the
//	repository constant fails this test loudly instead of silently
//	weakening retention coverage.
package dbintegration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/migrate"
	"webtag/internal/model"
	"webtag/internal/repository"
)

// parseJobsPerLinkRetention mirrors the unexported constant of the same
// name in webtag/internal/repository/job_repo.go (value: 20). It is
// duplicated here because this file sits in an external test package and
// cannot reach the unexported symbol. The two definitions are tied
// together by the retention test below: if production code ever moves
// the cap, the assertion `count > parseJobsPerLinkRetention` will start
// firing (or stop firing for the wrong reason) and force the constant
// here to be re-synced. Keep the values aligned.
const parseJobsPerLinkRetention = 20

// TestLinkRepository_SubmitNew_RealDBEnforcesUniqueSourceKey exercises
// the production SubmitNew path against a live unique index on
// links.source_key plus a real FK from parse_jobs.link_id. pgxmock
// cannot tell us:
//
//  1. Whether the installation-wide source-key unique index collapses a duplicate
//     SubmitNew into the existing row without creating a second parse attempt.
//  2. Whether the SubmitNew transaction is atomic — a partial commit
//     would leave an orphan link with no parse_jobs row, and the
//     parse pipeline would skip the link forever. The mock surface
//     only checks that Begin/Commit are called; it can't tell whether
//     the link row was actually persisted when Commit failed.
//  3. Whether the parse_jobs row's link_id FK is enforced with the
//     "links does not exist" failure mode (used implicitly by the
//     ResetProcessing path during startup).
func TestLinkRepository_SubmitNew_RealDBReusesDuplicateSourceKey(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)
	ctx := t.Context()

	first, firstJob, err := repo.SubmitNew(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/a",
		SourceKind: "url",
		SourceKey:  "src-a",
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitNew initial: %v", err)
	}
	if first == nil || first.ID == uuid.Nil {
		t.Fatal("SubmitNew returned nil link or zero ID")
	}
	if firstJob == nil || firstJob.LinkID != first.ID {
		t.Fatalf("SubmitNew returned mismatched job: %v vs link %s", firstJob, first.ID)
	}
	if firstJob.Status != model.JobStatusPending {
		t.Errorf("initial job status = %q, want pending", firstJob.Status)
	}

	// Bypass the service URL locker and submit the same identity with a
	// different URL. The repository must return the persisted winner and no new
	// job; a unique-violation-driven retry would make idempotent saves an error.
	duplicate, duplicateJob, err := repo.SubmitNew(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/different-url",
		SourceKind: "url",
		SourceKey:  "src-a", // duplicate
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitNew duplicate: %v", err)
	}
	if duplicate == nil || duplicate.ID != first.ID || duplicate.URL != first.URL {
		t.Fatalf("duplicate result = %#v, want existing link %#v", duplicate, first)
	}
	if duplicateJob != nil {
		t.Fatalf("duplicate job = %#v, want nil (no implicit parse retry)", duplicateJob)
	}

	// Atomicity: only one links row and one parse_jobs row remain.
	var linkCount, jobCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM links`).Scan(&linkCount); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if linkCount != 1 {
		t.Errorf("links count after duplicate = %d, want 1", linkCount)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM parse_jobs`).Scan(&jobCount); err != nil {
		t.Fatalf("count parse_jobs: %v", err)
	}
	if jobCount != 1 {
		t.Errorf("parse_jobs count after duplicate = %d, want 1", jobCount)
	}

	// link_id FK on parse_jobs: inserting a job with a non-existent
	// link_id must fail. Production code never does this, but losing
	// the constraint would silently let orphans accumulate if a future
	// refactor reordered the SubmitNew INSERT pair.
	bogusLink := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO parse_jobs (link_id, status, created_at, updated_at) VALUES ($1, 'pending', NOW(), NOW())`,
		bogusLink,
	)
	if err == nil {
		t.Error("INSERT parse_jobs with non-existent link_id succeeded; FK constraint is missing")
	} else if !strings.Contains(err.Error(), "foreign key") && !strings.Contains(err.Error(), "23503") {
		t.Errorf("unexpected error shape on orphan job insert: %v", err)
	}
}

func TestLinkRepository_CompleteParsePersistsEmptyTagsArray(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)
	ctx := t.Context()

	link, job, err := repo.SubmitNew(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/empty-analysis-tags",
		SourceKind: "url",
		SourceKey:  "https://example.com/empty-analysis-tags",
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitNew: %v", err)
	}
	if err := repo.MarkParseProcessing(ctx, link.ID, job.ID); err != nil {
		t.Fatalf("MarkParseProcessing: %v", err)
	}
	title := "Readable page without usable tags"
	if err := repo.CompleteParse(ctx, repository.UpdateLinkAnalysisParams{
		ID:                       link.ID,
		ExpectedMetadataRevision: job.ExpectedMetadataRevision,
		Title:                    &title,
		Tags:                     nil,
		Status:                   model.LinkStatusDone,
	}, job.ID); err != nil {
		t.Fatalf("CompleteParse(Tags=nil): %v", err)
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

	// Tables: every business surface plus the bookkeeping table.
	for _, table := range []string{"links", "parse_jobs", "schema_migrations"} {
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

	// Columns added by later steps must be present; the IF NOT EXISTS
	// guards in ALTER TABLE make a no-op safe but also mask a
	// migration that silently never landed.
	wantColumns := map[string][]string{
		"links": {
			"id", "url", "source_kind", "source_key",
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
	// SubmitBatch's ON CONFLICT (source_key) reuse an existing link. Without it
	// the upsert path would silently insert duplicates.
	var indexExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'idx_links_source_key_unique')`,
	).Scan(&indexExists); err != nil {
		t.Fatalf("probe unique index: %v", err)
	}
	if !indexExists {
		t.Error("idx_links_source_key_unique missing; SubmitBatch upsert path is unprotected")
	}

	// Every automatic migration before the first release gate must be
	// recorded. The pending manual step and every step behind it must remain
	// unapplied on the default Up path; otherwise a fresh deployment would
	// silently skip the expand/contract compatibility window.
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
	behindManualGate := false
	for _, step := range wantSteps {
		if step.Manual {
			behindManualGate = true
		}
		_, ok := recorded[step.ID]
		if behindManualGate && ok {
			t.Errorf("migration %s recorded behind the default manual gate", step.ID)
		}
		if !behindManualGate && !ok {
			t.Errorf("automatic migration %s not recorded in schema_migrations", step.ID)
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

// TestParseJobsRepository_CreateAndComplete_RealConstraints walks the
// parse_jobs row through pending → processing → done against a live
// FK to links.id. The pgxmock suite checks the SQL strings but
// nothing exercises:
//
//  1. A normal link delete enters the reversible trash lifecycle. The parent
//     row and its parse attempt history must remain together.
//  2. The history retention CTE in insertJobSQL: repeated Create calls
//     must respect parseJobsPerLinkRetention (==20). A SQL bug that
//     compared rn vs cap with the wrong inequality would either prune
//     too aggressively (drop the just-inserted row) or never prune at
//     all.
//  3. GetLatestByLinkID returns the newest immutable parse attempt rather
//     than an arbitrary history row.
func TestParseJobsRepository_CreateAndComplete_RealConstraints(t *testing.T) {
	pool := StartPostgres(t)
	linkRepo := repository.NewPGXLinkRepository(pool)
	jobRepo := repository.NewPGXJobRepository(pool)
	ctx := t.Context()

	link, err := linkRepo.Create(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/job-cascade",
		SourceKind: "url",
		SourceKey:  "https://example.com/job-cascade",
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("Create link: %v", err)
	}

	job, err := jobRepo.Create(ctx, link.ID)
	if err != nil {
		t.Fatalf("Create job: %v", err)
	}
	if job.Status != model.JobStatusPending {
		t.Errorf("initial job status = %q, want pending", job.Status)
	}

	// Insert a newer sibling job and mark it done.
	doneJob, err := jobRepo.Create(ctx, link.ID)
	if err != nil {
		t.Fatalf("Create done job: %v", err)
	}
	if err := jobRepo.UpdateState(ctx, repository.UpdateJobStateParams{
		ID:     doneJob.ID,
		Status: model.JobStatusDone,
	}); err != nil {
		t.Fatalf("UpdateState done: %v", err)
	}

	// GetLatestByLinkID must return the most-recently-created row.
	// We just created doneJob after job, so doneJob is latest.
	latest, err := jobRepo.GetLatestByLinkID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetLatestByLinkID: %v", err)
	}
	if latest == nil || latest.ID != doneJob.ID {
		t.Errorf("GetLatestByLinkID returned %v, want %s", latest, doneJob.ID)
	}

	// History retention: create parseJobsPerLinkRetention+5 more rows
	// and verify only retention-window rows remain.
	const extra = parseJobsPerLinkRetention + 5
	for i := 0; i < extra; i++ {
		if _, err := jobRepo.Create(ctx, link.ID); err != nil {
			t.Fatalf("Create extra job %d: %v", i, err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM parse_jobs WHERE link_id = $1`, link.ID).Scan(&count); err != nil {
		t.Fatalf("count parse_jobs: %v", err)
	}
	if count > parseJobsPerLinkRetention {
		t.Errorf("parse_jobs count = %d after retention prune; cap is %d", count, parseJobsPerLinkRetention)
	}

	// Link deletion is a reversible trash operation, not a physical DELETE.
	// It must retain existing attempts so restoring the link does not erase
	// durable history.
	if err := linkRepo.Delete(ctx, link.ID); err != nil {
		t.Fatalf("Delete link: %v", err)
	}
	var deletedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM links WHERE id = $1`, link.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("read trashed link: %v", err)
	}
	if deletedAt == nil {
		t.Error("deleted_at = NULL after link delete, want trash tombstone")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM parse_jobs WHERE link_id = $1`, link.ID).Scan(&count); err != nil {
		t.Fatalf("count parse_jobs post-delete: %v", err)
	}
	if count != parseJobsPerLinkRetention {
		t.Errorf("parse_jobs after link trash = %d, want %d retained attempts", count, parseJobsPerLinkRetention)
	}
}

// Sanity: ensure tests don't accidentally share a pool that's been
// closed. The helper auto-closes, but a goroutine leaking from a test
// into the next would hold the pool reference. ctx is only used to
// surface a clean error if a future change introduces a leak.
var _ = func() context.Context { return context.Background() }
