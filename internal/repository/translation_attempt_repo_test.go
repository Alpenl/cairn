package repository

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func translationAttemptRow(id, linkID uuid.UUID, status model.TranslationStatus, generation int64) *pgxmock.Rows {
	now := time.Now().UTC()
	return pgxmock.NewRows([]string{
		"id", "link_id", "scope", "block_key", "start_offset", "end_offset",
		"source_text", "translated_text", "source_format", "target_language", "source_hash",
		"source_content_revision", "status", "model", "error_msg", "attempt_generation", "created_at", "updated_at",
	}).AddRow(
		id, linkID, "selection", "summary", 3, 8,
		"hello", nil, "plain", "zh-CN", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		nil, string(status), nil, nil, generation, now, now,
	)
}

func TestTranslationRepositoryFencesAttemptWritesByGenerationAndSource(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXTranslationRepository(mock)
	linkID, translationID := uuid.New(), uuid.New()
	const generation int64 = 7
	const sourceHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	attempt := model.TranslationAttempt{
		TranslationID: translationID, AttemptGeneration: generation, RiverJobID: 731, SourceHash: sourceHash,
	}

	mock.ExpectQuery(`(?s)UPDATE link_translations.*attempt_generation = \$2.*source_hash = \$3.*source_content_revision IS NOT DISTINCT FROM \$4`).
		WithArgs(translationID, generation, sourceHash, (*int64)(nil)).
		WillReturnRows(translationAttemptRow(translationID, linkID, model.TranslationStatusProcessing, generation))
	item, err := repo.MarkProcessing(context.Background(), attempt)
	if err != nil || item == nil || item.Status != model.TranslationStatusProcessing {
		t.Fatalf("MarkProcessing() = %+v, %v", item, err)
	}

	mock.ExpectExec(`(?s)UPDATE link_translations.*attempt_generation = \$4.*source_hash = \$5.*source_content_revision IS NOT DISTINCT FROM \$6`).
		WithArgs("translated", "model", translationID, generation, sourceHash, (*int64)(nil)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	completed, err := repo.Complete(context.Background(), attempt, "translated", "model")
	if err != nil || !completed {
		t.Fatalf("Complete() = %v, %v", completed, err)
	}

	mock.ExpectExec(`(?s)UPDATE link_translations.*attempt_generation = \$3.*source_hash = \$4.*source_content_revision IS NOT DISTINCT FROM \$5`).
		WithArgs("failed", translationID, generation, sourceHash, (*int64)(nil)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	failed, err := repo.Fail(context.Background(), attempt, "failed")
	if err != nil || failed {
		t.Fatalf("Fail() = %v, %v; want stale write rejection", failed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
