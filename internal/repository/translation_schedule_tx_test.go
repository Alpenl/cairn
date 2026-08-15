package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func translationRevisionRow(
	id, linkID uuid.UUID,
	status model.TranslationStatus,
	revision *int64,
	generation int64,
) *pgxmock.Rows {
	now := time.Now().UTC()
	return pgxmock.NewRows([]string{
		"id", "link_id", "scope", "block_key", "start_offset", "end_offset",
		"source_text", "translated_text", "source_format", "target_language", "source_hash",
		"source_content_revision", "status", "model", "error_msg", "attempt_generation",
		"current_river_job_id", "created_at", "updated_at",
	}).AddRow(
		id, linkID, "selection", "content", 2, 7,
		"hello", nil, "plain", "zh-CN", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		revision, string(status), nil, nil, generation, nil, now, now,
	)
}

func TestLockTranslationSourceTxLocksCanonicalLinkProjection(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID := uuid.New()
	document := "# Saved"
	summary := "**Summary**"
	mock.ExpectQuery(regexp.QuoteMeta("FROM links WHERE id=$1 FOR UPDATE")).
		WithArgs(linkID).
		WillReturnRows(pgxmock.NewRows([]string{
			"status", "library_kind", "content_revision", "content_present", "content",
			"document_present", "content_document", "content_format", "summary_present", "summary",
		}).AddRow("done", "reading", int64(7), true, "Saved", true, document, "markdown", true, summary))

	repo := NewPGXTranslationRepository(mock)
	got, err := repo.LockTranslationSourceTx(context.Background(), mock, linkID)
	if err != nil {
		t.Fatalf("LockTranslationSourceTx() error = %v", err)
	}
	if got == nil || got.ContentRevision != 7 || got.Content == nil || *got.Content != "Saved" ||
		got.ContentDocument == nil || *got.ContentDocument != document || got.Summary == nil || *got.Summary != summary {
		t.Fatalf("LockTranslationSourceTx() = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestFindTranslationIdentityTxUsesSavedRevisionIdentity(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID, translationID := uuid.New(), uuid.New()
	revision := int64(7)
	params := UpsertTranslationParams{
		LinkID: linkID, Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 2, EndOffset: 7, SourceText: "hello",
		SourceFormat: model.TranslationFormatPlain, TargetLanguage: model.TranslationTargetChinese,
		SourceHash:            "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourceContentRevision: &revision,
	}
	mock.ExpectQuery(regexp.QuoteMeta("source_content_revision = $6 AND target_language = $7 FOR UPDATE")).
		WithArgs(linkID, params.Scope, params.BlockKey, 2, 7, revision, params.TargetLanguage).
		WillReturnRows(translationRevisionRow(translationID, linkID, model.TranslationStatusDone, &revision, 3))

	repo := NewPGXTranslationRepository(mock)
	got, err := repo.FindTranslationIdentityTx(context.Background(), mock, params)
	if err != nil {
		t.Fatalf("FindTranslationIdentityTx() error = %v", err)
	}
	if got == nil || got.ID != translationID || got.SourceContentRevision == nil || *got.SourceContentRevision != revision {
		t.Fatalf("FindTranslationIdentityTx() = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestInsertPendingTranslationTxDoesNotDependOnLegacyConflictTarget(t *testing.T) {
	t.Parallel()

	if regexp.MustCompile(`ON CONFLICT\s*\(`).MatchString(insertPendingTranslationTxSQL) {
		t.Fatalf("insertPendingTranslationTxSQL names a conflict target: %s", insertPendingTranslationTxSQL)
	}
	if !regexp.MustCompile(`ON CONFLICT\s+DO NOTHING`).MatchString(insertPendingTranslationTxSQL) {
		t.Fatalf("insertPendingTranslationTxSQL must use targetless ON CONFLICT DO NOTHING: %s", insertPendingTranslationTxSQL)
	}

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID, translationID := uuid.New(), uuid.New()
	revision := int64(7)
	params := UpsertTranslationParams{
		LinkID: linkID, Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 2, EndOffset: 7, SourceText: "hello",
		SourceFormat: model.TranslationFormatPlain, TargetLanguage: model.TranslationTargetChinese,
		SourceHash:            "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourceContentRevision: &revision,
	}
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO link_translations")).
		WithArgs(
			linkID, params.Scope, params.BlockKey, params.StartOffset, params.EndOffset,
			params.SourceText, params.SourceFormat, params.TargetLanguage, params.SourceHash,
			params.SourceContentRevision,
		).
		WillReturnRows(translationRevisionRow(translationID, linkID, model.TranslationStatusPending, &revision, 1))

	repo := NewPGXTranslationRepository(mock)
	got, inserted, err := repo.InsertPendingTranslationTx(context.Background(), mock, params)
	if err != nil || !inserted || got == nil || got.AttemptGeneration != 1 {
		t.Fatalf("InsertPendingTranslationTx() = %+v, %v, %v", got, inserted, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestAdvancePendingTranslationTxPreservesSourceIdentity(t *testing.T) {
	t.Parallel()

	for _, forbidden := range []string{"source_text =", "source_hash =", "source_content_revision ="} {
		if regexp.MustCompile(`(?i)` + regexp.QuoteMeta(forbidden)).MatchString(advancePendingTranslationTxSQL) {
			t.Fatalf("advancePendingTranslationTxSQL rewrites immutable identity via %q: %s", forbidden, advancePendingTranslationTxSQL)
		}
	}

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID, translationID := uuid.New(), uuid.New()
	revision := int64(7)
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE link_translations")).
		WithArgs(translationID).
		WillReturnRows(translationRevisionRow(translationID, linkID, model.TranslationStatusPending, &revision, 4))

	repo := NewPGXTranslationRepository(mock)
	got, err := repo.AdvancePendingTranslationTx(context.Background(), mock, translationID)
	if err != nil || got == nil || got.AttemptGeneration != 4 || got.SourceContentRevision == nil || *got.SourceContentRevision != revision {
		t.Fatalf("AdvancePendingTranslationTx() = %+v, %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
