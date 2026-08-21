package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func translationAttemptRow(
	id, linkID uuid.UUID,
	status model.TranslationStatus,
	generation int64,
	currentRiverJobID *int64,
) *pgxmock.Rows {
	now := time.Now().UTC()
	return pgxmock.NewRows([]string{
		"id", "link_id", "scope", "block_key", "start_offset", "end_offset",
		"source_text", "translated_text", "source_format", "target_language", "source_hash",
		"source_content_revision", "status", "model", "error_msg", "attempt_generation", "current_river_job_id", "created_at", "updated_at",
	}).AddRow(
		id, linkID, "selection", "summary", 3, 8,
		"hello", nil, "plain", "zh-CN", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		nil, string(status), nil, nil, generation, currentRiverJobID, now, now,
	)
}

func TestTranslationRepositorySchedulesAndBindsCurrentRiverAttemptAtomically(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXTranslationRepository(mock)
	linkID, translationID := uuid.New(), uuid.New()
	const generation int64 = 1
	const riverJobID int64 = 731
	ctx := context.Background()
	params := UpsertTranslationParams{
		LinkID:         linkID,
		Scope:          model.TranslationScopeSelection,
		BlockKey:       "summary",
		StartOffset:    3,
		EndOffset:      8,
		SourceText:     "hello",
		SourceFormat:   model.TranslationFormatPlain,
		TargetLanguage: model.TranslationTargetChinese,
		SourceHash:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM link_translations")).
		WithArgs(
			linkID, params.Scope, params.BlockKey, params.StartOffset,
			params.EndOffset, params.SourceHash, params.TargetLanguage,
		).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO link_translations")).
		WithArgs(
			linkID, params.Scope, params.BlockKey, params.StartOffset,
			params.EndOffset, params.SourceText, params.SourceFormat,
			params.TargetLanguage, params.SourceHash, params.SourceContentRevision,
		).
		WillReturnRows(translationAttemptRow(
			translationID, linkID, model.TranslationStatusPending, generation, nil,
		))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE link_translations")).
		WithArgs(riverJobID, translationID, generation).
		WillReturnRows(translationAttemptRow(
			translationID, linkID, model.TranslationStatusPending, generation, ptrTo(riverJobID),
		))
	mock.ExpectCommit()

	var hookCommand TranslationScheduleCommand
	created, scheduled, err := repo.SchedulePending(
		ctx,
		params,
		func(_ context.Context, _ pgx.Tx, command TranslationScheduleCommand) (int64, error) {
			hookCommand = command
			return riverJobID, nil
		},
	)
	if err != nil {
		t.Fatalf("SchedulePending() error = %v", err)
	}
	if !scheduled {
		t.Fatal("SchedulePending() scheduled = false, want true")
	}
	if hookCommand.Seed.TranslationID != translationID ||
		hookCommand.Seed.AttemptGeneration != generation ||
		hookCommand.Seed.SourceHash != params.SourceHash || hookCommand.Seed.SourceContentRevision != nil ||
		hookCommand.Previous != nil {
		t.Fatalf("schedule hook command = %+v", hookCommand)
	}
	if created.AttemptGeneration != generation || created.CurrentRiverJobID == nil ||
		*created.CurrentRiverJobID != riverJobID {
		t.Fatalf("scheduled translation = %+v", created)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationRepositoryMarksProcessingOnlyForWholeCurrentAttempt(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXTranslationRepository(mock)
	linkID, translationID := uuid.New(), uuid.New()
	const generation int64 = 7
	const riverJobID int64 = 731
	const sourceHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	attempt := model.TranslationAttempt{
		TranslationID:     translationID,
		AttemptGeneration: generation,
		RiverJobID:        riverJobID,
		SourceHash:        sourceHash,
	}
	ctx := context.Background()

	mock.ExpectQuery(`(?s)UPDATE link_translations.*source_hash = \$4.*source_content_revision IS NOT DISTINCT FROM \$5`).
		WithArgs(translationID, riverJobID, generation, sourceHash, (*int64)(nil)).
		WillReturnRows(translationAttemptRow(
			translationID, linkID, model.TranslationStatusProcessing, generation, ptrTo(riverJobID),
		))

	item, err := repo.MarkProcessing(ctx, attempt)
	if err != nil {
		t.Fatalf("MarkProcessing() error = %v", err)
	}
	if item == nil || item.Status != model.TranslationStatusProcessing ||
		item.CurrentRiverJobID == nil || *item.CurrentRiverJobID != riverJobID {
		t.Fatalf("MarkProcessing() = %+v", item)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationRepositoryCompletesAndClearsOnlyWholeCurrentAttempt(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXTranslationRepository(mock)
	translationID := uuid.New()
	const sourceHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	attempt := model.TranslationAttempt{
		TranslationID:     translationID,
		AttemptGeneration: 7, RiverJobID: 731,
		SourceHash: sourceHash,
	}
	ctx := context.Background()

	mock.ExpectExec(`(?s)UPDATE link_translations.*current_river_job_id = NULL.*current_river_job_id = \$4.*attempt_generation = \$5.*source_hash = \$6.*source_content_revision IS NOT DISTINCT FROM \$7`).
		WithArgs("你好", "grok-4.3-fast", translationID, int64(731), int64(7), sourceHash, (*int64)(nil)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	completed, err := repo.Complete(ctx, attempt, "你好", "grok-4.3-fast")
	if err != nil || !completed {
		t.Fatalf("Complete() = %v, %v; want true, nil", completed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationRepositoryFailsAndClearsOnlyWholeCurrentAttempt(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXTranslationRepository(mock)
	translationID := uuid.New()
	const sourceHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	attempt := model.TranslationAttempt{
		TranslationID:     translationID,
		AttemptGeneration: 7, RiverJobID: 731,
		SourceHash: sourceHash,
	}
	ctx := context.Background()

	mock.ExpectExec(`(?s)UPDATE link_translations.*current_river_job_id = NULL.*current_river_job_id = \$3.*attempt_generation = \$4.*source_hash = \$5.*source_content_revision IS NOT DISTINCT FROM \$6`).
		WithArgs("翻译任务已取消，请重试", translationID, int64(731), int64(7), sourceHash, (*int64)(nil)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	failed, err := repo.Fail(ctx, attempt, "翻译任务已取消，请重试")
	if err != nil || !failed {
		t.Fatalf("Fail() = %v, %v; want true, nil", failed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func ptrTo[T any](value T) *T { return &value }
