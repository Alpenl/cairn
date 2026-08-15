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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
	"webtag/internal/service/linktranslation"
	"webtag/internal/service/translator"
	"webtag/internal/worker"
)

// recordingProcessor is a service.ParseProcessor stub: it records the exact
// parse job it was asked to run and flips that job + its link to done,
// mirroring what ParsePipeline.Run does on success (parse_jobs stays the
// source-of-truth status table; River is the engine). Completion is signalled
// per parse job id so tests can wait deterministically without accepting a
// different attempt for the same link.
type recordingProcessor struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	seen map[uuid.UUID]int
	done map[uuid.UUID]chan struct{}
}

type cancellationAwareProcessor struct {
	targetJobID uuid.UUID
	started     chan struct{}
	cancelled   chan struct{}
	startOnce   sync.Once
	cancelOnce  sync.Once
}

func (p *cancellationAwareProcessor) Run(ctx context.Context, _ uuid.UUID, jobID uuid.UUID) error {
	if jobID != p.targetJobID {
		return nil
	}
	p.startOnce.Do(func() { close(p.started) })
	<-ctx.Done()
	p.cancelOnce.Do(func() { close(p.cancelled) })
	return ctx.Err()
}

func (*cancellationAwareProcessor) RecordDiscard(context.Context, uuid.UUID, uuid.UUID, error) error {
	return nil
}

func newRecordingProcessor(pool *pgxpool.Pool) *recordingProcessor {
	return &recordingProcessor{
		pool: pool,
		seen: make(map[uuid.UUID]int),
		done: make(map[uuid.UUID]chan struct{}),
	}
}

// waitChan returns (creating if needed) the per-job completion channel.
func (p *recordingProcessor) waitChan(jobID uuid.UUID) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, ok := p.done[jobID]
	if !ok {
		ch = make(chan struct{}, 1)
		p.done[jobID] = ch
	}
	return ch
}

func (p *recordingProcessor) Run(ctx context.Context, linkID, jobID uuid.UUID) error {
	if err := p.setTerminal(ctx, linkID, jobID, "done", nil); err != nil {
		return err
	}

	p.mu.Lock()
	p.seen[jobID]++
	ch := p.done[jobID]
	if ch == nil {
		ch = make(chan struct{}, 1)
		p.done[jobID] = ch
	}
	p.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
	return nil
}

func (p *recordingProcessor) RecordDiscard(ctx context.Context, linkID, jobID uuid.UUID, cause error) error {
	errorMsg := "river job discarded"
	if cause != nil {
		errorMsg = cause.Error()
	}
	return p.setTerminal(ctx, linkID, jobID, "failed", &errorMsg)
}

func (p *recordingProcessor) setTerminal(ctx context.Context, linkID, jobID uuid.UUID, status string, errorMsg *string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := tx.Exec(ctx,
		`UPDATE parse_jobs SET status = $3, error_msg = $4, updated_at = now()
		 WHERE id = $1 AND link_id = $2`,
		jobID, linkID, status, errorMsg,
	)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("parse job %s for link %s not found", jobID, linkID)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE links SET status = $2, error_msg = $3, updated_at = now() WHERE id = $1`,
		linkID, status, errorMsg,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *recordingProcessor) seenCount(jobID uuid.UUID) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen[jobID]
}

type noopTranslationProcessor struct{}

func (noopTranslationProcessor) Run(context.Context, model.TranslationAttempt) error { return nil }
func (noopTranslationProcessor) RecordDiscard(context.Context, model.TranslationAttempt, error) error {
	return nil
}
func (noopTranslationProcessor) RecordCancellation(context.Context, model.TranslationAttempt, error) error {
	return nil
}

type countingTranslationProcessor struct {
	delegate          linktranslation.JobProcessor
	mu                sync.Mutex
	cancellationCalls int
}

type invalidatingLegacyTerminalProcessor struct {
	translations *repository.PGXTranslationRepository
	mu           sync.Mutex
	runCalls     int
	discardCalls int
}

func (p *invalidatingLegacyTerminalProcessor) Run(ctx context.Context, attempt model.TranslationAttempt) error {
	p.mu.Lock()
	p.runCalls++
	p.mu.Unlock()
	applied, err := p.translations.Fail(ctx, attempt, "injected terminal proof invalidation")
	if err != nil {
		return err
	}
	if !applied {
		return errors.New("injected terminal proof invalidation was not applied")
	}
	return errors.New("injected final legacy failure")
}

func (p *invalidatingLegacyTerminalProcessor) RecordDiscard(
	context.Context,
	model.TranslationAttempt,
	error,
) error {
	p.mu.Lock()
	p.discardCalls++
	p.mu.Unlock()
	return nil
}

func (*invalidatingLegacyTerminalProcessor) RecordCancellation(
	context.Context,
	model.TranslationAttempt,
	error,
) error {
	return nil
}

func (p *invalidatingLegacyTerminalProcessor) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runCalls, p.discardCalls
}

func (p *countingTranslationProcessor) Run(ctx context.Context, attempt model.TranslationAttempt) error {
	return p.delegate.Run(ctx, attempt)
}

func (p *countingTranslationProcessor) RecordDiscard(
	ctx context.Context,
	attempt model.TranslationAttempt,
	cause error,
) error {
	return p.delegate.RecordDiscard(ctx, attempt, cause)
}

func (p *countingTranslationProcessor) RecordCancellation(
	ctx context.Context,
	attempt model.TranslationAttempt,
	cause error,
) error {
	p.mu.Lock()
	p.cancellationCalls++
	p.mu.Unlock()
	return p.delegate.RecordCancellation(ctx, attempt, cause)
}

func (p *countingTranslationProcessor) cancellationCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cancellationCalls
}

type recordingTranslationAttemptProcessor struct {
	runs chan model.TranslationAttempt
}

type emulatedOldTranslationArgs struct {
	TranslationID uuid.UUID `json:"translation_id"`
}

func (emulatedOldTranslationArgs) Kind() string { return model.TranslationJobKindLegacy }

type emulatedOldTranslationWorker struct {
	river.WorkerDefaults[emulatedOldTranslationArgs]
	runs chan uuid.UUID
}

func (w *emulatedOldTranslationWorker) Work(_ context.Context, job *river.Job[emulatedOldTranslationArgs]) error {
	w.runs <- job.Args.TranslationID
	return nil
}

type blockingTranslationEngine struct {
	started chan struct{}
	once    sync.Once
}

type failingTranslationEngine struct {
	err error
}

type successfulTranslationEngine struct{}

func (successfulTranslationEngine) Translate(context.Context, translator.Request) (translator.Result, error) {
	return translator.Result{Text: "兼容译文", Model: "compat-test"}, nil
}

func (e failingTranslationEngine) Translate(context.Context, translator.Request) (translator.Result, error) {
	return translator.Result{}, e.err
}

func (e *blockingTranslationEngine) Translate(ctx context.Context, _ translator.Request) (translator.Result, error) {
	e.once.Do(func() { close(e.started) })
	<-ctx.Done()
	return translator.Result{}, ctx.Err()
}

func (p *recordingTranslationAttemptProcessor) Run(_ context.Context, attempt model.TranslationAttempt) error {
	p.runs <- attempt
	return nil
}

func (*recordingTranslationAttemptProcessor) RecordDiscard(context.Context, model.TranslationAttempt, error) error {
	return nil
}

func (*recordingTranslationAttemptProcessor) RecordCancellation(context.Context, model.TranslationAttempt, error) error {
	return nil
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

// countRiverParseJobs returns how many river_job rows carry the parse_link kind
// with the exact link_id + parse_job_id pair encoded in args.
func countRiverParseJobs(t *testing.T, pool *pgxpool.Pool, linkID, jobID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job
		 WHERE kind = 'parse_link'
		   AND args->>'link_id' = $1
		   AND args->>'parse_job_id' = $2`,
		linkID.String(), jobID.String(),
	).Scan(&n); err != nil {
		t.Fatalf("count river parse jobs: %v", err)
	}
	return n
}

func TestRiverQueue_TranslationSchedulingFollowsRolloutProtocol(t *testing.T) {
	pool := StartPostgres(t)
	translations := repository.NewPGXTranslationRepository(pool)
	processor := newRecordingProcessor(pool)

	for _, tc := range []struct {
		stage          model.TranslationJobsRolloutStage
		wantKind       string
		wantGeneration bool
		wantPaused     bool
	}{
		{stage: model.TranslationJobsRolloutCompatV1, wantKind: "translate_link_content", wantGeneration: true},
		{stage: model.TranslationJobsRolloutDrainV1, wantPaused: true},
		{stage: model.TranslationJobsRolloutStrictV2, wantKind: "translate_link_v2", wantGeneration: true},
	} {
		t.Run(string(tc.stage), func(t *testing.T) {
			queue, err := worker.NewRiverQueue(worker.RiverQueueOptions{
				Pool:                   pool,
				Processor:              processor,
				TranslationProcessor:   noopTranslationProcessor{},
				TranslationAttempts:    translations,
				TranslationJobsRollout: tc.stage,
				MaxWorkers:             1,
				JobTimeout:             10 * time.Second,
			})
			if err != nil {
				t.Fatalf("NewRiverQueue() error = %v", err)
			}

			sourceRevision := int64(19)
			seed := model.TranslationAttemptSeed{
				TranslationID: uuid.New(), AttemptGeneration: 7,
				SourceHash:            "abababababababababababababababababababababababababababababababab",
				SourceContentRevision: &sourceRevision,
			}
			jobID, err := queue.EnqueueTranslation(t.Context(), seed)
			if tc.wantPaused {
				if !errors.Is(err, worker.ErrTranslationSchedulingPaused) || jobID != 0 {
					t.Fatalf("EnqueueTranslation() = %d, %v; want paused", jobID, err)
				}
				var rows int
				if err := pool.QueryRow(t.Context(),
					`SELECT count(*) FROM river_job WHERE args->>'translation_id' = $1`,
					seed.TranslationID.String(),
				).Scan(&rows); err != nil {
					t.Fatalf("count paused translation jobs: %v", err)
				}
				if rows != 0 {
					t.Fatalf("paused translation rows = %d, want 0", rows)
				}
				return
			}
			if err != nil || jobID <= 0 {
				t.Fatalf("EnqueueTranslation() = %d, %v", jobID, err)
			}

			var kind, translationID, generation, sourceHash, sourceContentRevision string
			if err := pool.QueryRow(t.Context(), `SELECT kind,
				args->>'translation_id', COALESCE(args->>'attempt_generation', ''),
				args->>'source_hash', args->>'source_content_revision'
				FROM river_job WHERE id = $1`, jobID).
				Scan(&kind, &translationID, &generation, &sourceHash, &sourceContentRevision); err != nil {
				t.Fatalf("read scheduled translation job: %v", err)
			}
			wantGeneration := ""
			if tc.wantGeneration {
				wantGeneration = fmt.Sprint(seed.AttemptGeneration)
			}
			if kind != tc.wantKind || translationID != seed.TranslationID.String() || generation != wantGeneration ||
				sourceHash != seed.SourceHash || sourceContentRevision != fmt.Sprint(sourceRevision) {
				t.Fatalf("job kind=%q translation=%q generation=%q hash=%q revision=%q",
					kind, translationID, generation, sourceHash, sourceContentRevision)
			}
		})
	}
}

func TestRiverQueue_CompatAPIWireIsConsumedByEmulatedOldWorker(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	ctx := t.Context()
	linkID := mustCreateDoneLink(t, links, ctx,
		"https://example.com/compat-api-old-worker", "compat API old worker", "example.com")
	scheduler, err := worker.NewRiverQueue(worker.RiverQueueOptions{
		Pool:                   pool,
		Processor:              newRecordingProcessor(pool),
		TranslationProcessor:   noopTranslationProcessor{},
		TranslationAttempts:    translations,
		TranslationJobsRollout: model.TranslationJobsRolloutCompatV1,
		MaxWorkers:             1,
		JobTimeout:             10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRiverQueue(compat scheduler) error = %v", err)
	}
	item, scheduled, err := translations.SchedulePending(
		ctx,
		translationSelectionParams(linkID, "compat API wire"),
		scheduler.EnqueueTranslationTx,
	)
	if err != nil || !scheduled || item.CurrentRiverJobID == nil {
		t.Fatalf("SchedulePending() = %+v, %v, %v", item, scheduled, err)
	}

	var kind, generation, sourceHash string
	var hasSourceContentRevision bool
	if err := pool.QueryRow(t.Context(), `SELECT kind, args->>'attempt_generation',
		args->>'source_hash', args ? 'source_content_revision'
		FROM river_job WHERE id = $1`, *item.CurrentRiverJobID).
		Scan(&kind, &generation, &sourceHash, &hasSourceContentRevision); err != nil {
		t.Fatalf("read compat API wire: %v", err)
	}
	if kind != model.TranslationJobKindLegacy || generation != "1" ||
		sourceHash != item.SourceHash || hasSourceContentRevision {
		t.Fatalf("compat wire kind=%q generation=%q hash=%q has_revision=%v; product=%+v",
			kind, generation, sourceHash, hasSourceContentRevision, item)
	}

	oldWorker := &emulatedOldTranslationWorker{runs: make(chan uuid.UUID, 1)}
	workers := river.NewWorkers()
	river.AddWorker(workers, oldWorker)
	oldClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers: workers,
	})
	if err != nil {
		t.Fatalf("new emulated old River client: %v", err)
	}
	if err := oldClient.Start(t.Context()); err != nil {
		t.Fatalf("start emulated old River client: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = oldClient.Stop(stopCtx)
	})

	select {
	case got := <-oldWorker.runs:
		if got != item.ID {
			t.Fatalf("old worker translation = %s, want %s", got, item.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("emulated old worker did not consume compat API wire")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := oldClient.Stop(stopCtx); err != nil {
		cancel()
		t.Fatalf("stop emulated old River client: %v", err)
	}
	cancel()
	stopped = true

	retained, err := translations.GetByID(ctx, item.ID)
	if err != nil || retained == nil || retained.Status != model.TranslationStatusPending ||
		retained.AttemptGeneration != 1 || retained.CurrentRiverJobID == nil ||
		*retained.CurrentRiverJobID != *item.CurrentRiverJobID {
		t.Fatalf("compat product attempt after old worker = %+v, %v", retained, err)
	}
}

func TestRiverQueue_StrictV2WireRemainsUnclaimedByEmulatedOldWorker(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	ctx := t.Context()
	linkID := mustCreateDoneLink(t, links, ctx,
		"https://example.com/strict-v2-old-worker", "strict v2 old worker", "example.com")
	scheduler, err := worker.NewRiverQueue(worker.RiverQueueOptions{
		Pool:                   pool,
		Processor:              newRecordingProcessor(pool),
		TranslationProcessor:   noopTranslationProcessor{},
		TranslationAttempts:    translations,
		TranslationJobsRollout: model.TranslationJobsRolloutStrictV2,
		MaxWorkers:             1,
		JobTimeout:             10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRiverQueue(strict scheduler) error = %v", err)
	}
	item, scheduled, err := translations.SchedulePending(
		ctx,
		translationSelectionParams(linkID, "strict v2 wire"),
		scheduler.EnqueueTranslationTx,
	)
	if err != nil || !scheduled || item.CurrentRiverJobID == nil {
		t.Fatalf("SchedulePending() = %+v, %v, %v", item, scheduled, err)
	}

	var strictKind string
	if err := pool.QueryRow(t.Context(), `SELECT kind FROM river_job WHERE id = $1`,
		*item.CurrentRiverJobID).Scan(&strictKind); err != nil {
		t.Fatalf("read strict v2 kind: %v", err)
	}
	if strictKind != model.TranslationJobKindV2 {
		t.Fatalf("strict job kind = %q, want %q", strictKind, model.TranslationJobKindV2)
	}

	probeTranslationID := uuid.New()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin old-worker probe transaction: %v", err)
	}
	probeRiverJobID := seedLegacyTranslationRiverJob(t, tx, probeTranslationID, "available")
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit old-worker probe transaction: %v", err)
	}

	oldWorker := &emulatedOldTranslationWorker{runs: make(chan uuid.UUID, 2)}
	workers := river.NewWorkers()
	river.AddWorker(workers, oldWorker)
	oldClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers: workers,
	})
	if err != nil {
		t.Fatalf("new emulated old River client: %v", err)
	}
	if err := oldClient.Start(t.Context()); err != nil {
		t.Fatalf("start emulated old River client: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = oldClient.Stop(stopCtx)
	})

	select {
	case got := <-oldWorker.runs:
		if got != probeTranslationID {
			t.Fatalf("old worker consumed translation %s, want legacy probe %s", got, probeTranslationID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("emulated old worker did not consume legacy probe")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := oldClient.Stop(stopCtx); err != nil {
		cancel()
		t.Fatalf("stop emulated old River client: %v", err)
	}
	cancel()
	stopped = true

	var strictState, probeState string
	if err := pool.QueryRow(t.Context(), `SELECT state::text FROM river_job WHERE id = $1`,
		*item.CurrentRiverJobID).Scan(&strictState); err != nil {
		t.Fatalf("read strict v2 state: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT state::text FROM river_job WHERE id = $1`,
		probeRiverJobID).Scan(&probeState); err != nil {
		t.Fatalf("read legacy probe state: %v", err)
	}
	if strictState != "available" || probeState != "completed" {
		t.Fatalf("strict/probe states = %q/%q, want available/completed", strictState, probeState)
	}
	select {
	case extra := <-oldWorker.runs:
		t.Fatalf("old worker unexpectedly consumed additional translation %s", extra)
	default:
	}
}

func TestRiverQueue_RejectsDuplicateV2AttemptInsertResult(t *testing.T) {
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
		t.Fatalf("count duplicate v2 attempts: %v", err)
	}
	if rows != 1 {
		t.Fatalf("v2 attempt rows = %d, want 1", rows)
	}
}

func TestRiverQueue_CompatWorkerAdoptsPostMigrationLegacyJob(t *testing.T) {
	pool := StartPostgres(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin pre-RF6A API transaction: %v", err)
	}
	translationID := seedLegacyTranslationRow(t, tx, "post-migration-old-api")
	riverJobID := seedLegacyTranslationRiverJob(t, tx, translationID, "available")
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit pre-RF6A API transaction: %v", err)
	}

	ctx := t.Context()
	translations := repository.NewPGXTranslationRepository(pool)
	before, err := translations.GetByID(ctx, translationID)
	if err != nil || before == nil {
		t.Fatalf("GetByID(before) = %+v, %v", before, err)
	}
	if before.AttemptGeneration != 0 || before.CurrentRiverJobID != nil ||
		before.Status != model.TranslationStatusPending {
		t.Fatalf("pre-RF6A product row = %+v", before)
	}

	processor := linktranslation.NewProcessor(linktranslation.ProcessorOptions{
		Translations: translations,
		Translator:   successfulTranslationEngine{},
	})
	queue, err := worker.NewRiverQueue(worker.RiverQueueOptions{
		Pool:                   pool,
		Processor:              newRecordingProcessor(pool),
		TranslationProcessor:   processor,
		TranslationAttempts:    translations,
		TranslationJobsRollout: model.TranslationJobsRolloutCompatV1,
		TranslationJobTimeout:  10 * time.Second,
		MaxWorkers:             1,
		JobTimeout:             10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRiverQueue() error = %v", err)
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
			t.Fatalf("GetByID(projected) error = %v", readErr)
		}
		if projected != nil && projected.Status == model.TranslationStatusDone {
			if projected.AttemptGeneration != 0 || projected.CurrentRiverJobID != nil ||
				projected.TranslatedText == nil || *projected.TranslatedText != "兼容译文" {
				t.Fatalf("completed legacy translation = %+v", projected)
			}
			break
		}

		var riverState string
		if err := pool.QueryRow(t.Context(), `SELECT state::text FROM river_job WHERE id = $1`, riverJobID).Scan(&riverState); err != nil {
			t.Fatalf("read legacy River state: %v", err)
		}
		if riverState == "completed" || riverState == "cancelled" || riverState == "discarded" {
			t.Fatalf("legacy River job finalized as %s while product remained %+v", riverState, projected)
		}
		if time.Now().After(deadline) {
			t.Fatalf("legacy translation did not complete: %+v, River=%s", projected, riverState)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRiverQueue_RunningTranslationCancellationProjectsStableTerminalState(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	ctx := t.Context()
	linkID := mustCreateDoneLink(t, links, ctx,
		"https://example.com/running-translation-cancel", "running translation cancel", "example.com")
	engine := &blockingTranslationEngine{started: make(chan struct{})}
	translationProcessor := linktranslation.NewProcessor(linktranslation.ProcessorOptions{
		Translations: translations,
		Translator:   engine,
	})
	countingProcessor := &countingTranslationProcessor{delegate: translationProcessor}
	queue, err := worker.NewRiverQueue(worker.RiverQueueOptions{
		Pool:                   pool,
		Processor:              newRecordingProcessor(pool),
		TranslationProcessor:   countingProcessor,
		TranslationAttempts:    translations,
		TranslationJobsRollout: model.TranslationJobsRolloutStrictV2,
		TranslationJobTimeout:  10 * time.Second,
		MaxWorkers:             1,
		JobTimeout:             10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRiverQueue() error = %v", err)
	}
	item, scheduled, err := translations.SchedulePending(
		ctx,
		translationSelectionParams(linkID, "cancel running translation"),
		queue.EnqueueTranslationTx,
	)
	if err != nil || !scheduled || item.CurrentRiverJobID == nil {
		t.Fatalf("SchedulePending() = %+v, %v, %v", item, scheduled, err)
	}
	if err := queue.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start() error = %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = queue.Stop(stopCtx)
	})
	select {
	case <-engine.started:
	case <-time.After(5 * time.Second):
		t.Fatal("translation worker did not reach translator")
	}

	control, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatalf("river.NewClient() error = %v", err)
	}
	if _, err := control.JobCancel(t.Context(), *item.CurrentRiverJobID); err != nil {
		t.Fatalf("JobCancel() error = %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		projected, readErr := translations.GetByID(ctx, item.ID)
		if readErr != nil {
			t.Fatalf("GetByID() error = %v", readErr)
		}
		var riverState string
		if err := pool.QueryRow(t.Context(), `SELECT state::text FROM river_job WHERE id = $1`,
			*item.CurrentRiverJobID).Scan(&riverState); err != nil {
			t.Fatalf("read cancelled River translation: %v", err)
		}
		if projected != nil && projected.Status == model.TranslationStatusFailed && riverState == "cancelled" {
			if projected.CurrentRiverJobID != nil || projected.ErrorMsg == nil ||
				*projected.ErrorMsg != "翻译任务已取消，请重试" {
				t.Fatalf("cancelled translation = %+v", projected)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("translation not cancelled: %+v", projected)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if calls := countingProcessor.cancellationCount(); calls != 1 {
		t.Fatalf("RecordCancellation() calls = %d, want exactly 1", calls)
	}
}

func TestRiverQueue_OrdinaryTranslationCancellationHasOneTerminalOwner(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	ctx := t.Context()
	linkID := mustCreateDoneLink(t, links, ctx,
		"https://example.com/ordinary-translation-cancel", "ordinary translation cancel", "example.com")
	realProcessor := linktranslation.NewProcessor(linktranslation.ProcessorOptions{
		Translations: translations,
		Translator:   failingTranslationEngine{err: context.Canceled},
	})
	countingProcessor := &countingTranslationProcessor{delegate: realProcessor}
	queue, err := worker.NewRiverQueue(worker.RiverQueueOptions{
		Pool:                   pool,
		Processor:              newRecordingProcessor(pool),
		TranslationProcessor:   countingProcessor,
		TranslationAttempts:    translations,
		TranslationJobsRollout: model.TranslationJobsRolloutStrictV2,
		TranslationJobTimeout:  10 * time.Second,
		MaxWorkers:             1,
		JobTimeout:             10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRiverQueue() error = %v", err)
	}
	item, scheduled, err := translations.SchedulePending(
		ctx,
		translationSelectionParams(linkID, "ordinary cancellation"),
		queue.EnqueueTranslationTx,
	)
	if err != nil || !scheduled || item.CurrentRiverJobID == nil {
		t.Fatalf("SchedulePending() = %+v, %v, %v", item, scheduled, err)
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
		projected, readErr := translations.GetByID(ctx, item.ID)
		if readErr != nil {
			t.Fatalf("GetByID() error = %v", readErr)
		}
		var riverState string
		if err := pool.QueryRow(t.Context(), `SELECT state::text FROM river_job WHERE id = $1`,
			*item.CurrentRiverJobID).Scan(&riverState); err != nil {
			t.Fatalf("read ordinary-cancel River translation: %v", err)
		}
		if projected != nil && projected.Status == model.TranslationStatusFailed && riverState == "cancelled" {
			if projected.CurrentRiverJobID != nil || projected.ErrorMsg == nil ||
				*projected.ErrorMsg != "翻译任务已取消，请重试" {
				t.Fatalf("cancelled translation = %+v", projected)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ordinary cancellation not finalized: product=%+v River=%s", projected, riverState)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if calls := countingProcessor.cancellationCount(); calls != 1 {
		t.Fatalf("RecordCancellation() calls = %d, want exactly 1", calls)
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
		Pool:                   pool,
		Processor:              newRecordingProcessor(pool),
		TranslationProcessor:   translationProcessor,
		TranslationAttempts:    translations,
		TranslationJobsRollout: model.TranslationJobsRolloutStrictV2,
		TranslationJobTimeout:  10 * time.Second,
		MaxWorkers:             1,
		JobTimeout:             10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRiverQueue() error = %v", err)
	}
	item, scheduled, err := translations.SchedulePending(
		ctx,
		translationSelectionParams(linkID, "final translation failure"),
		queue.EnqueueTranslationTx,
	)
	if err != nil || !scheduled || item.CurrentRiverJobID == nil {
		t.Fatalf("SchedulePending() = %+v, %v, %v", item, scheduled, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE river_job
		SET attempt = max_attempts - 1 WHERE id = $1`, *item.CurrentRiverJobID); err != nil {
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
		projected, readErr := translations.GetByID(ctx, item.ID)
		if readErr != nil {
			t.Fatalf("GetByID() error = %v", readErr)
		}
		if projected != nil && projected.Status == model.TranslationStatusFailed {
			if projected.CurrentRiverJobID != nil || projected.ErrorMsg == nil ||
				*projected.ErrorMsg != "翻译服务暂时不可用，请重试" {
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

func TestRiverQueue_ProductionLegacyTerminalHandlerLogsRejectedProof(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	ctx := t.Context()
	linkID := mustCreateDoneLink(t, links, ctx,
		"https://example.com/legacy-terminal-handler-wiring",
		"legacy terminal handler wiring", "example.com")
	processor := &invalidatingLegacyTerminalProcessor{translations: translations}
	var logs bytes.Buffer
	queue, err := worker.NewRiverQueue(worker.RiverQueueOptions{
		Pool:                   pool,
		Processor:              newRecordingProcessor(pool),
		TranslationProcessor:   processor,
		TranslationAttempts:    translations,
		TranslationJobsRollout: model.TranslationJobsRolloutCompatV1,
		TranslationJobTimeout:  10 * time.Second,
		MaxWorkers:             1,
		JobTimeout:             10 * time.Second,
		Logger:                 slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewRiverQueue() error = %v", err)
	}
	item, scheduled, err := translations.SchedulePending(
		ctx,
		translationSelectionParams(linkID, "legacy terminal handler wiring"),
		queue.EnqueueTranslationTx,
	)
	if err != nil || !scheduled || item.CurrentRiverJobID == nil {
		t.Fatalf("SchedulePending() = %+v, %v, %v", item, scheduled, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE river_job
		SET attempt = max_attempts - 1 WHERE id = $1`, *item.CurrentRiverJobID); err != nil {
		t.Fatalf("prepare final legacy attempt: %v", err)
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
	var riverState string
	for {
		if err := pool.QueryRow(t.Context(), `SELECT state::text FROM river_job WHERE id = $1`,
			*item.CurrentRiverJobID).Scan(&riverState); err != nil {
			t.Fatalf("read legacy terminal River state: %v", err)
		}
		if riverState == "discarded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("legacy terminal job state = %q, want discarded", riverState)
		}
		time.Sleep(20 * time.Millisecond)
	}
	runs, discards := processor.counts()
	if runs != 1 || discards != 0 {
		t.Fatalf("legacy processor runs/discards = %d/%d, want 1/0", runs, discards)
	}
	if got := logs.String(); !strings.Contains(got, "translation terminal projection rejected") ||
		!strings.Contains(got, "reason="+model.TranslationAttemptRejectionSourceStatusMismatch.String()) ||
		!strings.Contains(got, "kind="+model.TranslationJobKindLegacy) {
		t.Fatalf("production legacy terminal log = %q, want typed proof rejection", got)
	}
}

func TestRiverQueue_TranslationWorkerRegistrationFollowsRollout(t *testing.T) {
	for _, tc := range []struct {
		stage      model.TranslationJobsRolloutStage
		wantLegacy bool
		wantV2     bool
	}{
		{stage: model.TranslationJobsRolloutCompatV1, wantLegacy: true, wantV2: true},
		{stage: model.TranslationJobsRolloutDrainV1, wantLegacy: true, wantV2: true},
		{stage: model.TranslationJobsRolloutStrictV2, wantV2: true},
	} {
		t.Run(string(tc.stage), func(t *testing.T) {
			pool := StartPostgres(t)
			links := repository.NewPGXLinkRepository(pool)
			translations := repository.NewPGXTranslationRepository(pool)
			ctx := t.Context()
			linkID := mustCreateDoneLink(t, links, ctx,
				"https://example.com/translation-workers-"+string(tc.stage),
				"translation workers", "example.com")

			recorder := &recordingTranslationAttemptProcessor{runs: make(chan model.TranslationAttempt, 4)}
			newQueue := func(stage model.TranslationJobsRolloutStage, processor linktranslation.JobProcessor) *worker.RiverQueue {
				t.Helper()
				queue, err := worker.NewRiverQueue(worker.RiverQueueOptions{
					Pool:                   pool,
					Processor:              newRecordingProcessor(pool),
					TranslationProcessor:   processor,
					TranslationAttempts:    translations,
					TranslationJobsRollout: stage,
					TranslationJobTimeout:  10 * time.Second,
					MaxWorkers:             2,
					JobTimeout:             10 * time.Second,
				})
				if err != nil {
					t.Fatalf("NewRiverQueue(%s) error = %v", stage, err)
				}
				return queue
			}

			params := func(source string) repository.UpsertTranslationParams {
				return repository.UpsertTranslationParams{
					LinkID: linkID, Scope: model.TranslationScopeSelection,
					BlockKey: "summary", StartOffset: 0, EndOffset: len(source),
					SourceText: source, SourceFormat: model.TranslationFormatPlain,
					TargetLanguage: model.TranslationTargetChinese,
					SourceHash:     fmt.Sprintf("%x", sha256.Sum256([]byte(source))),
				}
			}

			legacyScheduler := newQueue(model.TranslationJobsRolloutCompatV1, noopTranslationProcessor{})
			legacy, scheduled, err := translations.SchedulePending(ctx, params("legacy source"), legacyScheduler.EnqueueTranslationTx)
			if err != nil || !scheduled {
				t.Fatalf("schedule legacy = %+v, %v, %v", legacy, scheduled, err)
			}
			v2Scheduler := newQueue(model.TranslationJobsRolloutStrictV2, noopTranslationProcessor{})
			v2, scheduled, err := translations.SchedulePending(ctx, params("v2 source"), v2Scheduler.EnqueueTranslationTx)
			if err != nil || !scheduled {
				t.Fatalf("schedule v2 = %+v, %v, %v", v2, scheduled, err)
			}

			runner := newQueue(tc.stage, recorder)
			if err := runner.Start(t.Context()); err != nil {
				t.Fatalf("Start(%s) error = %v", tc.stage, err)
			}
			stopped := false
			t.Cleanup(func() {
				if stopped {
					return
				}
				stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := runner.Stop(stopCtx); err != nil {
					t.Errorf("Stop(%s) error = %v", tc.stage, err)
				}
			})

			wantCount := 0
			if tc.wantLegacy {
				wantCount++
			}
			if tc.wantV2 {
				wantCount++
			}
			seen := make(map[uuid.UUID]model.TranslationAttempt, wantCount)
			waitCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			for len(seen) < wantCount {
				select {
				case attempt := <-recorder.runs:
					seen[attempt.TranslationID] = attempt
				case <-waitCtx.Done():
					t.Fatalf("timed out waiting for workers: seen=%v", seen)
				}
			}
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := runner.Stop(stopCtx); err != nil {
				stopCancel()
				t.Fatalf("Stop(%s) error = %v", tc.stage, err)
			}
			stopCancel()
			stopped = true
		drainRuns:
			for {
				select {
				case attempt := <-recorder.runs:
					seen[attempt.TranslationID] = attempt
				default:
					break drainRuns
				}
			}
			if _, ok := seen[legacy.ID]; ok != tc.wantLegacy {
				t.Fatalf("legacy worker ran=%v, want %v", ok, tc.wantLegacy)
			}
			if _, ok := seen[v2.ID]; ok != tc.wantV2 {
				t.Fatalf("v2 worker ran=%v, want %v", ok, tc.wantV2)
			}

		})
	}
}

// insertPendingLinkAndJob creates the product state that every parse_link
// River job must reference. River itself has no FK into parse_jobs, so keeping
// this fixture explicit prevents direct-enqueue tests from passing with args
// that the real processor could never resolve.
func insertPendingLinkAndJob(t *testing.T, pool *pgxpool.Pool, rawURL string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	var linkID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO links (url, source_key, status, first_collected_at)
		 VALUES ($1, $1, 'pending', NOW()) RETURNING id`,
		rawURL,
	).Scan(&linkID); err != nil {
		t.Fatalf("insert pending link: %v", err)
	}

	var jobID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO parse_jobs (link_id, status) VALUES ($1, 'pending') RETURNING id`,
		linkID,
	).Scan(&jobID); err != nil {
		t.Fatalf("insert pending parse job: %v", err)
	}
	return linkID, jobID
}

func seedLegacyTranslationRow(t *testing.T, tx pgx.Tx, suffix string) uuid.UUID {
	t.Helper()
	var linkID uuid.UUID
	if err := tx.QueryRow(t.Context(), `INSERT INTO links (
		url, source_key, status, first_collected_at
	) VALUES ($1, $1, 'done', NOW()) RETURNING id`,
		"https://example.com/legacy-translation/"+suffix).Scan(&linkID); err != nil {
		t.Fatalf("seed legacy link %s: %v", suffix, err)
	}
	var translationID uuid.UUID
	if err := tx.QueryRow(t.Context(), `INSERT INTO link_translations (
		link_id, scope, block_key, start_offset, end_offset,
		source_text, source_format, target_language, source_hash, status,
		attempt_generation, current_river_job_id
	) VALUES ($1, 'selection', 'summary', 0, 5,
		'hello', 'plain', 'zh-CN', $2, 'pending', 0, NULL) RETURNING id`,
		linkID, fmt.Sprintf("%064x", len(suffix)+1)).Scan(&translationID); err != nil {
		t.Fatalf("seed legacy translation %s: %v", suffix, err)
	}
	return translationID
}

func seedLegacyTranslationRiverJob(
	t *testing.T,
	tx pgx.Tx,
	translationID uuid.UUID,
	state string,
) int64 {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{"translation_id": translationID.String()})
	if err != nil {
		t.Fatalf("marshal legacy translation args: %v", err)
	}
	var jobID int64
	if err := tx.QueryRow(t.Context(), `INSERT INTO river_job (args, kind, max_attempts, state)
		VALUES ($1::jsonb, 'translate_link_content', 3, 'available') RETURNING id`, encoded).Scan(&jobID); err != nil {
		t.Fatalf("seed legacy River job: %v", err)
	}
	if state != "available" {
		if _, err := tx.Exec(t.Context(), `UPDATE river_job
			SET state = $2::river_job_state,
				finalized_at = CASE WHEN $2 IN ('completed', 'cancelled', 'discarded') THEN NOW() ELSE NULL END
			WHERE id = $1`, jobID, state); err != nil {
			t.Fatalf("set legacy River state %s: %v", state, err)
		}
	}
	return jobID
}

func serviceCapture(capture repository.CreateLinkParams) service.LinkCapture {
	return service.LinkCapture{
		URL:                        capture.URL,
		SourceKind:                 capture.SourceKind,
		SourceKey:                  capture.SourceKey,
		InputTitle:                 capture.InputTitle,
		InputText:                  capture.InputText,
		InputHTML:                  capture.InputHTML,
		InputImages:                capture.InputImages,
		SourceMetadata:             capture.SourceMetadata,
		Description:                capture.Description,
		Status:                     capture.Status,
		Domain:                     capture.Domain,
		ContentType:                capture.ContentType,
		PathDepth:                  capture.PathDepth,
		ParentPath:                 capture.ParentPath,
		ParentID:                   capture.ParentID,
		RequestedLibraryKind:       capture.RequestedLibraryKind,
		RequestedLibraryKindSource: capture.RequestedLibraryKindSource,
		PredictedLibraryKind:       capture.PredictedLibraryKind,
	}
}

func requeueWithRiver(t *testing.T, pool *pgxpool.Pool, repo *repository.PGXLinkRepository, queue *worker.RiverQueue, linkID uuid.UUID, capture *repository.CreateLinkParams) *model.ParseJob {
	t.Helper()
	var applicationCapture *service.LinkCapture
	if capture != nil {
		converted := serviceCapture(*capture)
		applicationCapture = &converted
	}
	result, err := dbLinkCommands(pool, repo, queue).RequeueLink(context.Background(), service.RequeueLinkCommand{
		LinkID:  linkID,
		Capture: applicationCapture,
	})
	if err != nil {
		t.Fatalf("RequeueLink: %v", err)
	}
	return result.Job
}

func TestRiverQueue_RequeueCancelsAvailableAttempt(t *testing.T) {
	pool := StartPostgres(t)
	proc := newRecordingProcessor(pool)
	queue := newRiverQueue(t, pool, proc)
	repo := repository.NewPGXLinkRepository(pool)
	linkID, oldJobID := insertPendingLinkAndJob(t, pool, "https://example.com/requeue-cancel-available")
	if err := queue.Enqueue(context.Background(), linkID, oldJobID); err != nil {
		t.Fatalf("enqueue old attempt: %v", err)
	}
	text := "new captured body"
	newJob := requeueWithRiver(t, pool, repo, queue, linkID, &repository.CreateLinkParams{
		URL: "https://example.com/requeue-cancel-available", SourceKind: "browser_capture",
		SourceKey: "capture:requeue-cancel-available", InputText: &text,
	})

	var oldState string
	var cancelAttempted bool
	if err := pool.QueryRow(context.Background(), `SELECT state::text, metadata ? 'cancel_attempted_at'
		FROM river_job WHERE kind='parse_link' AND args->>'parse_job_id'=$1`, oldJobID.String()).
		Scan(&oldState, &cancelAttempted); err != nil {
		t.Fatalf("read old River attempt: %v", err)
	}
	if oldState != "cancelled" || !cancelAttempted {
		t.Fatalf("old River attempt = state %q cancel_attempted=%v, want cancelled/true", oldState, cancelAttempted)
	}
	if got := countRiverParseJobs(t, pool, linkID, newJob.ID); got != 1 {
		t.Fatalf("new River attempt count = %d, want 1", got)
	}
}

func TestRiverQueue_RequeueCancelsRunningWorkerContext(t *testing.T) {
	pool := StartPostgres(t)
	linkID, oldJobID := insertPendingLinkAndJob(t, pool, "https://example.com/requeue-cancel-running")
	proc := &cancellationAwareProcessor{
		targetJobID: oldJobID,
		started:     make(chan struct{}),
		cancelled:   make(chan struct{}),
	}
	queue := newRiverQueue(t, pool, proc)
	if err := queue.Enqueue(context.Background(), linkID, oldJobID); err != nil {
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
	newJob := requeueWithRiver(t, pool, repo, queue, linkID, nil)
	var marked bool
	if err := pool.QueryRow(context.Background(), `SELECT metadata ? 'cancel_attempted_at'
		FROM river_job WHERE kind='parse_link' AND args->>'parse_job_id'=$1`, oldJobID.String()).Scan(&marked); err != nil {
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
			WHERE kind='parse_link' AND args->>'parse_job_id'=$1`, oldJobID.String()).Scan(&state); err != nil {
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
	if got := countRiverParseJobs(t, pool, linkID, newJob.ID); got != 1 {
		t.Fatalf("new River attempt count = %d, want 1", got)
	}
}

// TestRiverQueue_SubmitEnqueuesAndWorksToDone is the happy path: SubmitLink
// inserts a link + parse_jobs row AND a river_job in one tx; once the queue
// starts, the worker runs Run and the link reaches done.
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
	link, job := result.Link, result.Job

	if got := countRiverParseJobs(t, pool, link.ID, job.ID); got != 1 {
		t.Fatalf("river_job count after submit = %d, want 1", got)
	}

	ch := proc.waitChan(job.ID)
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
	if err := pool.QueryRow(context.Background(), `SELECT status FROM parse_jobs WHERE id = $1`, job.ID).Scan(&status); err != nil {
		t.Fatalf("read parse job status: %v", err)
	}
	if status != "done" {
		t.Fatalf("parse job status = %q, want done", status)
	}
}

// TestRiverQueue_UniqueDedupesInFlight verifies River's unique-job (ByArgs)
// dedupe: enqueuing the same link_id + parse_job_id pair twice while the first
// is still pending produces only one river_job row (replacing the old
// in-flight map dedupe).
func TestRiverQueue_UniqueDedupesInFlight(t *testing.T) {
	pool := StartPostgres(t)
	proc := newRecordingProcessor(pool)
	// Build the queue but DON'T start it, so the first job stays available
	// (in-flight from the unique-state perspective) when we enqueue again.
	q := newRiverQueue(t, pool, proc)

	linkID, jobID := insertPendingLinkAndJob(t, pool, "https://example.com/river-unique")

	if err := q.Enqueue(context.Background(), linkID, jobID); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if err := q.Enqueue(context.Background(), linkID, jobID); err != nil {
		t.Fatalf("second enqueue (should be deduped, not error): %v", err)
	}

	if got := countRiverParseJobs(t, pool, linkID, jobID); got != 1 {
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
	jobID := uuid.New()
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
	if _, err := tx.Exec(context.Background(),
		`INSERT INTO parse_jobs (id, link_id, status) VALUES ($1, $2, 'pending')`,
		jobID, linkID,
	); err != nil {
		t.Fatalf("insert parse job in tx: %v", err)
	}
	if err := q.EnqueueTx(context.Background(), tx, linkID, jobID); err != nil {
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if got := countRiverParseJobs(t, pool, linkID, jobID); got != 0 {
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
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM parse_jobs WHERE id = $1`, jobID).Scan(&n); err != nil {
		t.Fatalf("count parse jobs: %v", err)
	}
	if n != 0 {
		t.Fatalf("parse_jobs count after rollback = %d, want 0", n)
	}
}

// TestRiverQueue_CrashRecoveryWorksBacklog simulates a crash: enqueue a job
// with the client stopped (job sits pending in river_job), then bring up a
// fresh client and assert the backlog job gets worked — no app-level seed,
// River pulls pending jobs from the table on Start.
func TestRiverQueue_CrashRecoveryWorksBacklog(t *testing.T) {
	pool := StartPostgres(t)
	proc := newRecordingProcessor(pool)

	linkID, jobID := insertPendingLinkAndJob(t, pool, "https://example.com/river-crash")

	// "Pre-crash" client: enqueue but never start (job stays pending = the
	// state a process crash between commit and work would leave).
	q1 := newRiverQueue(t, pool, proc)
	if err := q1.Enqueue(context.Background(), linkID, jobID); err != nil {
		t.Fatalf("enqueue on pre-crash client: %v", err)
	}
	if got := countRiverParseJobs(t, pool, linkID, jobID); got != 1 {
		t.Fatalf("river_job count before recovery = %d, want 1", got)
	}

	// "Post-restart" fresh client: start it and assert the backlog drains
	// without any manual seed / ResetProcessingToPending.
	q2 := newRiverQueue(t, pool, proc)
	ch := proc.waitChan(jobID)
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
	if got := proc.seenCount(jobID); got < 1 {
		t.Fatalf("processor saw parse job %d times, want >= 1", got)
	}
}
