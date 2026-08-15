package repository

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

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
	linkID, jobID := uuid.New(), uuid.New()
	inputTitle := "latest title"
	inputText := "latest snapshot"
	description := "latest note"
	images := []string{"https://cdn.example.com/latest.png"}
	metadata := map[string]any{"capture": "latest"}
	now := time.Now().UTC()

	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	mock.ExpectExec(regexp.QuoteMeta(requeueLinkCaptureSQL)).
		WithArgs(
			linkID, "browser_capture", "capture:latest", &inputTitle, &inputText,
			(*string)(nil), mustJSONString(t, images), mustJSONString(t, metadata),
			&description, model.RequestedLibraryKindAuto,
			model.RequestedLibraryKindSourceAuto,
		).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(supersedeParseJobsSQL)).
		WithArgs(linkID, parseJobSupersededMessage).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(regexp.QuoteMeta(insertJobSQL)).
		WithArgs(linkID, parseJobsPerLinkRetention).
		WillReturnRows(mock.NewRows(jobColumns()).AddRow(
			jobID, linkID, model.JobStatusPending, nil, now, now, int64(1),
		))
	mock.ExpectRollback()

	ctx := context.Background()
	tx, err := mock.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	job, err := repo.RequeueExistingTx(ctx, tx, linkID, &CreateLinkParams{
		SourceKind:     "browser_capture",
		SourceKey:      "capture:latest",
		InputTitle:     &inputTitle,
		InputText:      &inputText,
		InputImages:    images,
		SourceMetadata: metadata,
		Description:    &description,
	})
	if err != nil {
		t.Fatalf("RequeueExistingTx() error = %v", err)
	}
	if job == nil || job.ID != jobID || job.LinkID != linkID {
		t.Fatalf("job = %#v, want %s for link %s", job, jobID, linkID)
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

func TestRequeueCaptureSQLDerivesPurgeDeadlineFromEffectiveIntent(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"WHEN $11 = 'user' THEN CASE WHEN $10 = 'site' THEN NOW() + INTERVAL '24 hours' ELSE NULL END",
		"WHEN requested_library_kind_source = 'user' THEN CASE WHEN requested_library_kind = 'site' THEN NOW() + INTERVAL '24 hours' ELSE NULL END",
		"WHEN $10 = 'reading' THEN NULL",
		"WHEN requested_library_kind = 'reading' THEN NULL",
		"WHEN library_kind = 'site' THEN NOW() + INTERVAL '24 hours'",
	} {
		if !strings.Contains(requeueLinkCaptureSQL, want) {
			t.Fatalf("requeueLinkCaptureSQL missing effective-intent purge branch %q: %s", want, requeueLinkCaptureSQL)
		}
	}
}

func TestUpdateRequestedLibraryIntentSQLClearsSiteDeadlineForAutomaticReadingRule(t *testing.T) {
	t.Parallel()

	if !strings.Contains(updateRequestedLibraryIntentSQL, "WHEN $2 = 'reading' THEN NULL") {
		t.Fatalf("automatic reading intent must clear a stale site purge deadline: %s", updateRequestedLibraryIntentSQL)
	}
}

func TestRequeueCaptureSQLInvalidatesSourceDerivedArtifacts(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"content = NULL",
		"content_document = NULL",
		"content_format = 'plain'",
		"content_revision = content_revision + 1",
		"embedding = NULL",
		"embedding_model = NULL",
	} {
		if !strings.Contains(requeueLinkCaptureSQL, want) {
			t.Fatalf("requeueLinkCaptureSQL missing %q: %s", want, requeueLinkCaptureSQL)
		}
	}
}

func TestUpdateRequestedLibraryIntentTxPreservesCommittedUserIntentFromAutomaticRule(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID := uuid.New()

	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockRequestedLibraryIntentSQL)).
		WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"status", "requested_library_kind_source"}).
			AddRow(model.LinkStatusProcessing, model.RequestedLibraryKindSourceUser))
	mock.ExpectRollback()

	ctx := context.Background()
	tx, err := mock.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	result, err := repo.UpdateRequestedLibraryIntentTx(ctx, tx, UpdateRequestedLibraryIntentParams{
		ID: linkID, Kind: model.RequestedLibraryKindReading, Source: model.RequestedLibraryKindSourceAuto,
	})
	if err != nil {
		t.Fatalf("UpdateRequestedLibraryIntentTx() error = %v", err)
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

func TestUpdateRequestedLibraryIntentTxLetsLaterUserChoiceWin(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID := uuid.New()

	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockRequestedLibraryIntentSQL)).
		WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"status", "requested_library_kind_source"}).
			AddRow(model.LinkStatusPending, model.RequestedLibraryKindSourceUser))
	mock.ExpectExec(regexp.QuoteMeta(updateRequestedLibraryIntentSQL)).
		WithArgs(linkID, model.RequestedLibraryKindSite, model.RequestedLibraryKindSourceUser).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectRollback()

	ctx := context.Background()
	tx, err := mock.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	result, err := repo.UpdateRequestedLibraryIntentTx(ctx, tx, UpdateRequestedLibraryIntentParams{
		ID: linkID, Kind: model.RequestedLibraryKindSite, Source: model.RequestedLibraryKindSourceUser,
	})
	if err != nil {
		t.Fatalf("UpdateRequestedLibraryIntentTx() error = %v", err)
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

func TestRequeueRefreshSQLPreservesContentAndInvalidatesEmbedding(t *testing.T) {
	t.Parallel()

	if strings.Contains(requeueLinkRefreshSQL, "content") {
		t.Fatalf("refresh must preserve explicitly saved content: %s", requeueLinkRefreshSQL)
	}
	for _, want := range []string{"embedding = NULL", "embedding_model = NULL"} {
		if !strings.Contains(requeueLinkRefreshSQL, want) {
			t.Fatalf("requeueLinkRefreshSQL missing %q: %s", want, requeueLinkRefreshSQL)
		}
	}
}
