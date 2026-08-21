// river_queue_test.go exercises the Phase 13 (v4.0 M2) River-backed parse
// queue end to end against a real PostgreSQL container with the River schema
// migrated in (internal/migrate.Up runs rivermigrate). The pure-Go unit tests
// in internal/service / internal/worker cover the job-args / worker / façade
// logic with stubs; this file is the only place that exercises:
//
//   - submit → a parse_link job lands in river_job → worker runs Run → link done
//   - same-link in-flight re-submit deduped by River unique job (ByArgs)
//   - same-tx enqueue: link insert rolled back ⇒ no river_job row (atomicity)
//   - crash recovery: a pending job survives a client restart and gets worked
//
// against the actual schema + River planner, which stubs cannot reproduce.
package dbintegration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
	"webtag/internal/service/linktranslation"
	"webtag/internal/service/translator"
	"webtag/internal/worker"
)

// recordingProcessor is a service.ParseProcessor stub: it records the exact
// generation it was asked to run and projects a terminal state onto the Link.
// Completion is signalled per immutable attempt so tests cannot accidentally
// accept a different generation for the same link.
type recordingProcessor struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	seen map[model.ParseAttempt]int
	done map[model.ParseAttempt]chan struct{}
}

type cancellationAwareProcessor struct {
	target     model.ParseAttempt
	started    chan struct{}
	cancelled  chan struct{}
	startOnce  sync.Once
	cancelOnce sync.Once
}

func (p *cancellationAwareProcessor) Run(ctx context.Context, attempt model.ParseAttempt) error {
	if attempt != p.target {
		return nil
	}
	p.startOnce.Do(func() { close(p.started) })
	<-ctx.Done()
	p.cancelOnce.Do(func() { close(p.cancelled) })
	return ctx.Err()
}

func (*cancellationAwareProcessor) RecordDiscard(context.Context, model.ParseAttempt, error) error {
	return nil
}

func newRecordingProcessor(pool *pgxpool.Pool) *recordingProcessor {
	return &recordingProcessor{
		pool: pool,
		seen: make(map[model.ParseAttempt]int),
		done: make(map[model.ParseAttempt]chan struct{}),
	}
}

// waitChan returns (creating if needed) the per-attempt completion channel.
func (p *recordingProcessor) waitChan(attempt model.ParseAttempt) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, ok := p.done[attempt]
	if !ok {
		ch = make(chan struct{}, 1)
		p.done[attempt] = ch
	}
	return ch
}

func (p *recordingProcessor) Run(ctx context.Context, attempt model.ParseAttempt) error {
	if err := p.setTerminal(ctx, attempt, "done", nil); err != nil {
		return err
	}

	p.mu.Lock()
	p.seen[attempt]++
	ch := p.done[attempt]
	if ch == nil {
		ch = make(chan struct{}, 1)
		p.done[attempt] = ch
	}
	p.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
	return nil
}

func (p *recordingProcessor) RecordDiscard(ctx context.Context, attempt model.ParseAttempt, cause error) error {
	errorMsg := "river job discarded"
	if cause != nil {
		errorMsg = cause.Error()
	}
	return p.setTerminal(ctx, attempt, "failed", &errorMsg)
}

func (p *recordingProcessor) setTerminal(ctx context.Context, attempt model.ParseAttempt, status string, errorMsg *string) error {
	result, err := p.pool.Exec(ctx,
		`UPDATE links SET status = $3, error_msg = $4, updated_at = now()
		 WHERE id = $1 AND parse_generation = $2
		   AND status IN ('pending', 'processing') AND deleted_at IS NULL`,
		attempt.LinkID, attempt.Generation, status, errorMsg,
	)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return repository.ErrParseAttemptNotRunnable
	}
	return nil
}

func (p *recordingProcessor) seenCount(attempt model.ParseAttempt) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen[attempt]
}

type noopTranslationProcessor struct{}

func (noopTranslationProcessor) Run(context.Context, model.TranslationAttempt) error { return nil }
func (noopTranslationProcessor) RecordFailure(context.Context, model.TranslationAttempt, error) error {
	return nil
}

type failingTranslationEngine struct {
	err error
}

func (e failingTranslationEngine) Translate(context.Context, translator.Request) (translator.Result, error) {
	return translator.Result{}, e.err
}

// newRiverQueue builds a RiverQueue against the test pool. The queue is NOT
// started; callers Start when they want jobs worked (some tests insert first,
// then start, to assert crash-recovery / backlog draining).
func newRiverQueue(t *testing.T, pool *pgxpool.Pool, proc service.ParseProcessor) *worker.RiverQueue {
	t.Helper()
	q, err := worker.NewRiverQueue(worker.RiverQueueOptions{
		Pool:                 pool,
		Processor:            proc,
		TranslationProcessor: noopTranslationProcessor{},
		MaxWorkers:           2,
		JobTimeout:           10 * time.Second,
	})
	if err != nil {
		t.Fatalf("new river queue: %v", err)
	}
	return q
}

// countRiverParseJobs returns how many river_job rows carry the exact current
// parse attempt encoded in args.
func countRiverParseJobs(t *testing.T, pool *pgxpool.Pool, attempt model.ParseAttempt) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job
		 WHERE kind = 'parse_link'
		   AND args->>'link_id' = $1
		   AND args->>'parse_generation' = $2
		   AND args->>'expected_metadata_revision' = $3`,
		attempt.LinkID.String(), fmt.Sprint(attempt.Generation), fmt.Sprint(attempt.ExpectedMetadataRevision),
	).Scan(&n); err != nil {
		t.Fatalf("count river parse jobs: %v", err)
	}
	return n
}

func TestRiverQueue_TranslationSchedulingUsesCurrentProtocol(t *testing.T) {
	pool := StartPostgres(t)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	sourceRevision := int64(19)
	seed := model.TranslationAttemptSeed{
		TranslationID: uuid.New(), AttemptGeneration: 7,
		SourceHash:            "abababababababababababababababababababababababababababababababab",
		SourceContentRevision: &sourceRevision,
	}
	jobID, err := queue.EnqueueTranslation(t.Context(), seed)
	if err != nil || jobID <= 0 {
		t.Fatalf("EnqueueTranslation() = %d, %v", jobID, err)
	}

	var kind, translationID, generation, sourceHash, sourceContentRevision string
	if err := pool.QueryRow(t.Context(), `SELECT kind,
		args->>'translation_id', args->>'attempt_generation',
		args->>'source_hash', args->>'source_content_revision'
		FROM river_job WHERE id = $1`, jobID).
		Scan(&kind, &translationID, &generation, &sourceHash, &sourceContentRevision); err != nil {
		t.Fatalf("read scheduled translation job: %v", err)
	}
	if kind != model.TranslationJobKind || translationID != seed.TranslationID.String() ||
		generation != fmt.Sprint(seed.AttemptGeneration) || sourceHash != seed.SourceHash ||
		sourceContentRevision != fmt.Sprint(sourceRevision) {
		t.Fatalf("job kind=%q translation=%q generation=%q hash=%q revision=%q",
			kind, translationID, generation, sourceHash, sourceContentRevision)
	}
}

func TestRiverQueue_RejectsDuplicateTranslationAttemptInsertResult(t *testing.T) {
	pool := StartPostgres(t)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	seed := model.TranslationAttemptSeed{
		TranslationID: uuid.New(), AttemptGeneration: 1,
		SourceHash: "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd",
	}

	firstID, err := queue.EnqueueTranslation(t.Context(), seed)
	if err != nil || firstID <= 0 {
		t.Fatalf("first EnqueueTranslation() = %d, %v", firstID, err)
	}
	secondID, err := queue.EnqueueTranslation(t.Context(), seed)
	if err == nil || secondID != 0 {
		t.Fatalf("duplicate EnqueueTranslation() = %d, %v; want 0, error", secondID, err)
	}
	var rows int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM river_job
		WHERE kind = 'translate_link_v2'
		  AND args->>'translation_id' = $1
		  AND args->>'attempt_generation' = '1'`, seed.TranslationID.String()).Scan(&rows); err != nil {
		t.Fatalf("count duplicate translation attempts: %v", err)
	}
	if rows != 1 {
		t.Fatalf("translation attempt rows = %d, want 1", rows)
	}
}

func TestRiverQueue_FinalTranslationFailureProjectsStableTerminalState(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	ctx := t.Context()
	linkID := mustCreateDoneLink(t, links, ctx,
		"https://example.com/final-translation-failure", "final translation failure", "example.com")
	translationProcessor := linktranslation.NewProcessor(linktranslation.ProcessorOptions{
		Translations: translations,
		Translator:   failingTranslationEngine{err: errors.New("upstream unavailable")},
	})
	queue, err := worker.NewRiverQueue(worker.RiverQueueOptions{
		Pool:                  pool,
		Processor:             newRecordingProcessor(pool),
		TranslationProcessor:  translationProcessor,
		TranslationJobTimeout: 10 * time.Second,
		MaxWorkers:            1,
		JobTimeout:            10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRiverQueue() error = %v", err)
	}
	translationID := schedulePendingTranslation(t, pool, queue, ctx, linkID, "final translation failure")
	var riverJobID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM river_job
		WHERE kind=$1 AND args->>'translation_id'=$2`, model.TranslationJobKind, translationID.String()).Scan(&riverJobID); err != nil {
		t.Fatalf("read translation River job: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE river_job
		SET attempt = max_attempts - 1 WHERE id = $1`, riverJobID); err != nil {
		t.Fatalf("prepare final attempt: %v", err)
	}
	if err := queue.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start() error = %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = queue.Stop(stopCtx)
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		projected, readErr := translations.GetByID(ctx, translationID)
		if readErr != nil {
			t.Fatalf("GetByID() error = %v", readErr)
		}
		if projected != nil && projected.Status == model.TranslationStatusFailed {
			if projected.ErrorMsg == nil || *projected.ErrorMsg != "翻译服务暂时不可用，请重试" {
				t.Fatalf("failed translation = %+v", projected)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("translation did not reach failed: %+v", projected)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// insertPendingLinkAttempt creates the product state that every parse_link
// River job must reference and returns the immutable attempt encoded in args.
func insertPendingLinkAttempt(t *testing.T, pool *pgxpool.Pool, rawURL string) (uuid.UUID, model.ParseAttempt) {
	t.Helper()
	ctx := context.Background()

	var attempt model.ParseAttempt
	if err := pool.QueryRow(ctx,
		`INSERT INTO links (url, source_key, status, first_collected_at)
		 VALUES ($1, $1, 'pending', NOW())
		 RETURNING id, parse_generation, metadata_revision`,
		rawURL,
	).Scan(&attempt.LinkID, &attempt.Generation, &attempt.ExpectedMetadataRevision); err != nil {
		t.Fatalf("insert pending link: %v", err)
	}
	return attempt.LinkID, attempt
}

func parseAttemptForLink(link *model.Link) model.ParseAttempt {
	return model.ParseAttempt{
		LinkID:                   link.ID,
		Generation:               link.ParseGeneration,
		ExpectedMetadataRevision: link.MetadataRevision,
	}
}

func serviceCapture(capture repository.CreateLinkParams) service.LinkCapture {
	return service.LinkCapture{
		URL:                     capture.URL,
		SourceKind:              capture.SourceKind,
		SourceKey:               capture.SourceKey,
		InputTitle:              capture.InputTitle,
		InputText:               capture.InputText,
		InputHTML:               capture.InputHTML,
		InputImages:             capture.InputImages,
		SourceMetadata:          capture.SourceMetadata,
		Description:             capture.Description,
		Status:                  capture.Status,
		Domain:                  capture.Domain,
		ContentType:             capture.ContentType,
		PathDepth:               capture.PathDepth,
		ParentPath:              capture.ParentPath,
		ParentID:                capture.ParentID,
		RequestedLibraryKind:    capture.RequestedLibraryKind,
		UserSelectedLibraryKind: capture.UserSelectedLibraryKind,
	}
}

func requeueWithRiver(t *testing.T, pool *pgxpool.Pool, repo *repository.PGXLinkRepository, queue *worker.RiverQueue, linkID uuid.UUID, capture *repository.CreateLinkParams) model.ParseAttempt {
	t.Helper()
	var applicationCapture *service.LinkCapture
	if capture != nil {
		converted := serviceCapture(*capture)
		applicationCapture = &converted
	}
	_, err := dbLinkCommands(pool, repo, queue).RequeueLink(context.Background(), service.RequeueLinkCommand{
		LinkID:  linkID,
		Capture: applicationCapture,
	})
	if err != nil {
		t.Fatalf("RequeueLink: %v", err)
	}
	link, err := repo.GetByID(context.Background(), linkID)
	if err != nil || link == nil {
		t.Fatalf("GetByID after requeue = %#v, %v", link, err)
	}
	return parseAttemptForLink(link)
}

func TestRiverQueue_RequeueCancelsAvailableAttempt(t *testing.T) {
	pool := StartPostgres(t)
	proc := newRecordingProcessor(pool)
	queue := newRiverQueue(t, pool, proc)
	repo := repository.NewPGXLinkRepository(pool)
	linkID, oldAttempt := insertPendingLinkAttempt(t, pool, "https://example.com/requeue-cancel-available")
	if err := queue.Enqueue(context.Background(), oldAttempt); err != nil {
		t.Fatalf("enqueue old attempt: %v", err)
	}
	text := "new captured body"
	newAttempt := requeueWithRiver(t, pool, repo, queue, linkID, &repository.CreateLinkParams{
		URL: "https://example.com/requeue-cancel-available", SourceKind: "browser_capture",
		SourceKey: "capture:requeue-cancel-available", InputText: &text,
	})

	var oldState string
	var cancelAttempted bool
	if err := pool.QueryRow(context.Background(), `SELECT state::text, metadata ? 'cancel_attempted_at'
		FROM river_job WHERE kind='parse_link' AND args->>'link_id'=$1 AND args->>'parse_generation'=$2`,
		oldAttempt.LinkID.String(), fmt.Sprint(oldAttempt.Generation)).
		Scan(&oldState, &cancelAttempted); err != nil {
		t.Fatalf("read old River attempt: %v", err)
	}
	if oldState != "cancelled" || !cancelAttempted {
		t.Fatalf("old River attempt = state %q cancel_attempted=%v, want cancelled/true", oldState, cancelAttempted)
	}
	if got := countRiverParseJobs(t, pool, newAttempt); got != 1 {
		t.Fatalf("new River attempt count = %d, want 1", got)
	}
}

func TestRiverQueue_RequeueCancelsRunningWorkerContext(t *testing.T) {
	pool := StartPostgres(t)
	linkID, oldAttempt := insertPendingLinkAttempt(t, pool, "https://example.com/requeue-cancel-running")
	proc := &cancellationAwareProcessor{
		target:    oldAttempt,
		started:   make(chan struct{}),
		cancelled: make(chan struct{}),
	}
	queue := newRiverQueue(t, pool, proc)
	if err := queue.Enqueue(context.Background(), oldAttempt); err != nil {
		t.Fatalf("enqueue old attempt: %v", err)
	}
	if err := queue.Start(context.Background()); err != nil {
		t.Fatalf("queue start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = queue.Stop(stopCtx)
	})
	select {
	case <-proc.started:
	case <-time.After(10 * time.Second):
		t.Fatal("old attempt did not enter running state")
	}

	repo := repository.NewPGXLinkRepository(pool)
	newAttempt := requeueWithRiver(t, pool, repo, queue, linkID, nil)
	var marked bool
	if err := pool.QueryRow(context.Background(), `SELECT metadata ? 'cancel_attempted_at'
		FROM river_job WHERE kind='parse_link' AND args->>'link_id'=$1 AND args->>'parse_generation'=$2`,
		oldAttempt.LinkID.String(), fmt.Sprint(oldAttempt.Generation)).Scan(&marked); err != nil {
		t.Fatalf("read running cancellation metadata: %v", err)
	}
	if !marked {
		t.Fatal("running River attempt missing cancel_attempted_at")
	}
	select {
	case <-proc.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("running worker context was not cancelled after requeue commit")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		var state string
		if err := pool.QueryRow(context.Background(), `SELECT state::text FROM river_job
			WHERE kind='parse_link' AND args->>'link_id'=$1 AND args->>'parse_generation'=$2`,
			oldAttempt.LinkID.String(), fmt.Sprint(oldAttempt.Generation)).Scan(&state); err != nil {
			t.Fatalf("read old River state: %v", err)
		}
		if state == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("old River state = %q, want cancelled", state)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := countRiverParseJobs(t, pool, newAttempt); got != 1 {
		t.Fatalf("new River attempt count = %d, want 1", got)
	}
}

// TestRiverQueue_SubmitEnqueuesAndWorksToDone is the happy path: SubmitLink
// inserts a Link and its River row in one tx; once the queue starts, the
// worker runs and the Link reaches done.
func TestRiverQueue_SubmitEnqueuesAndWorksToDone(t *testing.T) {
	pool := StartPostgres(t)
	proc := newRecordingProcessor(pool)
	q := newRiverQueue(t, pool, proc)

	linkRepo := repository.NewPGXLinkRepository(pool)
	result, err := dbLinkCommands(pool, linkRepo, q).SubmitLink(context.Background(), service.SubmitLinkCommand{
		Capture: service.LinkCapture{URL: "https://example.com/river-happy", Status: model.LinkStatusPending},
	})
	if err != nil {
		t.Fatalf("SubmitLink: %v", err)
	}
	if result.Link == nil || !result.Enqueued {
		t.Fatalf("SubmitLink() = %#v, want enqueued Link", result)
	}
	link := result.Link
	attempt := parseAttemptForLink(link)

	if got := countRiverParseJobs(t, pool, attempt); got != 1 {
		t.Fatalf("river_job count after submit = %d, want 1", got)
	}

	ch := proc.waitChan(attempt)
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("queue start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = q.Stop(ctx)
	})

	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		t.Fatal("worker did not process the link within 20s")
	}

	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM links WHERE id = $1`, link.ID).Scan(&status); err != nil {
		t.Fatalf("read link status: %v", err)
	}
	if status != "done" {
		t.Fatalf("link status = %q, want done", status)
	}
}

// TestRiverQueue_UniqueDedupesInFlight verifies River's unique-job (ByArgs)
// dedupe: enqueuing the same immutable attempt twice while the first
// is still pending produces only one river_job row (replacing the old
// in-flight map dedupe).
func TestRiverQueue_UniqueDedupesInFlight(t *testing.T) {
	pool := StartPostgres(t)
	proc := newRecordingProcessor(pool)
	// Build the queue but DON'T start it, so the first job stays available
	// (in-flight from the unique-state perspective) when we enqueue again.
	q := newRiverQueue(t, pool, proc)

	_, attempt := insertPendingLinkAttempt(t, pool, "https://example.com/river-unique")

	if err := q.Enqueue(context.Background(), attempt); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if err := q.Enqueue(context.Background(), attempt); err != nil {
		t.Fatalf("second enqueue (should be deduped, not error): %v", err)
	}

	if got := countRiverParseJobs(t, pool, attempt); got != 1 {
		t.Fatalf("river_job count after duplicate enqueue = %d, want 1 (unique dedupe)", got)
	}
}

// TestRiverQueue_TxRollbackLeavesNoJob proves same-tx atomicity: when the
// submit transaction rolls back (hook returns nil but caller rolls back), no
// river_job survives. We drive the tx directly to force the rollback after a
// successful EnqueueTx.
func TestRiverQueue_TxRollbackLeavesNoJob(t *testing.T) {
	pool := StartPostgres(t)
	proc := newRecordingProcessor(pool)
	q := newRiverQueue(t, pool, proc)

	linkID := uuid.New()
	attempt := model.ParseAttempt{LinkID: linkID, Generation: 1, ExpectedMetadataRevision: 1}
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	// Insert a link in the tx, enqueue the job in the SAME tx, then roll back.
	if _, err := tx.Exec(context.Background(),
		`INSERT INTO links (id, url, source_key, status, first_collected_at) VALUES ($1, $2, $2, 'pending', NOW())`,
		linkID, "https://example.com/river-rollback",
	); err != nil {
		t.Fatalf("insert link in tx: %v", err)
	}
	if err := q.EnqueueTx(context.Background(), tx, attempt); err != nil {
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if got := countRiverParseJobs(t, pool, attempt); got != 0 {
		t.Fatalf("river_job count after rollback = %d, want 0 (same-tx atomicity)", got)
	}
	// And the link must not exist either.
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM links WHERE id = $1`, linkID).Scan(&n); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if n != 0 {
		t.Fatalf("links count after rollback = %d, want 0", n)
	}
}

// TestRiverQueue_CrashRecoveryWorksBacklog simulates a crash: enqueue a job
// with the client stopped (job sits pending in river_job), then bring up a
// fresh client and assert the backlog job gets worked — no app-level seed,
// River pulls pending jobs from the table on Start.
func TestRiverQueue_CrashRecoveryWorksBacklog(t *testing.T) {
	pool := StartPostgres(t)
	proc := newRecordingProcessor(pool)

	_, attempt := insertPendingLinkAttempt(t, pool, "https://example.com/river-crash")

	// "Pre-crash" client: enqueue but never start (job stays pending = the
	// state a process crash between commit and work would leave).
	q1 := newRiverQueue(t, pool, proc)
	if err := q1.Enqueue(context.Background(), attempt); err != nil {
		t.Fatalf("enqueue on pre-crash client: %v", err)
	}
	if got := countRiverParseJobs(t, pool, attempt); got != 1 {
		t.Fatalf("river_job count before recovery = %d, want 1", got)
	}

	// "Post-restart" fresh client: start it and assert the backlog drains
	// without any manual seed / ResetProcessingToPending.
	q2 := newRiverQueue(t, pool, proc)
	ch := proc.waitChan(attempt)
	if err := q2.Start(context.Background()); err != nil {
		t.Fatalf("start post-restart client: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = q2.Stop(ctx)
	})

	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		t.Fatal("backlog job not worked after restart within 20s")
	}
	if got := proc.seenCount(attempt); got < 1 {
		t.Fatalf("processor saw parse job %d times, want >= 1", got)
	}
}
