package repository

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestRequeueExistingTxReturnsCreatedAttemptWithoutOwningCommit(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID := uuid.New()
	inputTitle := "latest title"
	inputText := "latest snapshot"
	description := "latest note"
	images := []string{"https://cdn.example.com/latest.png"}
	metadata := map[string]any{"capture": "latest"}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(requeueLinkCaptureSQL)).
		WithArgs(
			linkID, "browser_capture", "capture:latest", &inputTitle, &inputText,
			(*string)(nil), mustJSONString(t, images), mustJSONString(t, metadata),
			&description, model.RequestedLibraryKindAuto,
			false,
		).
		WillReturnRows(mock.NewRows([]string{"parse_generation", "metadata_revision"}).
			AddRow(int64(3), int64(5)))
	mock.ExpectRollback()

	ctx := context.Background()
	tx, err := mock.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	attempt, err := repo.RequeueExistingTx(ctx, tx, linkID, &CreateLinkParams{
		SourceKind:           "browser_capture",
		SourceKey:            "capture:latest",
		InputTitle:           &inputTitle,
		InputText:            &inputText,
		InputImages:          images,
		SourceMetadata:       metadata,
		Description:          &description,
		RequestedLibraryKind: model.RequestedLibraryKindAuto,
	})
	if err != nil {
		t.Fatalf("RequeueExistingTx() error = %v", err)
	}
	if attempt.LinkID != linkID || attempt.Generation != 3 || attempt.ExpectedMetadataRevision != 5 {
		t.Fatalf("attempt = %#v, want link %s generation 3 revision 5", attempt, linkID)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestRequeueCaptureSQLPreservesDescriptionWhenOmitted(t *testing.T) {
	t.Parallel()
	if !strings.Contains(requeueLinkCaptureSQL, "description = COALESCE($9, description)") {
		t.Fatalf("requeueLinkCaptureSQL must preserve description on nil input: %s", requeueLinkCaptureSQL)
	}
}

func TestRequeueCaptureSQLDerivesPurgeDeadlineFromEffectiveSelection(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"WHEN $10 = 'site' AND ($11 OR NOT library_kind_locked) THEN NOW() + INTERVAL '24 hours'",
		"WHEN $10 = 'reading' AND ($11 OR NOT library_kind_locked) THEN NULL",
		"WHEN library_kind = 'site' THEN NOW() + INTERVAL '24 hours'",
	} {
		if !strings.Contains(requeueLinkCaptureSQL, want) {
			t.Fatalf("requeueLinkCaptureSQL missing effective-intent purge branch %q: %s", want, requeueLinkCaptureSQL)
		}
	}
}

func TestSetLibraryKindSQLMaintainsSiteDeadline(t *testing.T) {
	t.Parallel()

	if !strings.Contains(setLibraryKindSQL, "WHEN $2 = 'reading' THEN NULL") {
		t.Fatalf("reading selection must clear a stale site purge deadline: %s", setLibraryKindSQL)
	}
}

func TestRequeueCaptureSQLInvalidatesSourceDerivedArtifacts(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"content = NULL",
		"content_document = NULL",
		"content_format = 'plain'",
		"content_revision = content_revision + 1",
	} {
		if !strings.Contains(requeueLinkCaptureSQL, want) {
			t.Fatalf("requeueLinkCaptureSQL missing %q: %s", want, requeueLinkCaptureSQL)
		}
	}
}

func TestSetLibraryKindTxPreservesExistingLockWithoutOverride(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockLibraryKindSQL)).
		WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"status", "library_kind_locked"}).
			AddRow(model.LinkStatusProcessing, true))
	mock.ExpectRollback()

	ctx := context.Background()
	tx, err := mock.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	result, err := repo.SetLibraryKindTx(ctx, tx, SetLibraryKindParams{
		ID: linkID, Kind: model.LibraryKindReading,
	})
	if err != nil {
		t.Fatalf("SetLibraryKindTx() error = %v", err)
	}
	if result.Status != model.LinkStatusProcessing {
		t.Fatalf("status = %q, want processing", result.Status)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestSetLibraryKindTxLetsExplicitOverrideWin(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(lockLibraryKindSQL)).
		WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"status", "library_kind_locked"}).
			AddRow(model.LinkStatusPending, true))
	mock.ExpectExec(regexp.QuoteMeta(setLibraryKindSQL)).
		WithArgs(linkID, model.LibraryKindSite).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectRollback()

	ctx := context.Background()
	tx, err := mock.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	result, err := repo.SetLibraryKindTx(ctx, tx, SetLibraryKindParams{
		ID: linkID, Kind: model.LibraryKindSite, Override: true,
	})
	if err != nil {
		t.Fatalf("SetLibraryKindTx() error = %v", err)
	}
	if result.Status != model.LinkStatusPending {
		t.Fatalf("status = %q, want pending", result.Status)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestRequeueRefreshSQLPreservesSavedContent(t *testing.T) {
	t.Parallel()

	if strings.Contains(requeueLinkRefreshSQL, "content") {
		t.Fatalf("refresh must preserve explicitly saved content: %s", requeueLinkRefreshSQL)
	}
}
