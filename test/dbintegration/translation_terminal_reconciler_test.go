package dbintegration

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service/linktranslation"
	"webtag/internal/worker"
)

func TestTranslationTerminalReconcilerProjectsTerminalStatesAcrossEqualTimestampPages(t *testing.T) {
	pool := StartPostgres(t)
	finalizedAt := time.Now().UTC().Add(-time.Minute)
	wantCodes := map[string]string{
		"cancelled": string(worker.TranslationTerminalJobCancelled),
		"discarded": string(worker.TranslationTerminalJobDiscarded),
		"completed": string(worker.TranslationTerminalProjectionMissing),
	}
	translations := make(map[uuid.UUID]string, len(wantCodes))
	for state, code := range wantCodes {
		translationID := uuid.New()
		sourceHash := terminalSourceHash(state)
		jobID := insertTerminalRiverJob(t, pool, linktranslation.JobArgs{
			TranslationID: translationID, AttemptGeneration: 1, SourceHash: sourceHash,
		}, model.TranslationJobKindV2, state, finalizedAt)
		insertTerminalTranslation(t, pool, translationID, state, sourceHash, 1, &jobID, time.Now().UTC())
		translations[translationID] = code
	}

	reconciler := newPostgresTranslationTerminalReconciler(t, pool, 1, time.Hour)
	result, err := reconciler.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce(): %v", err)
	}
	if result.Scanned != 3 || result.Reconciled != 3 || result.Failed != 0 || result.Invalid != 0 {
		t.Fatalf("RunOnce() result = %+v, want three reconciled terminal rows", result)
	}
	for translationID, wantCode := range translations {
		assertTerminalTranslationState(t, pool, translationID, model.TranslationStatusFailed, wantCode)
	}
}

func TestTranslationTerminalReconcilerConcurrentReplicasApplyOnce(t *testing.T) {
	pool := StartPostgres(t)
	translationID := uuid.New()
	sourceHash := terminalSourceHash("concurrent")
	jobID := insertTerminalRiverJob(t, pool, linktranslation.JobArgs{
		TranslationID: translationID, AttemptGeneration: 1, SourceHash: sourceHash,
	}, model.TranslationJobKindV2, "cancelled", time.Now().UTC().Add(-time.Minute))
	insertTerminalTranslation(t, pool, translationID, "concurrent", sourceHash, 1, &jobID, time.Now().UTC())

	results := make(chan worker.TranslationTerminalReconcileResult, 2)
	errors := make(chan error, 2)
	start := make(chan struct{})
	var replicas sync.WaitGroup
	for range 2 {
		reconciler := newPostgresTranslationTerminalReconciler(t, pool, 10, time.Hour)
		replicas.Add(1)
		go func() {
			defer replicas.Done()
			<-start
			result, err := reconciler.RunOnce(t.Context())
			results <- result
			errors <- err
		}()
	}
	close(start)
	replicas.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("replica RunOnce(): %v", err)
		}
	}
	reconciled := 0
	for result := range results {
		reconciled += result.Reconciled
	}
	if reconciled != 1 {
		t.Fatalf("replica reconciled total = %d, want exactly 1", reconciled)
	}
	assertTerminalTranslationState(t, pool, translationID, model.TranslationStatusFailed, string(worker.TranslationTerminalJobCancelled))
}

func TestTranslationTerminalReconcilerResolvesGenerationZeroLegacyJob(t *testing.T) {
	pool := StartPostgres(t)
	translationID := uuid.New()
	sourceHash := terminalSourceHash("legacy-generation-zero")
	jobID := insertTerminalRiverJob(t, pool, linktranslation.LegacyJobArgs{
		TranslationID: translationID,
	}, model.TranslationJobKindLegacy, "discarded", time.Now().UTC().Add(-time.Minute))
	insertTerminalTranslation(t, pool, translationID, "legacy-generation-zero", sourceHash, 0, &jobID, time.Now().UTC())

	reconciler := newPostgresTranslationTerminalReconciler(t, pool, 10, time.Hour)
	result, err := reconciler.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce(): %v", err)
	}
	if result.Reconciled != 1 || result.Invalid != 0 {
		t.Fatalf("RunOnce() result = %+v, want one resolved legacy attempt", result)
	}
	assertTerminalTranslationState(t, pool, translationID, model.TranslationStatusFailed, string(worker.TranslationTerminalJobDiscarded))
}

func TestTranslationTerminalReconcilerRecoversMissingRiverRow(t *testing.T) {
	pool := StartPostgres(t)
	translationID := uuid.New()
	missingJobID := int64(9_100_000_001)
	sourceHash := terminalSourceHash("missing-river-row")
	insertTerminalTranslation(
		t, pool, translationID, "missing-river-row", sourceHash, 1, &missingJobID,
		time.Now().UTC().Add(-2*time.Hour),
	)

	reconciler := newPostgresTranslationTerminalReconciler(t, pool, 10, time.Hour)
	result, err := reconciler.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce(): %v", err)
	}
	if result.Reconciled != 1 || result.Missing != 1 || result.Failed != 0 {
		t.Fatalf("RunOnce() result = %+v, want one missing-job recovery", result)
	}
	assertTerminalTranslationState(t, pool, translationID, model.TranslationStatusFailed, string(worker.TranslationTerminalJobMissing))
}

func TestTranslationTerminalReconcilerPreservesAttemptWithActiveReplacement(t *testing.T) {
	pool := StartPostgres(t)
	translationID := uuid.New()
	missingJobID := int64(9_100_000_002)
	sourceHash := terminalSourceHash("active-replacement")
	insertTerminalTranslation(
		t, pool, translationID, "active-replacement", sourceHash, 1, &missingJobID,
		time.Now().UTC().Add(-2*time.Hour),
	)
	insertActiveRiverJob(t, pool, linktranslation.JobArgs{
		TranslationID: translationID, AttemptGeneration: 1, SourceHash: sourceHash,
	}, model.TranslationJobKindV2)

	reconciler := newPostgresTranslationTerminalReconciler(t, pool, 10, time.Hour)
	result, err := reconciler.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce(): %v", err)
	}
	if result.Reconciled != 0 || result.Missing != 0 || result.Failed != 0 {
		t.Fatalf("RunOnce() result = %+v, want active replacement skipped", result)
	}
	assertTerminalTranslationState(t, pool, translationID, model.TranslationStatusPending, "")
}

func TestTranslationTerminalReconcilerFindsEarlierTerminalCommittedLate(t *testing.T) {
	pool := StartPostgres(t)
	base := time.Now().UTC().Add(-2 * time.Minute)
	reconciler := newPostgresTranslationTerminalReconciler(t, pool, 10, time.Hour)
	lateID := uuid.New()
	lateHash := terminalSourceHash("late-commit")
	const lateJobID int64 = 9_200_000_001
	insertTerminalTranslation(t, pool, lateID, "late-commit", lateHash, 1, int64Pointer(lateJobID), base)

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin late transaction: %v", err)
	}
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	})
	insertTerminalRiverJobWithID(t, tx, lateJobID, linktranslation.JobArgs{
		TranslationID: lateID, AttemptGeneration: 1, SourceHash: lateHash,
	}, model.TranslationJobKindV2, "cancelled", base)

	visibleID := uuid.New()
	visibleHash := terminalSourceHash("visible-first")
	visibleJobID := insertTerminalRiverJob(t, pool, linktranslation.JobArgs{
		TranslationID: visibleID, AttemptGeneration: 1, SourceHash: visibleHash,
	}, model.TranslationJobKindV2, "cancelled", base.Add(time.Second))
	insertTerminalTranslation(t, pool, visibleID, "visible-first", visibleHash, 1, &visibleJobID, time.Now().UTC())

	first, err := reconciler.RunOnce(t.Context())
	if err != nil || first.Reconciled != 1 {
		t.Fatalf("first RunOnce() = %+v, %v", first, err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit earlier terminal: %v", err)
	}
	committed = true
	second, err := reconciler.RunOnce(t.Context())
	if err != nil || second.Reconciled != 1 {
		t.Fatalf("second RunOnce() = %+v, %v", second, err)
	}
	assertTerminalTranslationState(t, pool, lateID, model.TranslationStatusFailed, string(worker.TranslationTerminalJobCancelled))
}

func newPostgresTranslationTerminalReconciler(
	t *testing.T,
	pool *pgxpool.Pool,
	batchSize int,
	missingAfter time.Duration,
) *worker.TranslationTerminalReconciler {
	t.Helper()
	translations := repository.NewPGXTranslationRepository(pool)
	reconciler, err := worker.NewTranslationTerminalReconciler(worker.TranslationTerminalReconcilerOptions{
		Pool: pool, Projector: translations, LegacyAttempts: translations,
		Interval: time.Hour, BatchSize: batchSize, RoundTimeout: 10 * time.Second,
		MissingAfter: missingAfter,
	})
	if err != nil {
		t.Fatalf("NewTranslationTerminalReconciler(): %v", err)
	}
	return reconciler
}

type terminalFixtureDB interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func insertTerminalRiverJob(
	t *testing.T,
	db terminalFixtureDB,
	args any,
	kind string,
	state string,
	finalizedAt time.Time,
) int64 {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal terminal River args: %v", err)
	}
	var jobID int64
	if err := db.QueryRow(t.Context(), `INSERT INTO river_job (
		args, kind, max_attempts, state, finalized_at
	) VALUES ($1::jsonb, $2, 3, $3::river_job_state, $4) RETURNING id`,
		encoded, kind, state, finalizedAt).Scan(&jobID); err != nil {
		t.Fatalf("insert terminal River job: %v", err)
	}
	return jobID
}

func insertTerminalRiverJobWithID(
	t *testing.T,
	db terminalFixtureDB,
	jobID int64,
	args any,
	kind string,
	state string,
	finalizedAt time.Time,
) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal terminal River args: %v", err)
	}
	var insertedID int64
	if err := db.QueryRow(t.Context(), `INSERT INTO river_job (
		id, args, kind, max_attempts, state, finalized_at
	) VALUES ($1, $2::jsonb, $3, 3, $4::river_job_state, $5) RETURNING id`,
		jobID, encoded, kind, state, finalizedAt).Scan(&insertedID); err != nil {
		t.Fatalf("insert terminal River job %d: %v", jobID, err)
	}
}

func insertActiveRiverJob(t *testing.T, db terminalFixtureDB, args any, kind string) int64 {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal active River args: %v", err)
	}
	var jobID int64
	if err := db.QueryRow(t.Context(), `INSERT INTO river_job (
		args, kind, max_attempts, state
	) VALUES ($1::jsonb, $2, 3, 'available') RETURNING id`, encoded, kind).Scan(&jobID); err != nil {
		t.Fatalf("insert active River job: %v", err)
	}
	return jobID
}

func insertTerminalTranslation(
	t *testing.T,
	db terminalFixtureDB,
	translationID uuid.UUID,
	suffix string,
	sourceHash string,
	generation int64,
	currentJobID *int64,
	updatedAt time.Time,
) {
	t.Helper()
	linkID := uuid.New()
	rawURL := "https://terminal-reconcile.example/" + suffix + "/" + linkID.String()
	if err := db.QueryRow(t.Context(), `INSERT INTO links (
		id, url, source_key, status, first_collected_at
	) VALUES ($1, $2, $2, 'done', NOW()) RETURNING id`, linkID, rawURL).Scan(&linkID); err != nil {
		t.Fatalf("insert terminal fixture link: %v", err)
	}
	sourceText := "source-" + suffix
	var inserted uuid.UUID
	if err := db.QueryRow(t.Context(), `INSERT INTO link_translations (
		id, link_id, scope, block_key, start_offset, end_offset,
		source_text, source_format, target_language, source_hash, status,
		attempt_generation, current_river_job_id, updated_at
	) VALUES ($1, $2, 'selection', 'summary', 0, $3,
		$4, 'plain', 'zh-CN', $5, 'pending', $6, $7, $8) RETURNING id`,
		translationID, linkID, len(sourceText), sourceText, sourceHash,
		generation, currentJobID, updatedAt).Scan(&inserted); err != nil {
		t.Fatalf("insert terminal translation: %v", err)
	}
}

func assertTerminalTranslationState(
	t *testing.T,
	pool *pgxpool.Pool,
	translationID uuid.UUID,
	wantStatus model.TranslationStatus,
	wantError string,
) {
	t.Helper()
	var status string
	var errorMessage *string
	var currentJobID *int64
	if err := pool.QueryRow(t.Context(), `SELECT status,error_msg,current_river_job_id
		FROM link_translations WHERE id=$1`, translationID).Scan(&status, &errorMessage, &currentJobID); err != nil {
		t.Fatalf("read translation %s: %v", translationID, err)
	}
	if status != string(wantStatus) {
		t.Fatalf("translation %s status = %q, want %q", translationID, status, wantStatus)
	}
	if wantError == "" {
		if errorMessage != nil {
			t.Fatalf("translation %s error = %q, want NULL", translationID, *errorMessage)
		}
	} else if errorMessage == nil || *errorMessage != wantError {
		t.Fatalf("translation %s error = %v, want %q", translationID, errorMessage, wantError)
	}
	if wantStatus == model.TranslationStatusFailed && currentJobID != nil {
		t.Fatalf("translation %s current job = %d, want NULL", translationID, *currentJobID)
	}
}

func terminalSourceHash(seed string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(seed)))
}

func int64Pointer(value int64) *int64 { return &value }
