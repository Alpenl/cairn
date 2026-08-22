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
	*repository.PGXLinkRepository
	readDone chan struct{}
	release  chan struct{}
}

func (r *delayedURLMissReader) GetSubmitLookupByURL(ctx context.Context, rawURL string) (*repository.LinkSubmitLookup, error) {
	link, err := r.PGXLinkRepository.GetSubmitLookupByURL(ctx, rawURL)
	close(r.readDone)
	select {
	case <-r.release:
		return link, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type countingSubmitQueue struct {
	enqueueTxCalls atomic.Int32
	enqueueErr     error
	cancelErr      error
}

type blockingFirstSubmitQueue struct {
	count   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (q *blockingFirstSubmitQueue) EnqueueTx(ctx context.Context, _ pgx.Tx, _ model.ParseAttempt) error {
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

func (q *blockingFirstSubmitQueue) CancelActiveTx(context.Context, pgx.Tx, uuid.UUID) error {
	return nil
}

func (q *blockingFirstSubmitQueue) CancelAllActiveTx(context.Context, pgx.Tx, uuid.UUID) error {
	return nil
}

func (q *countingSubmitQueue) EnqueueTx(context.Context, pgx.Tx, model.ParseAttempt) error {
	q.enqueueTxCalls.Add(1)
	return q.enqueueErr
}

func (q *countingSubmitQueue) CancelActiveTx(context.Context, pgx.Tx, uuid.UUID) error {
	return q.cancelErr
}

func (q *countingSubmitQueue) CancelAllActiveTx(context.Context, pgx.Tx, uuid.UUID) error {
	return q.cancelErr
}

func dbLinkCommands(pool *pgxpool.Pool, links *repository.PGXLinkRepository, queue interface {
	EnqueueTx(context.Context, pgx.Tx, model.ParseAttempt) error
	CancelActiveTx(context.Context, pgx.Tx, uuid.UUID) error
	CancelAllActiveTx(context.Context, pgx.Tx, uuid.UUID) error
}) *durablework.LinkCommands {
	return durablework.NewLinkCommands(pool, links, queue)
}

func submitRepositoryForTest(
	ctx context.Context,
	pool *pgxpool.Pool,
	repo *repository.PGXLinkRepository,
	params repository.CreateLinkParams,
) (repository.LinkSubmitResult, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return repository.LinkSubmitResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := repo.SubmitTx(ctx, tx, params)
	if err != nil {
		return repository.LinkSubmitResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return repository.LinkSubmitResult{}, err
	}
	return result, nil
}

func TestLinkRepositorySubmitTxConflictDoesNotRewriteLink(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)
	ctx := t.Context()
	params := repository.CreateLinkParams{
		URL: "https://concurrency.example.com/no-rewrite", SourceKind: "url",
		SourceKey: "https://concurrency.example.com/no-rewrite", Status: model.LinkStatusPending,
	}

	first, err := submitRepositoryForTest(ctx, pool, repo, params)
	if err != nil || first.Link == nil || first.Attempt == nil {
		t.Fatalf("first SubmitTx() = %#v, %v", first, err)
	}
	var beforeXmin, beforeCTID string
	if err := pool.QueryRow(ctx, `SELECT xmin::text,ctid::text FROM links WHERE id=$1`, first.Link.ID).Scan(&beforeXmin, &beforeCTID); err != nil {
		t.Fatalf("read original row version: %v", err)
	}

	second, err := submitRepositoryForTest(ctx, pool, repo, params)
	if err != nil || second.Link == nil || second.Attempt != nil || second.Link.ID != first.Link.ID {
		t.Fatalf("second SubmitTx() = %#v, %v, want existing row without attempt", second, err)
	}
	var afterXmin, afterCTID string
	if err := pool.QueryRow(ctx, `SELECT xmin::text,ctid::text FROM links WHERE id=$1`, first.Link.ID).Scan(&afterXmin, &afterCTID); err != nil {
		t.Fatalf("read repeated row version: %v", err)
	}
	if afterXmin != beforeXmin || afterCTID != beforeCTID {
		t.Fatalf("repeat save rewrote Link row: xmin/ctid %s/%s -> %s/%s", beforeXmin, beforeCTID, afterXmin, afterCTID)
	}
}

func TestSubmitServiceRealDBReusesConcurrentRepositoryInsertDuringMissWindow(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	ctx := t.Context()
	const rawURL = "https://concurrency.example.com/service-vs-repository"
	reader := &delayedURLMissReader{
		PGXLinkRepository: links, readDone: make(chan struct{}), release: make(chan struct{}),
	}
	queue := &countingSubmitQueue{}
	submit, _ := service.NewLinkServices(reader, dbLinkCommands(pool, links, queue), urllock.NewInProcessURLLocker(), service.SubmitServiceOptions{})

	type submitOutcome struct {
		response dto.SubmitResponse
		err      error
		panicVal any
	}
	outcomes := make(chan submitOutcome, 1)
	go func() {
		out := submitOutcome{}
		defer func() { out.panicVal = recover(); outcomes <- out }()
		out.response, out.err = submit.Submit(ctx, dto.LinkCreateRequest{URL: rawURL, Destination: "library"})
	}()
	<-reader.readDone
	inserted, err := submitRepositoryForTest(ctx, pool, links, repository.CreateLinkParams{
		URL: rawURL, SourceKind: "url", SourceKey: rawURL, Status: model.LinkStatusPending,
	})
	if err != nil || inserted.Link == nil || inserted.Attempt == nil {
		t.Fatalf("concurrent SubmitTx() = %#v, %v", inserted, err)
	}
	close(reader.release)
	out := <-outcomes
	if out.panicVal != nil || out.err != nil {
		t.Fatalf("single Submit() result = %#v panic=%v error=%v", out.response, out.panicVal, out.err)
	}
	if out.response.LinkID != inserted.Link.ID.String() || out.response.Status != string(model.LinkStatusPending) {
		t.Fatalf("single Submit() = %#v, want existing pending Link %s", out.response, inserted.Link.ID)
	}
	if got := queue.enqueueTxCalls.Load(); got != 0 {
		t.Fatalf("single queue calls = %d, want 0 for repository-owned attempt", got)
	}
	if got := rawCountLinks(t, pool); got != 1 {
		t.Fatalf("links after concurrent saves = %d, want 1", got)
	}
}

func TestSubmitServiceRealDBSubmitReusesURLOnlyIngestLink(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	queue := &countingSubmitQueue{}
	submit, ingest := service.NewLinkServices(links, dbLinkCommands(pool, links, queue), urllock.NewInProcessURLLocker(), service.SubmitServiceOptions{})
	ctx := t.Context()
	const rawURL = "https://concurrency.example.com/url-only-ingest"
	ingested, err := ingest.Ingest(ctx, dto.IngestRequest{Destination: "library", Sources: []dto.IngestSource{{Kind: "url", URL: rawURL}}})
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	submitted, err := submit.Submit(ctx, dto.LinkCreateRequest{URL: rawURL, Destination: "library"})
	if err != nil {
		t.Fatalf("Submit() = %#v, %v", submitted, err)
	}
	if submitted.LinkID != ingested.LinkID || submitted.Status != ingested.Status {
		t.Fatalf("Submit result = %#v, want ingest Link/status %#v", submitted, ingested)
	}
	if got := queue.enqueueTxCalls.Load(); got != 1 {
		t.Fatalf("transactional queue calls = %d, want only initial ingest", got)
	}
}

func TestSubmitServiceRealDBURLOnlyIngestReusesSubmittedLinkAcrossStates(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	queue := &countingSubmitQueue{}
	submit, ingest := service.NewLinkServices(links, dbLinkCommands(pool, links, queue), urllock.NewInProcessURLLocker(), service.SubmitServiceOptions{})
	ctx := t.Context()
	const rawURL = "https://concurrency.example.com/submit-then-url-ingest"
	submitted, err := submit.Submit(ctx, dto.LinkCreateRequest{URL: rawURL, Destination: "library"})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	pending, err := ingest.Ingest(ctx, dto.IngestRequest{Destination: "library", Sources: []dto.IngestSource{{Kind: "url", URL: rawURL}}})
	if err != nil {
		t.Fatalf("pending Ingest() error = %v", err)
	}
	if pending.LinkID != submitted.LinkID || pending.Status != submitted.Status {
		t.Fatalf("pending Ingest() = %#v, want submitted Link/status %#v", pending, submitted)
	}
	linkID := uuid.MustParse(submitted.LinkID)
	if _, err := pool.Exec(ctx, `UPDATE links SET status='done',updated_at=NOW() WHERE id=$1`, linkID); err != nil {
		t.Fatalf("mark Link done: %v", err)
	}
	done, err := ingest.Ingest(ctx, dto.IngestRequest{Destination: "library", Sources: []dto.IngestSource{{Kind: "url", URL: rawURL}}})
	if err != nil {
		t.Fatalf("done Ingest() error = %v", err)
	}
	if done.LinkID != submitted.LinkID || done.Status != string(model.LinkStatusDone) {
		t.Fatalf("done Ingest() = %#v, want terminal submitted Link %s", done, submitted.LinkID)
	}
	if got := queue.enqueueTxCalls.Load(); got != 1 {
		t.Fatalf("transactional queue calls = %d, want only initial Submit", got)
	}
}

func TestSubmitServiceRealDBConcurrentSubmitWaitsForURLOnlyIngest(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	queue := &blockingFirstSubmitQueue{started: make(chan struct{}), release: make(chan struct{})}
	locker := urllock.NewInProcessURLLocker()
	submit, ingest := service.NewLinkServices(links, dbLinkCommands(pool, links, queue), locker, service.SubmitServiceOptions{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	const rawURL = "https://concurrency.example.com/concurrent-url-only-ingest"

	type ingestOutcome struct {
		response dto.SubmitResponse
		err      error
	}
	ingestDone := make(chan ingestOutcome, 1)
	go func() {
		response, err := ingest.Ingest(ctx, dto.IngestRequest{Destination: "library", Sources: []dto.IngestSource{{Kind: "url", URL: rawURL}}})
		ingestDone <- ingestOutcome{response: response, err: err}
	}()
	<-queue.started

	type submitOutcome struct {
		response dto.SubmitResponse
		err      error
	}
	submitDone := make(chan submitOutcome, 1)
	go func() {
		response, err := submit.Submit(ctx, dto.LinkCreateRequest{URL: rawURL, Destination: "library"})
		submitDone <- submitOutcome{response: response, err: err}
	}()
	select {
	case outcome := <-submitDone:
		t.Fatalf("Submit completed before ingest released URL lock: %#v", outcome)
	case <-time.After(100 * time.Millisecond):
	}
	close(queue.release)
	ingested := <-ingestDone
	if ingested.err != nil {
		t.Fatalf("Ingest() error = %v", ingested.err)
	}
	submitted := <-submitDone
	if submitted.err != nil {
		t.Fatalf("Submit() = %#v, %v", submitted.response, submitted.err)
	}
	if submitted.response.LinkID != ingested.response.LinkID || submitted.response.Status != ingested.response.Status {
		t.Fatalf("Submit result = %#v, want concurrent ingest %#v", submitted.response, ingested.response)
	}
	if gotCalls := queue.count.Load(); gotCalls != 1 {
		t.Fatalf("queue calls = %d, want one ingest-owned attempt", gotCalls)
	}
	if gotLinks := rawCountLinks(t, pool); gotLinks != 1 {
		t.Fatalf("links after concurrent ingest/Submit = %d, want 1", gotLinks)
	}
}
