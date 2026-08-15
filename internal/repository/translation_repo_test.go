package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func translationRow(id, linkID uuid.UUID, status model.TranslationStatus, translated *string) *pgxmock.Rows {
	now := time.Now().UTC()
	return pgxmock.NewRows([]string{
		"id", "link_id", "scope", "block_key", "start_offset", "end_offset",
		"source_text", "translated_text", "source_format", "target_language", "source_hash",
		"source_content_revision", "status", "model", "error_msg", "attempt_generation", "current_river_job_id", "created_at", "updated_at",
	}).AddRow(
		id, linkID, "selection", "summary", 3, 8,
		"hello", translated, "plain", "zh-CN", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		nil, string(status), nil, nil, int64(1), nil, now, now,
	)
}

func TestTranslationRepositoryUpsertsAndLists(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXTranslationRepository(mock)
	linkID, translationID := uuid.New(), uuid.New()
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
		WillReturnRows(translationRow(translationID, linkID, model.TranslationStatusPending, nil))
	const riverJobID int64 = 101
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE link_translations")).
		WithArgs(riverJobID, translationID, int64(1)).
		WillReturnRows(translationAttemptRow(
			translationID, linkID, model.TranslationStatusPending, 1, ptrTo(riverJobID),
		))
	mock.ExpectCommit()
	created, scheduled, err := repo.SchedulePending(ctx, params, func(_ context.Context, _ pgx.Tx, command TranslationScheduleCommand) (int64, error) {
		if command.Seed.TranslationID != translationID ||
			command.Seed.AttemptGeneration != 1 || command.Previous != nil {
			t.Fatalf("schedule command = %+v", command)
		}
		return riverJobID, nil
	})
	if err != nil {
		t.Fatalf("SchedulePending() error = %v", err)
	}
	if !scheduled {
		t.Fatal("SchedulePending() scheduled = false, want true")
	}
	if created.ID != translationID || created.Status != model.TranslationStatusPending {
		t.Fatalf("SchedulePending() = %+v", created)
	}

	translated := "你好"
	mock.ExpectQuery(regexp.QuoteMeta("FROM link_translations WHERE link_id = $1")).
		WithArgs(linkID).
		WillReturnRows(translationRow(translationID, linkID, model.TranslationStatusDone, &translated))
	items, err := repo.ListByLink(ctx, linkID)
	if err != nil {
		t.Fatalf("ListByLink() error = %v", err)
	}
	if len(items) != 1 || items[0].TranslatedText == nil || *items[0].TranslatedText != translated {
		t.Fatalf("ListByLink() = %+v", items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationRepositoryReadsListSnapshotAtRepeatableRead(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXTranslationRepository(mock)
	linkID, translationID := uuid.New(), uuid.New()
	ctx := context.Background()
	const revision int64 = 9
	const summary = "A **summary**"
	const content = "Saved body"
	translated := "已保存正文"

	mock.ExpectBeginTx(pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	mock.ExpectQuery(regexp.QuoteMeta("FROM links WHERE id = $1")).
		WithArgs(linkID).
		WillReturnRows(pgxmock.NewRows([]string{
			"status", "library_kind", "content_revision", "content_present", "content",
			"document_present", "content_document", "content_format", "summary_present", "summary",
		}).AddRow("done", "reading", revision, true, content, false, "", "plain", true, summary))
	mock.ExpectQuery(regexp.QuoteMeta("FROM link_translations WHERE link_id = $1")).
		WithArgs(linkID).
		WillReturnRows(translationRow(
			translationID,
			linkID,
			model.TranslationStatusDone,
			&translated,
		))
	mock.ExpectCommit()

	snapshot, err := repo.ReadListSnapshot(ctx, linkID)
	if err != nil {
		t.Fatalf("ReadListSnapshot() error = %v", err)
	}
	if snapshot == nil || snapshot.Source.LibraryKind == nil || *snapshot.Source.LibraryKind != model.LibraryKindReading ||
		snapshot.Source.ContentRevision != revision || snapshot.Source.Summary == nil || *snapshot.Source.Summary != summary ||
		snapshot.Source.Content == nil || *snapshot.Source.Content != content ||
		len(snapshot.Items) != 1 || snapshot.Items[0].ID != translationID {
		t.Fatalf("ReadListSnapshot() = %+v", snapshot)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationRepositoryListSnapshotReturnsNilWhenLinkIsMissing(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXTranslationRepository(mock)
	linkID := uuid.New()
	ctx := context.Background()

	mock.ExpectBeginTx(pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	mock.ExpectQuery(regexp.QuoteMeta("FROM links WHERE id = $1")).
		WithArgs(linkID).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	snapshot, err := repo.ReadListSnapshot(ctx, linkID)
	if err != nil || snapshot != nil {
		t.Fatalf("ReadListSnapshot() = %+v, %v, want nil, nil", snapshot, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationRepositoryReusesActiveConflictWithoutScheduling(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXTranslationRepository(mock)
	linkID, translationID := uuid.New(), uuid.New()
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
		WillReturnRows(translationRow(translationID, linkID, model.TranslationStatusProcessing, nil))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO link_translations")).
		WithArgs(
			linkID, params.Scope, params.BlockKey, params.StartOffset,
			params.EndOffset, params.SourceText, params.SourceFormat,
			params.TargetLanguage, params.SourceHash, params.SourceContentRevision,
		).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM link_translations")).
		WithArgs(
			linkID, params.Scope, params.BlockKey, params.StartOffset,
			params.EndOffset, params.SourceHash, params.TargetLanguage,
		).
		WillReturnRows(translationRow(translationID, linkID, model.TranslationStatusProcessing, nil))

	mock.ExpectCommit()
	item, scheduled, err := repo.SchedulePending(ctx, params, nil)
	if err != nil {
		t.Fatalf("SchedulePending() error = %v", err)
	}
	if scheduled || item == nil || item.ID != translationID {
		t.Fatalf("SchedulePending() = %+v, scheduled=%v", item, scheduled)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationRepositoryCompletesOnlyCurrentJob(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXTranslationRepository(mock)
	translationID := uuid.New()
	const sourceHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ctx := context.Background()
	attempt := model.TranslationAttempt{
		TranslationID:     translationID,
		AttemptGeneration: 1, RiverJobID: 42,
		SourceHash: sourceHash,
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE link_translations")).
		WithArgs("你好", "grok-4.3-fast", translationID, int64(42), int64(1), sourceHash, (*int64)(nil)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	completed, err := repo.Complete(ctx, attempt, "你好", "grok-4.3-fast")
	if err != nil || !completed {
		t.Fatalf("Complete() = %v, %v; want true, nil", completed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationRepositoryRollsBackPendingWhenScheduleHookFails(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXTranslationRepository(mock)
	linkID, translationID := uuid.New(), uuid.New()
	ctx := context.Background()
	params := UpsertTranslationParams{
		LinkID:         linkID,
		Scope:          model.TranslationScopeSelection,
		BlockKey:       "summary",
		StartOffset:    0,
		EndOffset:      5,
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
		WillReturnRows(translationRow(translationID, linkID, model.TranslationStatusPending, nil))
	mock.ExpectRollback()
	hookErr := errors.New("river insert failed")
	item, scheduled, err := repo.SchedulePending(ctx, params, func(context.Context, pgx.Tx, TranslationScheduleCommand) (int64, error) {
		return 0, hookErr
	})
	if !errors.Is(err, hookErr) || item != nil || scheduled {
		t.Fatalf("SchedulePending() = %+v, %v, %v; want nil, false, hook error", item, scheduled, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
