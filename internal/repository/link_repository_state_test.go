package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestLinkRepositoryDeleteReturnsNotFoundWhenNoRowsAreAffected(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXLinkRepository(mock)
	linkID := uuid.MustParse("66666666-6666-6666-6666-666666666666")

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockLinkForDeleteSQL)).
		WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(linkID))
	mock.ExpectExec(regexp.QuoteMeta(terminalizeDeletedTranslationAttemptsSQL)).
		WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(regexp.QuoteMeta(deleteLinkSQL)).
		WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectRollback()

	err = repo.Delete(t.Context(), linkID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete() error = %v, want ErrNotFound", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestLinkRepositoryUpdateAnalysisPersistsMultimodalFields(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXLinkRepository(mock)
	linkID := uuid.MustParse("12121212-3434-5656-7878-909090909090")
	inputTitle := "Normalized Input"
	inputText := "Normalized text"
	inputHTML := "<p>Normalized text</p>"
	title := "Analyzed Title"
	summary := "Analyzed summary"
	fetcherType := "openai"
	lowConfidenceReason := "sparse_content"
	domain := "example.com"
	contentType := "article"
	pathDepth := 2
	parentPath := "/docs/"
	images := []string{"https://cdn.example.com/a.png"}
	metadata := map[string]any{"origin": "upload"}

	mock.ExpectExec(regexp.QuoteMeta(
		"UPDATE links SET source_kind = COALESCE(NULLIF($2, ''), source_kind), source_key = COALESCE(NULLIF($3, ''), source_key), input_title = COALESCE($4, input_title), input_text = COALESCE($5, input_text), input_html = COALESCE($6, input_html), input_images = COALESCE($7::jsonb, input_images), source_metadata = COALESCE($8::jsonb, source_metadata), title = $9, summary = $10, tags = COALESCE($11, '{}'::text[]), fetcher_type = $12, is_low_confidence = $13, low_confidence_reason = $14, status = $15, error_msg = $16, domain = $17, content_type = $18, path_depth = $19, parent_path = $20, parent_id = $21, updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL",
	)).
		WithArgs(
			linkID,
			"document",
			"upload:normalized-1",
			&inputTitle,
			&inputText,
			&inputHTML,
			mustJSONString(t, images),
			mustJSONString(t, metadata),
			&title,
			&summary,
			[]string{"go", "ai"},
			&fetcherType,
			true,
			&lowConfidenceReason,
			model.LinkStatusDone,
			(*string)(nil),
			&domain,
			&contentType,
			&pathDepth,
			&parentPath,
			nil,
		).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	err = repo.UpdateAnalysis(context.Background(), UpdateLinkAnalysisParams{
		ID:                  linkID,
		SourceKind:          "document",
		SourceKey:           "upload:normalized-1",
		InputTitle:          &inputTitle,
		InputText:           &inputText,
		InputHTML:           &inputHTML,
		InputImages:         images,
		SourceMetadata:      metadata,
		Title:               &title,
		Summary:             &summary,
		Tags:                []string{"go", "ai"},
		FetcherType:         &fetcherType,
		IsLowConfidence:     true,
		LowConfidenceReason: &lowConfidenceReason,
		Status:              model.LinkStatusDone,
		Domain:              &domain,
		ContentType:         &contentType,
		PathDepth:           &pathDepth,
		ParentPath:          &parentPath,
	})
	if err != nil {
		t.Fatalf("UpdateAnalysis() error = %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}
