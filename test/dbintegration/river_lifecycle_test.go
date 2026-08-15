package dbintegration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
	"webtag/internal/service/urllock"
	"webtag/internal/worker"
)

func TestLinkDeleteCancelsPendingRiverAttemptAtomically(t *testing.T) {
	pool := StartPostgres(t)
	linkID, jobID := insertPendingLinkAndJob(t, pool, "https://example.com/delete-pending")
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := queue.Enqueue(t.Context(), linkID, jobID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	deleteLinkThroughLifecycle(t, pool, queue, linkID)
	assertLinkLifecycleAndCancelledRiver(t, pool, linkID, jobID)
}

func TestLinkDeleteCancelsPendingTranslationAttemptAtomically(t *testing.T) {
	pool := StartPostgres(t)
	linkID, _ := insertPendingLinkAndJob(t, pool, "https://example.com/delete-pending-translation")
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	translationID := schedulePendingTranslation(t, pool, queue, context.Background(), linkID, "translate before link delete")

	deleteLinkThroughLifecycle(t, pool, queue, linkID)
	assertTranslationLifecycleAndRiverCancelled(t, pool, translationID, 1)
}

func TestLinkDeleteCancelsPendingLegacyTranslationAttemptAtomically(t *testing.T) {
	pool := StartPostgres(t)
	linkID, _ := insertPendingLinkAndJob(t, pool, "https://example.com/delete-pending-legacy-translation")
	translations := repository.NewPGXTranslationRepository(pool)
	queue, err := worker.NewRiverQueue(worker.RiverQueueOptions{
		Pool:                   pool,
		Processor:              newRecordingProcessor(pool),
		TranslationProcessor:   noopTranslationProcessor{},
		TranslationAttempts:    translations,
		TranslationJobsRollout: model.TranslationJobsRolloutCompatV1,
		MaxWorkers:             1,
		JobTimeout:             10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRiverQueue(compat-v1): %v", err)
	}
	translationID := schedulePendingTranslation(t, pool, queue, context.Background(), linkID, "legacy translate before link delete")

	deleteLinkThroughLifecycle(t, pool, queue, linkID)
	assertTranslationLifecycleAndRiverCancelled(t, pool, translationID, 1)
}

func TestLinkDeleteCancelsRunningWorkerContext(t *testing.T) {
	pool := StartPostgres(t)
	linkID, jobID := insertPendingLinkAndJob(t, pool, "https://example.com/delete-running")
	proc := &cancellationAwareProcessor{
		targetJobID: jobID,
		started:     make(chan struct{}),
		cancelled:   make(chan struct{}),
	}
	queue := newRiverQueue(t, pool, proc)
	if err := queue.Enqueue(t.Context(), linkID, jobID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := queue.Start(t.Context()); err != nil {
		t.Fatalf("start queue: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = queue.Stop(stopCtx)
	})
	select {
	case <-proc.started:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not start")
	}

	deleteLinkThroughLifecycle(t, pool, queue, linkID)
	select {
	case <-proc.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("running worker context was not cancelled")
	}
	assertLinkLifecycleAndCancelledRiver(t, pool, linkID, jobID)
}

func deleteLinkThroughLifecycle(t *testing.T, pool *pgxpool.Pool, queue *worker.RiverQueue, linkID uuid.UUID) {
	t.Helper()
	repo := repository.NewPGXLinkRepository(pool)
	commands := dbLinkCommands(pool, repo, queue)
	svc := service.NewLinkReadService(service.LinkReadServiceOptions{
		Links:          repo,
		DeleteCommands: commands,
		MutationLocker: urllock.NewInProcessURLLocker(),
	})
	if err := svc.Delete(t.Context(), linkID.String()); err != nil {
		t.Fatalf("Delete(): %v", err)
	}
}

func assertLinkLifecycleAndCancelledRiver(t *testing.T, pool *pgxpool.Pool, linkID, jobID uuid.UUID) {
	t.Helper()
	var trashed bool
	if err := pool.QueryRow(t.Context(), `SELECT deleted_at IS NOT NULL FROM links WHERE id=$1`, linkID).Scan(&trashed); err != nil {
		t.Fatalf("read trashed link: %v", err)
	}
	if !trashed {
		t.Fatal("single-link delete did not move the link to trash")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var state string
		if err := pool.QueryRow(t.Context(), `SELECT state::text FROM river_job
			WHERE kind='parse_link' AND args->>'parse_job_id'=$1`, jobID.String()).Scan(&state); err != nil {
			t.Fatalf("read River state: %v", err)
		}
		if state == "cancelled" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("River state = %q, want cancelled", state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func schedulePendingTranslation(t *testing.T, pool *pgxpool.Pool, queue *worker.RiverQueue, ctx context.Context, linkID uuid.UUID, source string) uuid.UUID {
	t.Helper()
	repo := repository.NewPGXTranslationRepository(pool)
	item, scheduled, err := repo.SchedulePending(ctx, repository.UpsertTranslationParams{
		LinkID:         linkID,
		Scope:          model.TranslationScopeSelection,
		BlockKey:       "summary",
		StartOffset:    0,
		EndOffset:      len(source),
		SourceText:     source,
		SourceFormat:   model.TranslationFormatPlain,
		TargetLanguage: model.TranslationTargetChinese,
		SourceHash:     fmt.Sprintf("%x", sha256.Sum256([]byte(source))),
	}, queue.EnqueueTranslationTx)
	if err != nil || !scheduled || item == nil {
		t.Fatalf("SchedulePending(): item=%+v scheduled=%v error=%v", item, scheduled, err)
	}
	return item.ID
}

func assertTranslationLifecycleAndRiverCancelled(t *testing.T, pool *pgxpool.Pool, translationID uuid.UUID, wantRows int) {
	t.Helper()
	var translations int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM link_translations WHERE id=$1`, translationID).Scan(&translations); err != nil {
		t.Fatalf("count deleted translation: %v", err)
	}
	if translations != wantRows {
		t.Fatalf("translation row count = %d, want %d", translations, wantRows)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var state string
		if err := pool.QueryRow(t.Context(), `SELECT state::text FROM river_job
			WHERE kind IN ('translate_link_content', 'translate_link_v2')
			  AND args->>'translation_id'=$1`, translationID.String()).Scan(&state); err != nil {
			t.Fatalf("read translation River state: %v", err)
		}
		if state == "cancelled" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("translation River state = %q, want cancelled", state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
