package dbintegration

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/app/durablework"
	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
	"webtag/internal/service/urllock"
)

type delayedURLMissReader struct {
	repository.LinkReader
	readDone chan struct{}
	release  chan struct{}
}

func (r *delayedURLMissReader) GetSubmitLookupByURL(ctx context.Context, rawURL string) (*repository.LinkSubmitLookup, error) {
	link, err := r.LinkReader.GetSubmitLookupByURL(ctx, rawURL)
	close(r.readDone)
	select {
	case <-r.release:
		return link, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type countingSubmitQueue struct {
	enqueueCalls   atomic.Int32
	enqueueTxCalls atomic.Int32
	enqueueErr     error
	cancelErr      error
}

type blockingFirstSubmitQueue struct {
	count   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (q *blockingFirstSubmitQueue) Enqueue(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

func (q *blockingFirstSubmitQueue) EnqueueTx(ctx context.Context, _ pgx.Tx, _ uuid.UUID, _ uuid.UUID) error {
	if q.count.Add(1) != 1 {
		return nil
	}
	close(q.started)
	select {
	case <-q.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *blockingFirstSubmitQueue) CancelActiveTx(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) error {
	return nil
}

func (q *blockingFirstSubmitQueue) CancelAllActiveTx(context.Context, pgx.Tx, uuid.UUID) error {
	return nil
}

func (q *countingSubmitQueue) Enqueue(context.Context, uuid.UUID, uuid.UUID) error {
	q.enqueueCalls.Add(1)
	return nil
}

func (q *countingSubmitQueue) EnqueueTx(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) error {
	q.enqueueTxCalls.Add(1)
	return q.enqueueErr
}

func (q *countingSubmitQueue) CancelActiveTx(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) error {
	return q.cancelErr
}

func (q *countingSubmitQueue) CancelAllActiveTx(context.Context, pgx.Tx, uuid.UUID) error {
	return q.cancelErr
}

func dbLinkCommands(pool *pgxpool.Pool, links *repository.PGXLinkRepository, queue durablework.LinkQueue) *durablework.LinkCommands {
	return durablework.NewLinkCommands(durablework.LinkCommandsOptions{
		Transactions: pool,
		Links:        links,
		Queue:        queue,
	})
}

func TestLinkRepositorySubmitBatchDifferentURLsDoNotDeadlockOnRepresentationGate(t *testing.T) {
	pool := StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Hold the exclusive gate so both SubmitBatch calls queue at their first
	// representation lock. Releasing it grants compatible shared waiters as a
	// group; an implementation that then upgrades each transaction to exclusive
	// deadlocks, while direct exclusive acquisition serializes successfully.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin representation gate blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(ctx, `SELECT lock_representation_write_gate_exclusive()`); err != nil {
		t.Fatalf("lock exclusive representation gate: %v", err)
	}

	type submitCase struct {
		applicationName string
		url             string
	}
	cases := []submitCase{
		{applicationName: "submit_gate_first", url: "https://concurrency.example.com/gate-first"},
		{applicationName: "submit_gate_second", url: "https://concurrency.example.com/gate-second"},
	}
	type submitOutcome struct {
		url     string
		results []repository.LinkSubmitResult
		err     error
	}
	outcomes := make(chan submitOutcome, len(cases))
	for _, tc := range cases {
		tc := tc
		submitPool := openNamedPool(t, tc.applicationName)
		submitRepo := repository.NewPGXLinkRepository(submitPool)
		go func() {
			results, submitErr := submitRepo.SubmitBatch(ctx, []repository.CreateLinkParams{{
				URL: tc.url, SourceKind: "url", SourceKey: tc.url, Status: model.LinkStatusPending,
			}})
			outcomes <- submitOutcome{url: tc.url, results: results, err: submitErr}
		}()
	}
	for _, tc := range cases {
		waitForPostgresLock(t, ctx, pool, tc.applicationName)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("release representation gate blocker: %v", err)
	}

	links := make(map[uuid.UUID]struct{}, len(cases))
	attempts := make(map[uuid.UUID]struct{}, len(cases))
	for range cases {
		outcome := <-outcomes
		assertNotDeadlock(t, "SubmitBatch("+outcome.url+")", outcome.err)
		if outcome.err != nil {
			t.Fatalf("SubmitBatch(%q): %v", outcome.url, outcome.err)
		}
		if len(outcome.results) != 1 || outcome.results[0].Link == nil || outcome.results[0].Job == nil || !outcome.results[0].Inserted {
			t.Fatalf("SubmitBatch(%q) = %#v, want one inserted Link with an attempt", outcome.url, outcome.results)
		}
		result := outcome.results[0]
		if result.Link.URL != outcome.url || result.Job.LinkID != result.Link.ID || result.Job.Status != model.JobStatusPending {
			t.Fatalf("SubmitBatch(%q) Link/attempt = %#v/%#v", outcome.url, result.Link, result.Job)
		}
		links[result.Link.ID] = struct{}{}
		attempts[result.Job.ID] = struct{}{}
	}
	if len(links) != len(cases) || len(attempts) != len(cases) {
		t.Fatalf("distinct committed Link/attempt counts = %d/%d, want %d/%d", len(links), len(attempts), len(cases), len(cases))
	}
}

func TestLinkRepository_SubmitNew_RealDB_ReusesBatchConflict(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)
	ctx := t.Context()
	params := repository.CreateLinkParams{
		URL:        "https://concurrency.example.com/shared",
		SourceKind: "url",
		SourceKey:  "https://concurrency.example.com/shared",
		Status:     model.LinkStatusPending,
	}

	batch, err := repo.SubmitBatch(ctx, []repository.CreateLinkParams{params})
	if err != nil {
		t.Fatalf("SubmitBatch() error = %v", err)
	}
	if len(batch) != 1 || !batch[0].Inserted || batch[0].Job == nil {
		t.Fatalf("SubmitBatch() = %#v, want one inserted link and job", batch)
	}

	link, job, err := repo.SubmitNew(ctx, params)
	if err != nil {
		t.Fatalf("SubmitNewTx() after batch conflict error = %v", err)
	}
	if link == nil || link.ID != batch[0].Link.ID {
		t.Fatalf("SubmitNewTx() link = %#v, want existing id %s", link, batch[0].Link.ID)
	}
	if job != nil {
		t.Fatalf("SubmitNewTx() job = %#v, want nil for existing link", job)
	}
	if got := rawCountLinks(t, pool); got != 1 {
		t.Fatalf("links after conflict = %d, want 1", got)
	}
	if got := rawCountJobs(t, pool); got != 1 {
		t.Fatalf("parse jobs after conflict = %d, want 1", got)
	}
}

func TestLinkRepository_SubmitBatch_RealDB_ConflictDoesNotRewriteLink(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)
	ctx := t.Context()
	params := repository.CreateLinkParams{
		URL:        "https://concurrency.example.com/no-rewrite",
		SourceKind: "url",
		SourceKey:  "https://concurrency.example.com/no-rewrite",
		Status:     model.LinkStatusPending,
	}

	first, err := repo.SubmitBatch(ctx, []repository.CreateLinkParams{params})
	if err != nil {
		t.Fatalf("first SubmitBatch() error = %v", err)
	}
	if len(first) != 1 || !first[0].Inserted {
		t.Fatalf("first SubmitBatch() = %#v, want inserted row", first)
	}

	var beforeXmin, beforeCTID string
	if err := pool.QueryRow(ctx,
		`SELECT xmin::text, ctid::text FROM links WHERE id = $1`,
		first[0].Link.ID,
	).Scan(&beforeXmin, &beforeCTID); err != nil {
		t.Fatalf("read original row version: %v", err)
	}

	second, err := repo.SubmitBatch(ctx, []repository.CreateLinkParams{params})
	if err != nil {
		t.Fatalf("second SubmitBatch() error = %v", err)
	}
	if len(second) != 1 || second[0].Inserted || second[0].Link.ID != first[0].Link.ID {
		t.Fatalf("second SubmitBatch() = %#v, want existing row", second)
	}

	var afterXmin, afterCTID string
	if err := pool.QueryRow(ctx,
		`SELECT xmin::text, ctid::text FROM links WHERE id = $1`,
		first[0].Link.ID,
	).Scan(&afterXmin, &afterCTID); err != nil {
		t.Fatalf("read repeated row version: %v", err)
	}
	if afterXmin != beforeXmin || afterCTID != beforeCTID {
		t.Fatalf(
			"repeat save rewrote link row: xmin/ctid %s/%s -> %s/%s",
			beforeXmin,
			beforeCTID,
			afterXmin,
			afterCTID,
		)
	}
}

func TestSubmitService_RealDB_SingleReusesBatchInsertedDuringMissWindow(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	jobs := repository.NewPGXJobRepository(pool)
	ctx := t.Context()
	const rawURL = "https://concurrency.example.com/single-vs-batch"

	reader := &delayedURLMissReader{
		LinkReader: links,
		readDone:   make(chan struct{}),
		release:    make(chan struct{}),
	}
	queue := &countingSubmitQueue{}
	commands := dbLinkCommands(pool, links, queue)
	submit := service.NewSubmitService(
		reader,
		jobs,
		commands,
		urllock.NewInProcessURLLocker(),
		service.SubmitServiceOptions{},
	)

	type submitOutcome struct {
		response dto.SubmitResponse
		err      error
		panicVal any
	}
	outcomes := make(chan submitOutcome, 1)
	go func() {
		out := submitOutcome{}
		defer func() {
			out.panicVal = recover()
			outcomes <- out
		}()
		out.response, out.err = submit.Submit(ctx, dto.LinkCreateRequest{URL: rawURL})
	}()

	<-reader.readDone
	batch, err := links.SubmitBatch(ctx, []repository.CreateLinkParams{{
		URL: rawURL, SourceKind: "url", SourceKey: rawURL, Status: model.LinkStatusPending,
	}})
	if err != nil {
		t.Fatalf("concurrent SubmitBatch() error = %v", err)
	}
	if len(batch) != 1 || !batch[0].Inserted || batch[0].Job == nil {
		t.Fatalf("concurrent SubmitBatch() = %#v, want inserted link and attempt", batch)
	}
	close(reader.release)

	out := <-outcomes
	if out.panicVal != nil {
		t.Fatalf("single Submit() panicked after repository conflict: %v", out.panicVal)
	}
	if out.err != nil {
		t.Fatalf("single Submit() error = %v", out.err)
	}
	wantJobID := batch[0].Job.ID.String()
	if out.response.LinkID != batch[0].Link.ID.String() || out.response.JobID == nil || *out.response.JobID != wantJobID || out.response.Status != string(model.LinkStatusPending) {
		t.Fatalf("single Submit() = %#v, want existing link %s job %s", out.response, batch[0].Link.ID, wantJobID)
	}
	if got := queue.enqueueCalls.Load() + queue.enqueueTxCalls.Load(); got != 0 {
		t.Fatalf("single queue calls = %d, want 0 for Batch-owned attempt", got)
	}
	if got := rawCountLinks(t, pool); got != 1 {
		t.Fatalf("links after concurrent saves = %d, want 1", got)
	}
	if got := rawCountJobs(t, pool); got != 1 {
		t.Fatalf("parse jobs after concurrent saves = %d, want 1", got)
	}
}

func TestSubmitService_RealDB_BatchReusesURLOnlyIngestLink(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	jobs := repository.NewPGXJobRepository(pool)
	queue := &countingSubmitQueue{}
	commands := dbLinkCommands(pool, links, queue)
	locker := urllock.NewInProcessURLLocker()
	submit, ingest := service.NewLinkServices(
		links,
		jobs,
		commands,
		locker,
		service.SubmitServiceOptions{},
	)
	ctx := t.Context()
	const rawURL = "https://concurrency.example.com/url-only-ingest"

	ingested, err := ingest.Ingest(ctx, dto.IngestRequest{Sources: []dto.IngestSource{{
		Kind: "url", URL: rawURL,
	}}})
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	batch, err := submit.Batch(ctx, dto.BatchCreateRequest{Items: []dto.LinkCreateRequest{{URL: rawURL}}})
	if err != nil {
		t.Fatalf("Batch() error = %v", err)
	}
	if len(batch.Results) != 1 || batch.Results[0].Result == nil {
		t.Fatalf("Batch() = %#v, want one successful result", batch)
	}
	if got := batch.Results[0].Result; got.LinkID != ingested.LinkID || got.JobID == nil || ingested.JobID == nil || *got.JobID != *ingested.JobID {
		t.Fatalf("Batch result = %#v, want ingested link/job %#v", got, ingested)
	}
	if got := queue.enqueueTxCalls.Load(); got != 1 {
		t.Fatalf("transactional queue calls = %d, want only initial ingest", got)
	}
	if got := rawCountLinks(t, pool); got != 1 {
		t.Fatalf("links after ingest then Batch = %d, want 1", got)
	}
	if got := rawCountJobs(t, pool); got != 1 {
		t.Fatalf("parse jobs after ingest then Batch = %d, want 1", got)
	}
}

func TestSubmitService_RealDB_URLOnlyIngestReusesSubmittedLinkAcrossStates(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	jobs := repository.NewPGXJobRepository(pool)
	queue := &countingSubmitQueue{}
	commands := dbLinkCommands(pool, links, queue)
	submit, ingest := service.NewLinkServices(
		links,
		jobs,
		commands,
		urllock.NewInProcessURLLocker(),
		service.SubmitServiceOptions{},
	)
	ctx := t.Context()
	const rawURL = "https://concurrency.example.com/submit-then-url-ingest"

	submitted, err := submit.Submit(ctx, dto.LinkCreateRequest{URL: rawURL})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	pending, err := ingest.Ingest(ctx, dto.IngestRequest{Sources: []dto.IngestSource{{Kind: "url", URL: rawURL}}})
	if err != nil {
		t.Fatalf("pending Ingest() error = %v", err)
	}
	if pending.LinkID != submitted.LinkID || pending.JobID == nil || submitted.JobID == nil || *pending.JobID != *submitted.JobID {
		t.Fatalf("pending Ingest() = %#v, want submitted link/job %#v", pending, submitted)
	}

	linkID := uuid.MustParse(submitted.LinkID)
	if _, err := pool.Exec(ctx, `UPDATE links SET status = 'done', updated_at = NOW() WHERE id = $1`, linkID); err != nil {
		t.Fatalf("mark link done: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE parse_jobs SET status = 'done', updated_at = NOW() WHERE link_id = $1`, linkID); err != nil {
		t.Fatalf("mark parse job done: %v", err)
	}
	done, err := ingest.Ingest(ctx, dto.IngestRequest{Sources: []dto.IngestSource{{Kind: "url", URL: rawURL}}})
	if err != nil {
		t.Fatalf("done Ingest() error = %v", err)
	}
	if done.LinkID != submitted.LinkID || done.JobID == nil || *done.JobID != *submitted.JobID || done.Status != string(model.LinkStatusDone) {
		t.Fatalf("done Ingest() = %#v, want terminal submitted link/job %#v", done, submitted)
	}
	if got := queue.enqueueTxCalls.Load(); got != 1 {
		t.Fatalf("transactional queue calls = %d, want only initial Submit", got)
	}
	if got := rawCountLinks(t, pool); got != 1 {
		t.Fatalf("links after cross-entry saves = %d, want 1", got)
	}
	if got := rawCountJobs(t, pool); got != 1 {
		t.Fatalf("parse jobs after cross-entry saves = %d, want 1", got)
	}
}

func TestSubmitService_RealDB_ConcurrentBatchWaitsForURLOnlyIngest(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	jobs := repository.NewPGXJobRepository(pool)
	queue := &blockingFirstSubmitQueue{started: make(chan struct{}), release: make(chan struct{})}
	commands := dbLinkCommands(pool, links, queue)
	locker := urllock.NewAdvisoryURLLocker(pool, urllock.AdvisoryLockClassSubmit)
	submit, ingest := service.NewLinkServices(
		links,
		jobs,
		commands,
		locker,
		service.SubmitServiceOptions{},
	)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	const rawURL = "https://concurrency.example.com/concurrent-url-only-ingest"

	type ingestOutcome struct {
		response dto.SubmitResponse
		err      error
	}
	ingestDone := make(chan ingestOutcome, 1)
	go func() {
		response, err := ingest.Ingest(ctx, dto.IngestRequest{Sources: []dto.IngestSource{{Kind: "url", URL: rawURL}}})
		ingestDone <- ingestOutcome{response: response, err: err}
	}()
	<-queue.started

	type batchOutcome struct {
		response dto.BatchSubmitResponse
		err      error
	}
	batchDone := make(chan batchOutcome, 1)
	go func() {
		response, err := submit.Batch(ctx, dto.BatchCreateRequest{Items: []dto.LinkCreateRequest{{URL: rawURL}}})
		batchDone <- batchOutcome{response: response, err: err}
	}()
	select {
	case outcome := <-batchDone:
		t.Fatalf("Batch completed before ingest released URL lock: %#v", outcome)
	case <-time.After(100 * time.Millisecond):
	}
	close(queue.release)

	ingested := <-ingestDone
	if ingested.err != nil {
		t.Fatalf("Ingest() error = %v", ingested.err)
	}
	batched := <-batchDone
	if batched.err != nil {
		t.Fatalf("Batch() error = %v", batched.err)
	}
	if len(batched.response.Results) != 1 || batched.response.Results[0].Result == nil {
		t.Fatalf("Batch() = %#v, want successful existing result", batched.response)
	}
	got := batched.response.Results[0].Result
	if got.LinkID != ingested.response.LinkID || got.JobID == nil || ingested.response.JobID == nil || *got.JobID != *ingested.response.JobID {
		t.Fatalf("Batch result = %#v, want concurrent ingest %#v", got, ingested.response)
	}
	if gotCalls := queue.count.Load(); gotCalls != 1 {
		t.Fatalf("queue calls = %d, want one ingest-owned attempt", gotCalls)
	}
	if gotLinks := rawCountLinks(t, pool); gotLinks != 1 {
		t.Fatalf("links after concurrent ingest/Batch = %d, want 1", gotLinks)
	}
	if gotJobs := rawCountJobs(t, pool); gotJobs != 1 {
		t.Fatalf("jobs after concurrent ingest/Batch = %d, want 1", gotJobs)
	}
}
