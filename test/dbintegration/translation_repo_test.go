package dbintegration

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"webtag/internal/model"
	"webtag/internal/repository"
)

func TestTranslationRepositoryRealDBStateMachine(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	ctx := t.Context()
	linkID := mustCreateDoneLink(
		t,
		links,
		ctx,
		"https://example.com/translation-state",
		"translation",
		"example.com",
	)
	source := "Persistent translation works."
	params := repository.UpsertTranslationParams{
		LinkID:         linkID,
		Scope:          model.TranslationScopeSelection,
		BlockKey:       "summary",
		StartOffset:    0,
		EndOffset:      len(source),
		SourceText:     source,
		SourceFormat:   model.TranslationFormatPlain,
		TargetLanguage: model.TranslationTargetChinese,
		SourceHash:     fmt.Sprintf("%x", sha256.Sum256([]byte(source))),
	}

	const firstRiverJobID int64 = 1001
	created, scheduled, err := translations.SchedulePending(ctx, params, func(context.Context, pgx.Tx, repository.TranslationScheduleCommand) (int64, error) {
		return firstRiverJobID, nil
	})
	if err != nil {
		t.Fatalf("first SchedulePending() error = %v", err)
	}
	if !scheduled || created == nil || created.Status != model.TranslationStatusPending {
		t.Fatalf("first SchedulePending() = %+v, scheduled=%v", created, scheduled)
	}

	reused, scheduled, err := translations.SchedulePending(ctx, params, nil)
	if err != nil {
		t.Fatalf("active SchedulePending() error = %v", err)
	}
	if scheduled || reused.ID != created.ID {
		t.Fatalf("active SchedulePending() = %+v, scheduled=%v", reused, scheduled)
	}

	firstAttempt := model.TranslationAttempt{
		TranslationID:     created.ID,
		AttemptGeneration: created.AttemptGeneration, RiverJobID: firstRiverJobID,
		SourceHash: created.SourceHash, SourceContentRevision: created.SourceContentRevision,
	}
	processing, err := translations.MarkProcessing(ctx, firstAttempt)
	if err != nil || processing == nil || processing.Status != model.TranslationStatusProcessing {
		t.Fatalf("MarkProcessing() = %+v, %v", processing, err)
	}
	completed, err := translations.Complete(ctx, firstAttempt, "持久翻译有效。", "grok-4.3-fast")
	if err != nil || !completed {
		t.Fatalf("Complete() = %v, %v", completed, err)
	}

	cached, scheduled, err := translations.SchedulePending(ctx, params, nil)
	if err != nil {
		t.Fatalf("cached SchedulePending() error = %v", err)
	}
	if scheduled || cached.Status != model.TranslationStatusDone || cached.TranslatedText == nil {
		t.Fatalf("cached SchedulePending() = %+v, scheduled=%v", cached, scheduled)
	}

	params.Force = true
	const secondRiverJobID int64 = 1002
	retried, scheduled, err := translations.SchedulePending(ctx, params, func(context.Context, pgx.Tx, repository.TranslationScheduleCommand) (int64, error) {
		return secondRiverJobID, nil
	})
	if err != nil {
		t.Fatalf("forced SchedulePending() error = %v", err)
	}
	if !scheduled || retried.ID != created.ID || retried.Status != model.TranslationStatusPending || retried.TranslatedText != nil ||
		retried.AttemptGeneration != created.AttemptGeneration+1 || retried.CurrentRiverJobID == nil || *retried.CurrentRiverJobID != secondRiverJobID {
		t.Fatalf("forced SchedulePending() = %+v, scheduled=%v", retried, scheduled)
	}
	secondAttempt := model.TranslationAttempt{
		TranslationID:     retried.ID,
		AttemptGeneration: retried.AttemptGeneration, RiverJobID: secondRiverJobID,
		SourceHash: retried.SourceHash, SourceContentRevision: retried.SourceContentRevision,
	}
	if applied, err := translations.Fail(ctx, secondAttempt, "retryable failure"); err != nil || !applied {
		t.Fatalf("Fail(second attempt) = %v, %v", applied, err)
	}
	const thirdRiverJobID int64 = 1003
	retriedAfterFailure, scheduled, err := translations.SchedulePending(ctx, params, func(context.Context, pgx.Tx, repository.TranslationScheduleCommand) (int64, error) {
		return thirdRiverJobID, nil
	})
	if err != nil || !scheduled || retriedAfterFailure.AttemptGeneration != retried.AttemptGeneration+1 ||
		retriedAfterFailure.CurrentRiverJobID == nil || *retriedAfterFailure.CurrentRiverJobID != thirdRiverJobID {
		t.Fatalf("failed retry SchedulePending() = %+v, %v, %v", retriedAfterFailure, scheduled, err)
	}
}

func TestTranslationScheduleCommitsAndRollsBackWithRiverJobAtomically(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	ctx := t.Context()
	linkID := mustCreateDoneLink(
		t,
		links,
		ctx,
		"https://example.com/translation-atomic",
		"translation atomicity",
		"example.com",
	)
	paramsFor := func(source string) repository.UpsertTranslationParams {
		return repository.UpsertTranslationParams{
			LinkID:         linkID,
			Scope:          model.TranslationScopeSelection,
			BlockKey:       "summary",
			StartOffset:    0,
			EndOffset:      len(source),
			SourceText:     source,
			SourceFormat:   model.TranslationFormatPlain,
			TargetLanguage: model.TranslationTargetChinese,
			SourceHash:     fmt.Sprintf("%x", sha256.Sum256([]byte(source))),
		}
	}

	committed, scheduled, err := translations.SchedulePending(ctx, paramsFor("committed source"), queue.EnqueueTranslationTx)
	if err != nil || !scheduled || committed == nil {
		t.Fatalf("SchedulePending(committed) = %+v, %v, %v", committed, scheduled, err)
	}
	var translationRows, riverRows int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM link_translations WHERE id=$1`, committed.ID).Scan(&translationRows); err != nil {
		t.Fatalf("count committed translation: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM river_job WHERE kind='translate_link_v2' AND args->>'translation_id'=$1`, committed.ID.String()).Scan(&riverRows); err != nil {
		t.Fatalf("count committed River translation: %v", err)
	}
	if translationRows != 1 || riverRows != 1 {
		t.Fatalf("committed counts translation=%d River=%d, want 1/1", translationRows, riverRows)
	}
	var committedRiverJobID int64
	if err := pool.QueryRow(t.Context(), `SELECT id FROM river_job
		WHERE kind='translate_link_v2' AND args->>'translation_id'=$1`, committed.ID.String()).Scan(&committedRiverJobID); err != nil {
		t.Fatalf("read committed River translation: %v", err)
	}
	if committed.CurrentRiverJobID == nil || *committed.CurrentRiverJobID != committedRiverJobID {
		t.Fatalf("committed current River job = %v, want %d", committed.CurrentRiverJobID, committedRiverJobID)
	}

	rollbackErr := errors.New("abort after River insert")
	var rolledBackID uuid.UUID
	rolledBackParams := paramsFor("rolled back source")
	item, scheduled, err := translations.SchedulePending(ctx, rolledBackParams, func(hookCtx context.Context, tx pgx.Tx, command repository.TranslationScheduleCommand) (int64, error) {
		rolledBackID = command.Seed.TranslationID
		jobID, enqueueErr := queue.EnqueueTranslationTx(hookCtx, tx, command)
		if enqueueErr != nil {
			return 0, enqueueErr
		}
		return jobID, rollbackErr
	})
	if !errors.Is(err, rollbackErr) || item != nil || scheduled || rolledBackID == uuid.Nil {
		t.Fatalf("SchedulePending(rollback) = %+v, %v, %v, id=%s", item, scheduled, err, rolledBackID)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM link_translations WHERE id=$1`, rolledBackID).Scan(&translationRows); err != nil {
		t.Fatalf("count rolled-back translation: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM river_job WHERE kind='translate_link_v2' AND args->>'translation_id'=$1`, rolledBackID.String()).Scan(&riverRows); err != nil {
		t.Fatalf("count rolled-back River translation: %v", err)
	}
	if translationRows != 0 || riverRows != 0 {
		t.Fatalf("rolled-back counts translation=%d River=%d, want 0/0", translationRows, riverRows)
	}
}

func TestTranslationStalledRescheduleHookFailureRollsBackOldCancellationAndNewJob(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	ctx := t.Context()
	linkID := mustCreateDoneLink(t, links, ctx,
		"https://example.com/translation-reschedule-hook-rollback",
		"translation reschedule hook rollback", "example.com")
	params := translationSelectionParams(linkID, "reschedule hook rollback")
	params.StallAfter = time.Hour

	initial, scheduled, err := translations.SchedulePending(ctx, params, queue.EnqueueTranslationTx)
	if err != nil || !scheduled || initial == nil || initial.CurrentRiverJobID == nil {
		t.Fatalf("initial SchedulePending() = %+v, %v, %v", initial, scheduled, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE link_translations
		SET status = 'processing', updated_at = NOW() - INTERVAL '3 hours'
		WHERE id = $1`, initial.ID); err != nil {
		t.Fatalf("mark initial attempt stalled: %v", err)
	}

	hookErr := errors.New("abort after replacement River insert")
	var replacementRiverJobID int64
	item, scheduled, err := translations.SchedulePending(ctx, params,
		func(hookCtx context.Context, tx pgx.Tx, command repository.TranslationScheduleCommand) (int64, error) {
			insertedID, enqueueErr := queue.EnqueueTranslationTx(hookCtx, tx, command)
			if enqueueErr != nil {
				return 0, enqueueErr
			}
			replacementRiverJobID = insertedID
			return insertedID, hookErr
		})
	if !errors.Is(err, hookErr) || item != nil || scheduled || replacementRiverJobID <= 0 {
		t.Fatalf("reschedule SchedulePending() = %+v, %v, %v replacement=%d",
			item, scheduled, err, replacementRiverJobID)
	}

	retained, err := translations.GetByID(ctx, initial.ID)
	if err != nil || retained == nil || retained.Status != model.TranslationStatusProcessing ||
		retained.AttemptGeneration != initial.AttemptGeneration || retained.CurrentRiverJobID == nil ||
		*retained.CurrentRiverJobID != *initial.CurrentRiverJobID {
		t.Fatalf("retained initial attempt = %+v, %v; initial=%+v", retained, err, initial)
	}
	var oldState string
	if err := pool.QueryRow(ctx, `SELECT state FROM river_job WHERE id = $1`,
		*initial.CurrentRiverJobID).Scan(&oldState); err != nil {
		t.Fatalf("read retained River attempt: %v", err)
	}
	if oldState != "available" {
		t.Fatalf("retained River attempt state = %q, want available", oldState)
	}
	var replacementRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE id = $1`,
		replacementRiverJobID).Scan(&replacementRows); err != nil {
		t.Fatalf("count rolled-back replacement River job: %v", err)
	}
	if replacementRows != 0 {
		t.Fatalf("rolled-back replacement River rows = %d, want 0", replacementRows)
	}
}

func TestTranslationScheduleReverseBindFailureRollsBackInsertedRiverJob(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	ctx := t.Context()
	linkID := mustCreateDoneLink(t, links, ctx,
		"https://example.com/translation-reverse-bind", "translation reverse bind", "example.com")
	params := translationSelectionParams(linkID, "reverse bind failure")
	params.StallAfter = time.Hour
	initial, scheduled, err := translations.SchedulePending(ctx, params, queue.EnqueueTranslationTx)
	if err != nil || !scheduled || initial == nil || initial.CurrentRiverJobID == nil {
		t.Fatalf("initial SchedulePending() = %+v, %v, %v", initial, scheduled, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE link_translations
		SET status = 'processing', updated_at = NOW() - INTERVAL '3 hours'
		WHERE id = $1`, initial.ID); err != nil {
		t.Fatalf("mark initial attempt stalled: %v", err)
	}

	var translationID uuid.UUID
	var riverJobID int64
	item, scheduled, err := translations.SchedulePending(ctx, params,
		func(hookCtx context.Context, tx pgx.Tx, command repository.TranslationScheduleCommand) (int64, error) {
			translationID = command.Seed.TranslationID
			insertedID, enqueueErr := queue.EnqueueTranslationTx(hookCtx, tx, command)
			if enqueueErr != nil {
				return 0, enqueueErr
			}
			riverJobID = insertedID
			if _, updateErr := tx.Exec(hookCtx, `UPDATE link_translations
				SET status = 'processing' WHERE id = $1`, command.Seed.TranslationID); updateErr != nil {
				return 0, updateErr
			}
			return insertedID, nil
		})
	if err == nil || item != nil || scheduled || translationID == uuid.Nil || riverJobID <= 0 {
		t.Fatalf("SchedulePending() = %+v, %v, %v translation=%s river=%d",
			item, scheduled, err, translationID, riverJobID)
	}
	retained, err := translations.GetByID(ctx, initial.ID)
	if err != nil || retained == nil || retained.Status != model.TranslationStatusProcessing ||
		retained.AttemptGeneration != initial.AttemptGeneration || retained.CurrentRiverJobID == nil ||
		*retained.CurrentRiverJobID != *initial.CurrentRiverJobID {
		t.Fatalf("retained initial attempt = %+v, %v; initial=%+v", retained, err, initial)
	}
	var riverRows int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM river_job WHERE id = $1`, riverJobID).Scan(&riverRows); err != nil {
		t.Fatalf("count River rows: %v", err)
	}
	if riverRows != 0 {
		t.Fatalf("rolled-back replacement River rows = %d, want 0", riverRows)
	}
	var oldState string
	if err := pool.QueryRow(ctx, `SELECT state FROM river_job WHERE id = $1`,
		*initial.CurrentRiverJobID).Scan(&oldState); err != nil {
		t.Fatalf("read retained River attempt: %v", err)
	}
	if oldState != "available" {
		t.Fatalf("retained River attempt state = %q, want available", oldState)
	}
}

func TestTranslationRetryInsertFailureRollsBackGenerationAndStatus(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	ctx := t.Context()
	linkID := mustCreateDoneLink(t, links, ctx,
		"https://example.com/translation-retry-rollback", "translation retry rollback", "example.com")
	params := translationSelectionParams(linkID, "retry insert rollback")
	const firstRiverJobID int64 = 2001
	initial, scheduled, err := translations.SchedulePending(ctx, params,
		func(context.Context, pgx.Tx, repository.TranslationScheduleCommand) (int64, error) {
			return firstRiverJobID, nil
		})
	if err != nil || !scheduled {
		t.Fatalf("initial SchedulePending() = %+v, %v, %v", initial, scheduled, err)
	}
	initialAttempt := model.TranslationAttempt{
		TranslationID:     initial.ID,
		AttemptGeneration: initial.AttemptGeneration, RiverJobID: firstRiverJobID,
		SourceHash: initial.SourceHash, SourceContentRevision: initial.SourceContentRevision,
	}
	if applied, err := translations.Fail(ctx, initialAttempt, "initial failed"); err != nil || !applied {
		t.Fatalf("Fail(initial) = %v, %v", applied, err)
	}

	insertErr := errors.New("injected River insert failure")
	item, scheduled, err := translations.SchedulePending(ctx, params,
		func(context.Context, pgx.Tx, repository.TranslationScheduleCommand) (int64, error) {
			return 0, insertErr
		})
	if !errors.Is(err, insertErr) || item != nil || scheduled {
		t.Fatalf("retry SchedulePending() = %+v, %v, %v", item, scheduled, err)
	}
	retained, err := translations.GetByID(ctx, initial.ID)
	if err != nil || retained == nil || retained.Status != model.TranslationStatusFailed ||
		retained.AttemptGeneration != initial.AttemptGeneration || retained.CurrentRiverJobID != nil {
		t.Fatalf("retained failed attempt = %+v, %v", retained, err)
	}
}

func TestTranslationConcurrentForceCreatesOneCurrentAttemptWithoutOrphan(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	ctx := t.Context()
	linkID := mustCreateDoneLink(t, links, ctx,
		"https://example.com/translation-concurrent-force", "translation concurrent force", "example.com")
	params := translationSelectionParams(linkID, "concurrent force")

	initial, scheduled, err := translations.SchedulePending(ctx, params, queue.EnqueueTranslationTx)
	if err != nil || !scheduled || initial.CurrentRiverJobID == nil {
		t.Fatalf("initial SchedulePending() = %+v, %v, %v", initial, scheduled, err)
	}
	initialAttempt := model.TranslationAttempt{
		TranslationID:     initial.ID,
		AttemptGeneration: initial.AttemptGeneration, RiverJobID: *initial.CurrentRiverJobID,
		SourceHash: initial.SourceHash, SourceContentRevision: initial.SourceContentRevision,
	}
	if _, err := translations.MarkProcessing(ctx, initialAttempt); err != nil {
		t.Fatalf("MarkProcessing(initial): %v", err)
	}
	if applied, err := translations.Complete(ctx, initialAttempt, "初始译文", "grok-4.3-fast"); err != nil || !applied {
		t.Fatalf("Complete(initial) = %v, %v", applied, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE river_job
		SET state = 'completed', finalized_at = NOW() WHERE id = $1`, initialAttempt.RiverJobID); err != nil {
		t.Fatalf("finalize initial River job: %v", err)
	}

	params.Force = true
	type scheduleResult struct {
		item      *model.LinkTranslation
		scheduled bool
		err       error
	}
	start := make(chan struct{})
	results := make(chan scheduleResult, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			item, won, scheduleErr := translations.SchedulePending(ctx, params, queue.EnqueueTranslationTx)
			results <- scheduleResult{item: item, scheduled: won, err: scheduleErr}
		}()
	}
	ready.Wait()
	close(start)

	winners := 0
	var current *model.LinkTranslation
	for range 2 {
		result := <-results
		if result.err != nil || result.item == nil {
			t.Fatalf("concurrent SchedulePending() = %+v, %v, %v", result.item, result.scheduled, result.err)
		}
		if result.scheduled {
			winners++
		}
		current = result.item
	}
	if winners != 1 {
		t.Fatalf("scheduled winners = %d, want 1", winners)
	}
	if current.AttemptGeneration != initial.AttemptGeneration+1 || current.CurrentRiverJobID == nil {
		t.Fatalf("current translation = %+v", current)
	}

	var activeCount int
	var onlyActiveID int64
	if err := pool.QueryRow(t.Context(), `SELECT count(*), min(id)
		FROM river_job
		WHERE kind = 'translate_link_v2'
		  AND args->>'translation_id' = $1
		  AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')`,
		initial.ID.String()).Scan(&activeCount, &onlyActiveID); err != nil {
		t.Fatalf("read active River attempts: %v", err)
	}
	if activeCount != 1 || onlyActiveID != *current.CurrentRiverJobID {
		t.Fatalf("active River count=%d id=%d current=%d", activeCount, onlyActiveID, *current.CurrentRiverJobID)
	}

	if applied, err := translations.Complete(ctx, initialAttempt, "迟到成功", "old-model"); err != nil || applied {
		t.Fatalf("Complete(stale) = %v, %v; want false, nil", applied, err)
	}
	if applied, err := translations.Fail(ctx, initialAttempt, "迟到失败"); err != nil || applied {
		t.Fatalf("Fail(stale) = %v, %v; want false, nil", applied, err)
	}
	currentAttempt := model.TranslationAttempt{
		TranslationID:     current.ID,
		AttemptGeneration: current.AttemptGeneration, RiverJobID: *current.CurrentRiverJobID,
		SourceHash: current.SourceHash, SourceContentRevision: current.SourceContentRevision,
	}
	if processing, err := translations.MarkProcessing(ctx, currentAttempt); err != nil || processing == nil {
		t.Fatalf("MarkProcessing(current) = %+v, %v", processing, err)
	}
	if applied, err := translations.Complete(ctx, currentAttempt, "当前译文", "grok-4.3-fast"); err != nil || !applied {
		t.Fatalf("Complete(current) = %v, %v", applied, err)
	}
}

func TestTranslationConcurrentFailedRetryCreatesOneCurrentAttemptWithoutOrphan(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	ctx := t.Context()
	linkID := mustCreateDoneLink(t, links, ctx,
		"https://example.com/translation-concurrent-failed-retry", "translation concurrent failed retry", "example.com")
	params := translationSelectionParams(linkID, "concurrent failed retry")

	initial, scheduled, err := translations.SchedulePending(ctx, params, queue.EnqueueTranslationTx)
	if err != nil || !scheduled || initial.CurrentRiverJobID == nil {
		t.Fatalf("initial SchedulePending() = %+v, %v, %v", initial, scheduled, err)
	}
	initialAttempt := model.TranslationAttempt{
		TranslationID:     initial.ID,
		AttemptGeneration: initial.AttemptGeneration, RiverJobID: *initial.CurrentRiverJobID,
		SourceHash: initial.SourceHash, SourceContentRevision: initial.SourceContentRevision,
	}
	if applied, err := translations.Fail(ctx, initialAttempt, "initial failed"); err != nil || !applied {
		t.Fatalf("Fail(initial) = %v, %v", applied, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE river_job
		SET state = 'discarded', finalized_at = NOW() WHERE id = $1`, initialAttempt.RiverJobID); err != nil {
		t.Fatalf("finalize initial River job: %v", err)
	}

	type scheduleResult struct {
		item      *model.LinkTranslation
		scheduled bool
		err       error
	}
	start := make(chan struct{})
	results := make(chan scheduleResult, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			item, won, scheduleErr := translations.SchedulePending(ctx, params, queue.EnqueueTranslationTx)
			results <- scheduleResult{item: item, scheduled: won, err: scheduleErr}
		}()
	}
	ready.Wait()
	close(start)

	winners := 0
	var current *model.LinkTranslation
	for range 2 {
		result := <-results
		if result.err != nil || result.item == nil {
			t.Fatalf("concurrent failed retry SchedulePending() = %+v, %v, %v", result.item, result.scheduled, result.err)
		}
		if result.scheduled {
			winners++
		}
		current = result.item
	}
	if winners != 1 {
		t.Fatalf("scheduled failed-retry winners = %d, want 1", winners)
	}
	if current.AttemptGeneration != initial.AttemptGeneration+1 || current.CurrentRiverJobID == nil {
		t.Fatalf("current failed-retry translation = %+v", current)
	}

	var activeCount int
	var onlyActiveID int64
	if err := pool.QueryRow(t.Context(), `SELECT count(*), min(id)
		FROM river_job
		WHERE kind = 'translate_link_v2'
		  AND args->>'translation_id' = $1
		  AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')`,
		initial.ID.String()).Scan(&activeCount, &onlyActiveID); err != nil {
		t.Fatalf("read active failed-retry River attempts: %v", err)
	}
	if activeCount != 1 || onlyActiveID != *current.CurrentRiverJobID {
		t.Fatalf("active failed-retry River count=%d id=%d current=%d", activeCount, onlyActiveID, *current.CurrentRiverJobID)
	}
}

func TestTranslationProjectionFailureRetainsCurrentAttemptIdentity(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	ctx := t.Context()
	linkID := mustCreateDoneLink(t, links, ctx,
		"https://example.com/translation-projection-failure", "translation projection failure", "example.com")
	item, scheduled, err := translations.SchedulePending(
		ctx,
		translationSelectionParams(linkID, "retain current on projection error"),
		queue.EnqueueTranslationTx,
	)
	if err != nil || !scheduled || item.CurrentRiverJobID == nil {
		t.Fatalf("SchedulePending() = %+v, %v, %v", item, scheduled, err)
	}
	attempt := model.TranslationAttempt{
		TranslationID:     item.ID,
		AttemptGeneration: item.AttemptGeneration, RiverJobID: *item.CurrentRiverJobID,
		SourceHash: item.SourceHash, SourceContentRevision: item.SourceContentRevision,
	}
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if applied, err := translations.Fail(cancelledCtx, attempt, "should not commit"); err == nil || applied {
		t.Fatalf("Fail(cancelled context) = %v, %v; want false, error", applied, err)
	}

	retained, err := translations.GetByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if retained == nil || retained.Status != model.TranslationStatusPending ||
		retained.AttemptGeneration != attempt.AttemptGeneration || retained.CurrentRiverJobID == nil ||
		*retained.CurrentRiverJobID != attempt.RiverJobID {
		t.Fatalf("retained translation = %+v", retained)
	}
}

func TestTranslationProcessingRequiresWholeCurrentAttemptIdentity(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	ctx := t.Context()
	linkID := mustCreateDoneLink(t, links, ctx,
		"https://example.com/translation-whole-identity", "translation whole identity", "example.com")
	item, scheduled, err := translations.SchedulePending(
		ctx,
		translationSelectionParams(linkID, "whole attempt identity"),
		queue.EnqueueTranslationTx,
	)
	if err != nil || !scheduled || item.CurrentRiverJobID == nil {
		t.Fatalf("SchedulePending() = %+v, %v, %v", item, scheduled, err)
	}
	current := model.TranslationAttempt{
		TranslationID:     item.ID,
		AttemptGeneration: item.AttemptGeneration, RiverJobID: *item.CurrentRiverJobID,
		SourceHash: item.SourceHash, SourceContentRevision: item.SourceContentRevision,
	}
	wrongTranslation := current
	wrongTranslation.TranslationID = uuid.New()
	wrongGeneration := current
	wrongGeneration.AttemptGeneration++
	wrongRiverJob := current
	wrongRiverJob.RiverJobID++
	wrongSourceHash := current
	wrongSourceHash.SourceHash = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	wrongSourceRevision := current
	invalidSourceRevision := int64(1)
	wrongSourceRevision.SourceContentRevision = &invalidSourceRevision
	for name, attempt := range map[string]model.TranslationAttempt{
		"translation":     wrongTranslation,
		"generation":      wrongGeneration,
		"river job":       wrongRiverJob,
		"source hash":     wrongSourceHash,
		"source revision": wrongSourceRevision,
	} {
		t.Run(name, func(t *testing.T) {
			processing, err := translations.MarkProcessing(ctx, attempt)
			if err != nil || processing != nil {
				t.Fatalf("MarkProcessing(%s) = %+v, %v; want nil, nil", name, processing, err)
			}
		})
	}
	retained, err := translations.GetByID(ctx, item.ID)
	if err != nil || retained == nil || retained.Status != model.TranslationStatusPending {
		t.Fatalf("retained pending = %+v, %v", retained, err)
	}
	processing, err := translations.MarkProcessing(ctx, current)
	if err != nil || processing == nil || processing.Status != model.TranslationStatusProcessing {
		t.Fatalf("MarkProcessing(current) = %+v, %v", processing, err)
	}
	for name, attempt := range map[string]model.TranslationAttempt{
		"source hash":     wrongSourceHash,
		"source revision": wrongSourceRevision,
	} {
		t.Run("terminal "+name, func(t *testing.T) {
			if applied, err := translations.Complete(ctx, attempt, "must not apply", "wrong-source"); err != nil || applied {
				t.Fatalf("Complete(%s) = %v, %v; want false, nil", name, applied, err)
			}
			if applied, err := translations.Fail(ctx, attempt, "must not apply"); err != nil || applied {
				t.Fatalf("Fail(%s) = %v, %v; want false, nil", name, applied, err)
			}
		})
	}
	retained, err = translations.GetByID(ctx, item.ID)
	if err != nil || retained == nil || retained.Status != model.TranslationStatusProcessing ||
		retained.CurrentRiverJobID == nil || *retained.CurrentRiverJobID != current.RiverJobID {
		t.Fatalf("retained processing after source mismatch = %+v, %v", retained, err)
	}
	if applied, err := translations.Complete(ctx, current, "exact source", "grok-4.3-fast"); err != nil || !applied {
		t.Fatalf("Complete(current) = %v, %v; want true, nil", applied, err)
	}
}

func TestQueuedTranslationCancellationRetainsNonterminalCurrentIdentity(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	ctx := t.Context()
	linkID := mustCreateDoneLink(t, links, ctx,
		"https://example.com/translation-queued-cancel", "translation queued cancel", "example.com")
	item, scheduled, err := translations.SchedulePending(
		ctx,
		translationSelectionParams(linkID, "queued cancellation"),
		queue.EnqueueTranslationTx,
	)
	if err != nil || !scheduled || item.CurrentRiverJobID == nil {
		t.Fatalf("SchedulePending() = %+v, %v, %v", item, scheduled, err)
	}

	riverClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatalf("river.NewClient() error = %v", err)
	}
	cancelled, err := riverClient.JobCancel(t.Context(), *item.CurrentRiverJobID)
	if err != nil {
		t.Fatalf("JobCancel() error = %v", err)
	}
	if cancelled.State != "cancelled" {
		t.Fatalf("River state = %q, want cancelled", cancelled.State)
	}

	retained, err := translations.GetByID(ctx, item.ID)
	if err != nil || retained == nil || retained.Status != model.TranslationStatusPending ||
		retained.AttemptGeneration != item.AttemptGeneration || retained.CurrentRiverJobID == nil ||
		*retained.CurrentRiverJobID != *item.CurrentRiverJobID {
		t.Fatalf("retained translation = %+v, %v", retained, err)
	}
}

func translationSelectionParams(linkID uuid.UUID, source string) repository.UpsertTranslationParams {
	return repository.UpsertTranslationParams{
		LinkID: linkID, Scope: model.TranslationScopeSelection,
		BlockKey: "summary", StartOffset: 0, EndOffset: len(source),
		SourceText: source, SourceFormat: model.TranslationFormatPlain,
		TargetLanguage: model.TranslationTargetChinese,
		SourceHash:     fmt.Sprintf("%x", sha256.Sum256([]byte(source))),
	}
}
