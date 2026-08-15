package dbintegration

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// These tests use real PostgreSQL because pgxmock cannot validate multi-row
// INSERT arity, conflict-target resolution, or transaction rollback.
func TestLinkRepositorySubmitBatchInsertsRowsAndJobs(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)

	results, err := repo.SubmitBatch(t.Context(), []repository.CreateLinkParams{
		{URL: "https://batch.example.com/one", SourceKind: "url", SourceKey: "batch-one", Status: model.LinkStatusPending},
		{URL: "https://batch.example.com/two", SourceKind: "url", SourceKey: "batch-two", Status: model.LinkStatusPending},
	})
	if err != nil {
		t.Fatalf("SubmitBatch(new rows): %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	for index, result := range results {
		if !result.Inserted || result.Link == nil || result.Link.ID == uuid.Nil {
			t.Fatalf("results[%d] = %+v, want an inserted link", index, result)
		}
		if result.Job == nil || result.Job.LinkID != result.Link.ID || result.Job.Status != model.JobStatusPending {
			t.Fatalf("results[%d] job = %+v, want matching pending job", index, result.Job)
		}
	}
	if got := rawCountLinks(t, pool); got != 2 {
		t.Fatalf("links = %d, want 2", got)
	}
	if got := rawCountJobs(t, pool); got != 2 {
		t.Fatalf("parse jobs = %d, want 2", got)
	}
}

func TestLinkRepositorySubmitBatchReusesConflictWithoutNewJob(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)
	params := repository.CreateLinkParams{
		URL: "https://batch.example.com/duplicate", SourceKind: "url",
		SourceKey: "batch-duplicate", Status: model.LinkStatusPending,
	}

	first, err := repo.SubmitBatch(t.Context(), []repository.CreateLinkParams{params})
	if err != nil || len(first) != 1 || !first[0].Inserted || first[0].Link == nil {
		t.Fatalf("initial SubmitBatch() = %+v, %v", first, err)
	}
	second, err := repo.SubmitBatch(t.Context(), []repository.CreateLinkParams{params})
	if err != nil {
		t.Fatalf("conflicting SubmitBatch(): %v", err)
	}
	if len(second) != 1 || second[0].Inserted || second[0].Link == nil || second[0].Link.ID != first[0].Link.ID {
		t.Fatalf("conflicting SubmitBatch() = %+v, want existing link %s", second, first[0].Link.ID)
	}
	if second[0].Job != nil {
		t.Fatalf("conflicting SubmitBatch() job = %+v, want nil", second[0].Job)
	}
	if got := rawCountLinks(t, pool); got != 1 {
		t.Fatalf("links after conflict = %d, want 1", got)
	}
	if got := rawCountJobs(t, pool); got != 1 {
		t.Fatalf("parse jobs after conflict = %d, want 1", got)
	}
}

func TestLinkRepositorySubmitBatchPreservesMixedInputOrder(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)
	existing := repository.CreateLinkParams{
		URL: "https://batch.example.com/existing", SourceKind: "url",
		SourceKey: "batch-existing", Status: model.LinkStatusPending,
	}
	seeded, err := repo.SubmitBatch(t.Context(), []repository.CreateLinkParams{existing})
	if err != nil || len(seeded) != 1 || seeded[0].Link == nil {
		t.Fatalf("seed SubmitBatch() = %+v, %v", seeded, err)
	}

	items := []repository.CreateLinkParams{
		{URL: "https://batch.example.com/new-a", SourceKind: "url", SourceKey: "batch-new-a", Status: model.LinkStatusPending},
		existing,
		{URL: "https://batch.example.com/new-b", SourceKind: "url", SourceKey: "batch-new-b", Status: model.LinkStatusPending},
	}
	results, err := repo.SubmitBatch(t.Context(), items)
	if err != nil {
		t.Fatalf("mixed SubmitBatch(): %v", err)
	}
	if len(results) != len(items) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(items))
	}
	for index, result := range results {
		if result.Link == nil || result.Link.SourceKey != items[index].SourceKey {
			t.Fatalf("results[%d] = %+v, want source key %q", index, result, items[index].SourceKey)
		}
	}
	if !results[0].Inserted || results[1].Inserted || !results[2].Inserted {
		t.Fatalf("mixed Inserted flags = %v/%v/%v, want true/false/true", results[0].Inserted, results[1].Inserted, results[2].Inserted)
	}
	if results[1].Job != nil || results[0].Job == nil || results[2].Job == nil {
		t.Fatalf("mixed jobs = %+v/%+v/%+v", results[0].Job, results[1].Job, results[2].Job)
	}
}

func TestLinkRepositorySubmitBatchRestoresTrashInsideMixedBatch(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	reader := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()
	trashed := repository.CreateLinkParams{
		URL: "https://batch.example.com/trashed", SourceKind: "url",
		SourceKey: "batch-trashed", Status: model.LinkStatusPending,
	}
	seeded, err := links.SubmitBatch(ctx, []repository.CreateLinkParams{trashed})
	if err != nil || len(seeded) != 1 || seeded[0].Link == nil {
		t.Fatalf("seed SubmitBatch() = %+v, %v", seeded, err)
	}
	linkID := seeded[0].Link.ID
	if _, err := pool.Exec(ctx, `UPDATE links SET status='done' WHERE id=$1`, linkID); err != nil {
		t.Fatalf("terminalize seed link: %v", err)
	}
	if _, err := reader.SoftDeleteHost(ctx, model.ReaderHostLink, linkID); err != nil {
		t.Fatalf("trash seed link: %v", err)
	}

	items := []repository.CreateLinkParams{
		{URL: "https://batch.example.com/new-before", SourceKind: "url", SourceKey: "batch-new-before", Status: model.LinkStatusPending},
		trashed,
		{URL: "https://batch.example.com/new-after", SourceKind: "url", SourceKey: "batch-new-after", Status: model.LinkStatusPending},
	}
	results, err := links.SubmitBatch(ctx, items)
	if err != nil {
		t.Fatalf("mixed Trash SubmitBatch(): %v", err)
	}
	if len(results) != 3 || !results[0].Inserted || results[0].Job == nil || !results[2].Inserted || results[2].Job == nil {
		t.Fatalf("fresh mixed results = %+v", results)
	}
	if results[1].Link == nil || results[1].Link.ID != linkID || results[1].Inserted || !results[1].Restored || results[1].Job != nil {
		t.Fatalf("restored mixed result = %+v, want terminal link %s without reparse", results[1], linkID)
	}
	assertReaderFeedLinkLive(t, pool, linkID, true)
	if got := rawCountLinks(t, pool); got != 3 {
		t.Fatalf("links after mixed restore = %d, want 3", got)
	}
}

func TestLinkRepositorySubmitBatchDuplicateTrashInputsCreateOneReplacementAttempt(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	reader := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()
	params := repository.CreateLinkParams{
		URL: "https://batch.example.com/duplicate-trash", SourceKind: "url",
		SourceKey: "batch-duplicate-trash", Status: model.LinkStatusPending,
	}
	seeded, err := links.SubmitBatch(ctx, []repository.CreateLinkParams{params})
	if err != nil || len(seeded) != 1 || seeded[0].Link == nil || seeded[0].Job == nil {
		t.Fatalf("seed SubmitBatch() = %+v, %v", seeded, err)
	}
	linkID, oldJobID := seeded[0].Link.ID, seeded[0].Job.ID
	if _, err := reader.SoftDeleteHost(ctx, model.ReaderHostLink, linkID); err != nil {
		t.Fatalf("trash seed link: %v", err)
	}

	results, err := links.SubmitBatch(ctx, []repository.CreateLinkParams{params, params})
	if err != nil {
		t.Fatalf("duplicate Trash SubmitBatch(): %v", err)
	}
	if len(results) != 2 || results[0].Job == nil || results[1].Job == nil {
		t.Fatalf("duplicate Trash results = %+v, want two views of one replacement attempt", results)
	}
	if results[0].Link == nil || results[1].Link == nil ||
		results[0].Link.ID != linkID || results[1].Link.ID != linkID ||
		results[0].Job.ID != results[1].Job.ID || results[0].Job.ID == oldJobID ||
		!results[0].Restored || !results[1].Restored || results[0].Inserted || results[1].Inserted {
		t.Fatalf("duplicate Trash results = %+v, want shared restored Link %s and one new attempt", results, linkID)
	}
	var runnable, attempts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status IN ('pending','processing')),count(*) FROM parse_jobs WHERE link_id=$1`, linkID).Scan(&runnable, &attempts); err != nil {
		t.Fatalf("count restored attempts: %v", err)
	}
	if runnable != 1 || attempts != 2 {
		t.Fatalf("restored attempts = runnable %d / total %d, want 1 / 2", runnable, attempts)
	}
	assertReaderFeedLinkLive(t, pool, linkID, true)
}

func TestLinkRepositorySubmitBatchRollsBackOnEncodingFailure(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)

	_, err := repo.SubmitBatch(t.Context(), []repository.CreateLinkParams{
		{URL: "https://batch.example.com/would-write", SourceKind: "url", SourceKey: "batch-would-write", Status: model.LinkStatusPending},
		{
			URL: "https://batch.example.com/invalid", SourceKind: "url", SourceKey: "batch-invalid",
			Status: model.LinkStatusPending, SourceMetadata: map[string]any{"invalid": make(chan struct{})},
		},
	})
	if err == nil {
		t.Fatal("SubmitBatch() with non-JSON metadata succeeded")
	}
	if got := rawCountLinks(t, pool); got != 0 {
		t.Fatalf("links after failed batch = %d, want 0", got)
	}
	if got := rawCountJobs(t, pool); got != 0 {
		t.Fatalf("parse jobs after failed batch = %d, want 0", got)
	}
}

func rawCountJobs(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM parse_jobs`).Scan(&count); err != nil {
		t.Fatalf("count parse jobs: %v", err)
	}
	return count
}
