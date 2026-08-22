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

func translationRow(id, linkID uuid.UUID, status model.TranslationStatus, translated *string) *pgxmock.Rows {
	now := time.Now().UTC()
	return pgxmock.NewRows([]string{
		"id", "link_id", "scope", "block_key", "start_offset", "end_offset",
		"source_text", "translated_text", "source_format", "target_language", "source_hash",
		"source_content_revision", "status", "model", "error_msg", "attempt_generation", "created_at", "updated_at",
	}).AddRow(
		id, linkID, "selection", "summary", 3, 8,
		"hello", translated, "plain", "zh-CN", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		nil, string(status), nil, nil, int64(1), now, now,
	)
}

func TestTranslationRepositoryListsTranslations(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXTranslationRepository(mock)
	linkID, translationID := uuid.New(), uuid.New()
	translated := "translated"

	mock.ExpectQuery(regexp.QuoteMeta("FROM link_translations WHERE link_id = $1")).
		WithArgs(linkID).
		WillReturnRows(translationRow(translationID, linkID, model.TranslationStatusDone, &translated))
	items, err := repo.ListByLink(context.Background(), linkID)
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
	const revision int64 = 9
	const summary = "A **summary**"
	const content = "Saved body"
	translated := "translated body"

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	mock.ExpectQuery(regexp.QuoteMeta("FROM links WHERE id = $1")).
		WithArgs(linkID).
		WillReturnRows(pgxmock.NewRows([]string{
			"status", "library_kind", "content_revision", "content_present", "content",
			"document_present", "content_document", "content_format", "summary_present", "summary",
		}).AddRow("done", "reading", revision, true, content, false, "", "plain", true, summary))
	mock.ExpectQuery(regexp.QuoteMeta("FROM link_translations WHERE link_id = $1")).
		WithArgs(linkID).
		WillReturnRows(translationRow(translationID, linkID, model.TranslationStatusDone, &translated))
	mock.ExpectCommit()

	snapshot, err := repo.ReadListSnapshot(context.Background(), linkID)
	if err != nil {
		t.Fatalf("ReadListSnapshot() error = %v", err)
	}
	if snapshot == nil || snapshot.Source.LibraryKind == nil || *snapshot.Source.LibraryKind != model.LibraryKindReading ||
		snapshot.Source.ContentRevision != revision || snapshot.Source.Summary == nil || *snapshot.Source.Summary != summary ||
		snapshot.Source.Content == nil || *snapshot.Source.Content != content || len(snapshot.Items) != 1 ||
		snapshot.Items[0].ID != translationID {
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

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	mock.ExpectQuery(regexp.QuoteMeta("FROM links WHERE id = $1")).WithArgs(linkID).WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()
	snapshot, err := repo.ReadListSnapshot(context.Background(), linkID)
	if err != nil || snapshot != nil {
		t.Fatalf("ReadListSnapshot() = %+v, %v; want nil, nil", snapshot, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
