package durablework

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/contentdoc"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type translationQueueStub struct {
	jobID   int64
	err     error
	command *model.TranslationScheduleCommand
}

func (q *translationQueueStub) EnqueueTranslationTx(_ context.Context, _ pgx.Tx, command model.TranslationScheduleCommand) (int64, error) {
	copy := command
	q.command = &copy
	if q.err != nil {
		return 0, q.err
	}
	return q.jobID, nil
}

func expectLockedPlainSource(mock pgxmock.PgxPoolIface, linkID uuid.UUID, revision int64, content, summary string) {
	mock.ExpectQuery(regexp.QuoteMeta("FROM links WHERE id=$1 FOR UPDATE")).
		WithArgs(linkID).
		WillReturnRows(pgxmock.NewRows([]string{
			"status", "library_kind", "content_revision", "content_present", "content",
			"document_present", "content_document", "content_format", "summary_present", "summary",
		}).AddRow("done", "reading", revision, true, content, false, "", "plain", true, summary))
}

func expectLockedSourceSnapshot(
	mock pgxmock.PgxPoolIface,
	linkID uuid.UUID,
	source repository.TranslationSourceSnapshot,
) {
	libraryKind := ""
	if source.LibraryKind != nil {
		libraryKind = string(*source.LibraryKind)
	}
	content, contentPresent := "", source.Content != nil
	if contentPresent {
		content = *source.Content
	}
	document, documentPresent := "", source.ContentDocument != nil
	if documentPresent {
		document = *source.ContentDocument
	}
	summary, summaryPresent := "", source.Summary != nil
	if summaryPresent {
		summary = *source.Summary
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM links WHERE id=$1 FOR UPDATE")).
		WithArgs(linkID).
		WillReturnRows(pgxmock.NewRows([]string{
			"status", "library_kind", "content_revision", "content_present", "content",
			"document_present", "content_document", "content_format", "summary_present", "summary",
		}).AddRow(
			source.Status, libraryKind, source.ContentRevision, contentPresent, content,
			documentPresent, document, source.ContentFormat, summaryPresent, summary,
		))
}

func durableTranslationRow(
	id, linkID uuid.UUID,
	revision *int64,
	generation int64,
	jobID *int64,
) *pgxmock.Rows {
	now := time.Now().UTC()
	return pgxmock.NewRows([]string{
		"id", "link_id", "scope", "block_key", "start_offset", "end_offset",
		"source_text", "translated_text", "source_format", "target_language", "source_hash",
		"source_content_revision", "status", "model", "error_msg", "attempt_generation",
		"current_river_job_id", "created_at", "updated_at",
	}).AddRow(
		id, linkID, "selection", "content", 0, 5, "hello", nil, "plain", "zh-CN",
		"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		revision, "pending", nil, nil, generation, jobID, now, now,
	)
}

func durableSummaryTranslationRow(
	id, linkID uuid.UUID,
	sourceHash string,
	generation int64,
	jobID *int64,
) *pgxmock.Rows {
	now := time.Now().UTC()
	return pgxmock.NewRows([]string{
		"id", "link_id", "scope", "block_key", "start_offset", "end_offset",
		"source_text", "translated_text", "source_format", "target_language", "source_hash",
		"source_content_revision", "status", "model", "error_msg", "attempt_generation",
		"current_river_job_id", "created_at", "updated_at",
	}).AddRow(
		id, linkID, "selection", "summary", 0, 5, "hello", nil, "plain", "zh-CN",
		sourceHash,
		nil, "pending", nil, nil, generation, jobID, now, now,
	)
}

func TestTranslationSchedulerRejectsStaleRevisionBeforeAnyProductOrJobWrite(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID := uuid.New()
	expected := int64(7)
	mock.ExpectBegin()
	expectLockedPlainSource(mock, linkID, 8, "hello world", "summary")
	mock.ExpectRollback()

	queue := &translationQueueStub{jobID: 41}
	scheduler := NewTranslationScheduler(TranslationSchedulerOptions{
		Transactions: mock,
		Products:     repository.NewPGXTranslationRepository(mock),
		Queue:        queue,
	})
	_, err = scheduler.Schedule(context.Background(), linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "hello",
		ExpectedContentRevision: &expected,
	}, time.Hour)
	var status httperr.StatusCarrier
	var code httperr.ErrorCoder
	if !errors.As(err, &status) || status.HTTPStatus() != 409 ||
		!errors.As(err, &code) || code.HTTPErrorCode() != httperr.CodeContentRevisionConflict {
		t.Fatalf("Schedule() error = %v, want 409/%s", err, httperr.CodeContentRevisionConflict)
	}
	if queue.command != nil {
		t.Fatalf("stale request enqueued command %+v", *queue.command)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationSchedulerPrioritizesStaleSavedRevisionAndReturnsCanonicalBlock(t *testing.T) {
	t.Parallel()

	reading, site := model.LibraryKindReading, model.LibraryKindSite
	currentPlain := "current plain body"
	currentDocument := "# Current document"
	for _, tc := range []struct {
		name         string
		source       repository.TranslationSourceSnapshot
		request      model.TranslationRequest
		wantBlockKey string
	}{
		{
			name: "capture requeue cleared the saved body and made the link pending",
			source: repository.TranslationSourceSnapshot{
				Status: model.LinkStatusPending, LibraryKind: &reading,
				ContentRevision: 8, ContentFormat: model.ContentFormatPlain,
			},
			request: model.TranslationRequest{
				Scope: model.TranslationScopeFull, ExpectedContentRevision: ptr(int64(7)),
			},
			wantBlockKey: "content",
		},
		{
			name: "site completion cleared the saved body and changed eligibility",
			source: repository.TranslationSourceSnapshot{
				Status: model.LinkStatusDone, LibraryKind: &site,
				ContentRevision: 8, ContentFormat: model.ContentFormatPlain,
			},
			request: model.TranslationRequest{
				Scope: model.TranslationScopeFull, ExpectedContentRevision: ptr(int64(7)),
			},
			wantBlockKey: "content",
		},
		{
			name: "current reading generation has no saved body",
			source: repository.TranslationSourceSnapshot{
				Status: model.LinkStatusDone, LibraryKind: &reading,
				ContentRevision: 8, ContentFormat: model.ContentFormatPlain,
			},
			request: model.TranslationRequest{
				Scope: model.TranslationScopeFull, ExpectedContentRevision: ptr(int64(7)),
			},
			wantBlockKey: "content",
		},
		{
			name: "plain selection was replaced by a markdown document",
			source: repository.TranslationSourceSnapshot{
				Status: model.LinkStatusDone, LibraryKind: &reading, ContentRevision: 8,
				Content: &currentPlain, ContentDocument: &currentDocument,
				ContentFormat: model.ContentFormatMarkdown,
			},
			request: model.TranslationRequest{
				Scope: model.TranslationScopeSelection, BlockKey: "content",
				StartOffset: 0, EndOffset: 5, SourceText: "stale",
				ExpectedContentRevision: ptr(int64(7)),
			},
			wantBlockKey: "content-document",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock.NewPool() error = %v", err)
			}
			defer mock.Close()

			linkID := uuid.New()
			mock.ExpectBegin()
			expectLockedSourceSnapshot(mock, linkID, tc.source)
			mock.ExpectRollback()

			queue := &translationQueueStub{jobID: 41}
			scheduler := NewTranslationScheduler(TranslationSchedulerOptions{
				Transactions: mock,
				Products:     repository.NewPGXTranslationRepository(mock),
				Queue:        queue,
			})
			item, err := scheduler.Schedule(
				context.Background(),
				linkID,
				tc.request,
				time.Hour,
			)
			if item != nil {
				t.Fatalf("Schedule() item = %+v, want nil", item)
			}
			var status httperr.StatusCarrier
			var coder httperr.ErrorCoder
			if !errors.As(err, &status) || status.HTTPStatus() != http.StatusConflict ||
				!errors.As(err, &coder) || coder.HTTPErrorCode() != httperr.CodeContentRevisionConflict {
				t.Fatalf("Schedule() error = %v, want 409/%s", err, httperr.CodeContentRevisionConflict)
			}
			var identityProvider httperr.CurrentIdentityProvider
			if !errors.As(err, &identityProvider) {
				t.Fatalf("Schedule() error = %v, want current identity", err)
			}
			identity, ok := identityProvider.HTTPCurrentIdentity()
			if !ok || identity.ContentRevision == nil || *identity.ContentRevision != tc.source.ContentRevision ||
				identity.BlockKey != tc.wantBlockKey || identity.SourceHash != nil {
				t.Fatalf("current identity = %+v/%v, want revision %d/block %s",
					identity, ok, tc.source.ContentRevision, tc.wantBlockKey)
			}
			if queue.command != nil {
				t.Fatalf("stale request enqueued command %+v", *queue.command)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet expectations: %v", err)
			}
		})
	}
}

func TestTranslationSchedulerCountsCanonicalSourceBlockConflict(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID := uuid.New()
	revision := int64(8)
	mock.ExpectBegin()
	expectLockedPlainSource(mock, linkID, revision, "hello world", "summary")
	mock.ExpectRollback()

	queue := &translationQueueStub{jobID: 41}
	scheduler := NewTranslationScheduler(TranslationSchedulerOptions{
		Transactions: mock,
		Products:     repository.NewPGXTranslationRepository(mock),
		Queue:        queue,
	})
	_, err = scheduler.Schedule(context.Background(), linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "HELLO",
		ExpectedContentRevision: &revision,
	}, time.Hour)
	var code httperr.ErrorCoder
	if !errors.As(err, &code) || code.HTTPErrorCode() != httperr.CodeSourceBlockConflict {
		t.Fatalf("Schedule() error = %v, want %s", err, httperr.CodeSourceBlockConflict)
	}
	if queue.command != nil {
		t.Fatalf("mismatched source enqueued command %+v", *queue.command)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationSchedulerDoesNotMisclassifyUnrelatedValidationFailure(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID := uuid.New()
	revision := int64(8)
	mock.ExpectBegin()
	expectLockedPlainSource(mock, linkID, revision, "hello world", "summary")
	mock.ExpectRollback()

	scheduler := NewTranslationScheduler(TranslationSchedulerOptions{
		Transactions: mock,
		Products:     repository.NewPGXTranslationRepository(mock),
		Queue:        &translationQueueStub{jobID: 41},
	})
	_, err = scheduler.Schedule(context.Background(), linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 1, SourceText: "h",
		ExpectedContentRevision: &revision,
	}, time.Hour)
	var code httperr.ErrorCoder
	if !errors.As(err, &code) || code.HTTPErrorCode() != httperr.CodeTranslationInvalidRequest {
		t.Fatalf("Schedule() error = %v, want %s", err, httperr.CodeTranslationInvalidRequest)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationSchedulerDoesNotCountReusableProductAsNewSchedule(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID, translationID := uuid.New(), uuid.New()
	revision := int64(8)
	mock.ExpectBegin()
	expectLockedPlainSource(mock, linkID, revision, "hello world", "summary")
	mock.ExpectQuery(regexp.QuoteMeta("source_content_revision = $6 AND target_language = $7 FOR UPDATE")).
		WithArgs(linkID, model.TranslationScopeSelection, "content", 0, 5, revision, model.TranslationTargetChinese).
		WillReturnRows(durableTranslationRow(translationID, linkID, &revision, 1, ptr(int64(41))))
	mock.ExpectCommit()

	queue := &translationQueueStub{jobID: 42}
	scheduler := NewTranslationScheduler(TranslationSchedulerOptions{
		Transactions: mock,
		Products:     repository.NewPGXTranslationRepository(mock),
		Queue:        queue,
	})
	got, err := scheduler.Schedule(context.Background(), linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "hello",
		ExpectedContentRevision: &revision,
	}, time.Hour)
	if err != nil || got == nil || got.ID != translationID {
		t.Fatalf("Schedule() = %+v, %v, want reusable product", got, err)
	}
	if queue.command != nil {
		t.Fatalf("reusable product enqueued command %+v", *queue.command)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationSchedulerCommitsProductAndRiverJobAsOneOperation(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID, translationID := uuid.New(), uuid.New()
	revision := int64(8)
	const jobID int64 = 41
	mock.ExpectBegin()
	expectLockedPlainSource(mock, linkID, revision, "hello world", "summary")
	mock.ExpectQuery(regexp.QuoteMeta("source_content_revision = $6 AND target_language = $7 FOR UPDATE")).
		WithArgs(linkID, model.TranslationScopeSelection, "content", 0, 5, revision, model.TranslationTargetChinese).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO link_translations")).
		WithArgs(
			linkID, model.TranslationScopeSelection, "content", 0, 5,
			"hello", model.TranslationFormatPlain, model.TranslationTargetChinese,
			"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", &revision,
		).
		WillReturnRows(durableTranslationRow(translationID, linkID, &revision, 1, nil))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE link_translations")).
		WithArgs(jobID, translationID, int64(1)).
		WillReturnRows(durableTranslationRow(translationID, linkID, &revision, 1, ptr(jobID)))
	mock.ExpectCommit()

	queue := &translationQueueStub{jobID: jobID}
	scheduler := NewTranslationScheduler(TranslationSchedulerOptions{
		Transactions: mock,
		Products:     repository.NewPGXTranslationRepository(mock),
		Queue:        queue,
	})
	ctx := context.Background()
	got, err := scheduler.Schedule(ctx, linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "hello",
		ExpectedContentRevision: &revision,
	}, time.Hour)
	if err != nil || got == nil || got.CurrentRiverJobID == nil || *got.CurrentRiverJobID != jobID {
		t.Fatalf("Schedule() = %+v, %v", got, err)
	}
	if queue.command == nil || queue.command.Seed.TranslationID != translationID ||
		queue.command.Seed.AttemptGeneration != 1 ||
		queue.command.Seed.SourceHash != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" ||
		queue.command.Seed.SourceContentRevision == nil ||
		*queue.command.Seed.SourceContentRevision != revision || queue.command.Previous != nil {
		t.Fatalf("queue command = %+v", queue.command)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationSchedulerCountsVerifiedSummaryHashSchedule(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID, translationID := uuid.New(), uuid.New()
	const jobID int64 = 43
	const summaryHash = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	summaryIdentity := contentdoc.RenderedSourceBlockPersistenceHash("summary", "hello world")
	mock.ExpectBegin()
	expectLockedPlainSource(mock, linkID, 8, "saved content", "hello world")
	mock.ExpectQuery(regexp.QuoteMeta("source_hash = $6")).
		WithArgs(
			linkID, model.TranslationScopeSelection, "summary", 0, 5,
			summaryIdentity, model.TranslationTargetChinese,
		).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO link_translations")).
		WithArgs(
			linkID, model.TranslationScopeSelection, "summary", 0, 5,
			"hello", model.TranslationFormatPlain, model.TranslationTargetChinese,
			summaryIdentity, (*int64)(nil),
		).
		WillReturnRows(durableSummaryTranslationRow(translationID, linkID, summaryIdentity, 1, nil))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE link_translations")).
		WithArgs(jobID, translationID, int64(1)).
		WillReturnRows(durableSummaryTranslationRow(translationID, linkID, summaryIdentity, 1, ptr(jobID)))
	mock.ExpectCommit()

	scheduler := NewTranslationScheduler(TranslationSchedulerOptions{
		Transactions: mock,
		Products:     repository.NewPGXTranslationRepository(mock),
		Queue:        &translationQueueStub{jobID: jobID},
	})
	ctx := context.Background()
	got, err := scheduler.Schedule(ctx, linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "summary",
		StartOffset: 0, EndOffset: 5, SourceText: "hello",
		ExpectedSourceHash: ptr(summaryHash),
	}, time.Hour)
	if err != nil || got == nil || got.SourceHash != summaryIdentity || got.SourceContentRevision != nil {
		t.Fatalf("Schedule() = %+v, %v, want verified domain-separated summary product", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestTranslationSchedulerRollsBackProductWhenRiverInsertFails(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID, translationID := uuid.New(), uuid.New()
	revision := int64(8)
	mock.ExpectBegin()
	expectLockedPlainSource(mock, linkID, revision, "hello world", "summary")
	mock.ExpectQuery(regexp.QuoteMeta("source_content_revision = $6 AND target_language = $7 FOR UPDATE")).
		WithArgs(linkID, model.TranslationScopeSelection, "content", 0, 5, revision, model.TranslationTargetChinese).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO link_translations")).
		WithArgs(
			linkID, model.TranslationScopeSelection, "content", 0, 5,
			"hello", model.TranslationFormatPlain, model.TranslationTargetChinese,
			"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", &revision,
		).
		WillReturnRows(durableTranslationRow(translationID, linkID, &revision, 1, nil))
	mock.ExpectRollback()

	queueErr := errors.New("river insert failed")
	scheduler := NewTranslationScheduler(TranslationSchedulerOptions{
		Transactions: mock,
		Products:     repository.NewPGXTranslationRepository(mock),
		Queue:        &translationQueueStub{err: queueErr},
	})
	ctx := context.Background()
	_, err = scheduler.Schedule(ctx, linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "hello",
		ExpectedContentRevision: &revision,
	}, time.Hour)
	if !errors.Is(err, queueErr) {
		t.Fatalf("Schedule() error = %v, want queue error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestAuthoritativeTranslationParamsSourceIdentityMatrix(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	revision := int64(8)
	reading := model.LibraryKindReading
	plain := "A😀中B"
	document := "# Heading\n\n**Bold** 😀中"
	summary := "**Hi** 😀中"
	summaryProjection, err := contentdoc.RenderedBlockProjection(model.ContentFormatMarkdown, summary)
	if err != nil {
		t.Fatalf("summary projection: %v", err)
	}
	summaryHash := hashTranslationSource(summaryProjection)
	summaryIdentity := contentdoc.RenderedSourceBlockPersistenceHash("summary", summaryProjection)

	base := repository.TranslationSourceSnapshot{
		Status: model.LinkStatusDone, LibraryKind: &reading, ContentRevision: revision,
		Content: &plain, ContentFormat: model.ContentFormatPlain, Summary: &summary,
	}
	markdown := base
	markdown.ContentDocument = &document
	markdown.ContentFormat = model.ContentFormatMarkdown
	nextLineText := "\u0085\u0085"
	nextLineContent := "A" + nextLineText + "Z"
	nextLine := base
	nextLine.Content = &nextLineContent
	byteOrderMarkText := "\uFEFF\uFEFF"
	byteOrderMarkContent := "A" + byteOrderMarkText + "Z"
	byteOrderMark := base
	byteOrderMark.Content = &byteOrderMarkContent

	for _, tc := range []struct {
		name       string
		source     repository.TranslationSourceSnapshot
		req        model.TranslationRequest
		wantCode   string
		assertions func(*testing.T, repository.UpsertTranslationParams)
	}{
		{
			name:   "saved selection uses UTF-16 offsets and revision",
			source: base,
			req: model.TranslationRequest{
				Scope: model.TranslationScopeSelection, BlockKey: "content",
				StartOffset: 1, EndOffset: 4, SourceText: "😀中", ExpectedContentRevision: &revision,
			},
			assertions: func(t *testing.T, got repository.UpsertTranslationParams) {
				t.Helper()
				if got.SourceContentRevision == nil || *got.SourceContentRevision != revision ||
					got.SourceHash != hashTranslationSource("😀中") {
					t.Fatalf("params = %+v", got)
				}
			},
		},
		{
			name:   "one astral character satisfies Reader minimum length",
			source: base,
			req: model.TranslationRequest{
				Scope: model.TranslationScopeSelection, BlockKey: "content",
				StartOffset: 1, EndOffset: 3, SourceText: "😀", ExpectedContentRevision: &revision,
			},
			assertions: func(t *testing.T, got repository.UpsertTranslationParams) {
				t.Helper()
				if got.SourceHash != hashTranslationSource("😀") {
					t.Fatalf("params = %+v", got)
				}
			},
		},
		{
			name:   "ECMAScript trim keeps next-line characters",
			source: nextLine,
			req: model.TranslationRequest{
				Scope: model.TranslationScopeSelection, BlockKey: "content",
				StartOffset: 1, EndOffset: 3, SourceText: nextLineText, ExpectedContentRevision: &revision,
			},
			assertions: func(t *testing.T, got repository.UpsertTranslationParams) {
				t.Helper()
				if got.SourceHash != hashTranslationSource(nextLineText) {
					t.Fatalf("params = %+v", got)
				}
			},
		},
		{
			name:   "ECMAScript trim removes byte-order marks",
			source: byteOrderMark,
			req: model.TranslationRequest{
				Scope: model.TranslationScopeSelection, BlockKey: "content",
				StartOffset: 1, EndOffset: 3, SourceText: byteOrderMarkText, ExpectedContentRevision: &revision,
			},
			wantCode: httperr.CodeTranslationInvalidRequest,
		},
		{
			name:   "verified summary persists domain-separated rendered block identity",
			source: base,
			req: model.TranslationRequest{
				Scope: model.TranslationScopeSelection, BlockKey: "summary",
				StartOffset: 0, EndOffset: 2, SourceText: "Hi", ExpectedSourceHash: &summaryHash,
			},
			assertions: func(t *testing.T, got repository.UpsertTranslationParams) {
				t.Helper()
				if got.SourceContentRevision != nil || got.SourceHash != summaryIdentity {
					t.Fatalf("params = %+v", got)
				}
			},
		},
		{
			name:   "full markdown carries saved revision",
			source: markdown,
			req: model.TranslationRequest{
				Scope: model.TranslationScopeFull, ExpectedContentRevision: &revision,
			},
			assertions: func(t *testing.T, got repository.UpsertTranslationParams) {
				t.Helper()
				if got.BlockKey != "content-document" || got.SourceText != document ||
					got.SourceFormat != model.TranslationFormatMarkdown || got.EndOffset != utf16Length(document) ||
					got.SourceContentRevision == nil || *got.SourceContentRevision != revision {
					t.Fatalf("params = %+v", got)
				}
			},
		},
		{
			name:   "strict saved request requires revision",
			source: base,
			req: model.TranslationRequest{
				Scope: model.TranslationScopeSelection, BlockKey: "content",
				StartOffset: 1, EndOffset: 4, SourceText: "😀中",
			},
			wantCode: httperr.CodeTranslationInvalidRequest,
		},
		{
			name:   "stale saved revision is a conflict",
			source: base,
			req: model.TranslationRequest{
				Scope: model.TranslationScopeFull, ExpectedContentRevision: ptr(int64(7)),
			},
			wantCode: httperr.CodeContentRevisionConflict,
		},
		{
			name:   "stale summary hash is a conflict",
			source: base,
			req: model.TranslationRequest{
				Scope: model.TranslationScopeSelection, BlockKey: "summary",
				StartOffset: 0, EndOffset: 2, SourceText: "Hi", ExpectedSourceHash: ptr("stale"),
			},
			wantCode: httperr.CodeSourceBlockConflict,
		},
		{
			name:   "retired deep research block cannot trust client text",
			source: base,
			req: model.TranslationRequest{
				Scope: model.TranslationScopeSelection, BlockKey: "dr",
				StartOffset: 0, EndOffset: 2, SourceText: "DR", ExpectedSourceHash: &summaryHash,
			},
			wantCode: httperr.CodeTranslationInvalidRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := authoritativeTranslationParams(&tc.source, linkID, tc.req)
			if tc.wantCode != "" {
				var status httperr.StatusCarrier
				var code httperr.ErrorCoder
				if !errors.As(err, &status) || !errors.As(err, &code) || code.HTTPErrorCode() != tc.wantCode {
					t.Fatalf("error = %v, want code %s", err, tc.wantCode)
				}
				if tc.wantCode == httperr.CodeTranslationInvalidRequest && status.HTTPStatus() != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422", status.HTTPStatus())
				}
				return
			}
			if err != nil {
				t.Fatalf("authoritativeTranslationParams() error = %v", err)
			}
			tc.assertions(t, got)
		})
	}
}

func TestAuthoritativeTranslationConflictsReturnCompleteCurrentIdentity(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	revision := int64(8)
	staleRevision := int64(7)
	reading := model.LibraryKindReading
	document := "# Current\n\nBody"
	source := repository.TranslationSourceSnapshot{
		Status: model.LinkStatusDone, LibraryKind: &reading, ContentRevision: revision,
		ContentDocument: &document, ContentFormat: model.ContentFormatMarkdown,
	}

	for _, tc := range []struct {
		name     string
		request  model.TranslationRequest
		wantCode string
	}{
		{
			name: "markdown full revision conflict names document block",
			request: model.TranslationRequest{
				Scope: model.TranslationScopeFull, ExpectedContentRevision: &staleRevision,
			},
			wantCode: httperr.CodeContentRevisionConflict,
		},
		{
			name: "saved selection mismatch carries current revision",
			request: model.TranslationRequest{
				Scope: model.TranslationScopeSelection, BlockKey: "content-document",
				StartOffset: 0, EndOffset: 7, SourceText: "Changed",
				ExpectedContentRevision: &revision,
			},
			wantCode: httperr.CodeSourceBlockConflict,
		},
		{
			name: "saved selection wrong block names canonical document",
			request: model.TranslationRequest{
				Scope: model.TranslationScopeSelection, BlockKey: "content",
				StartOffset: 0, EndOffset: 7, SourceText: "Current",
				ExpectedContentRevision: &revision,
			},
			wantCode: httperr.CodeSourceBlockConflict,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := authoritativeTranslationParams(&source, linkID, tc.request)
			var coder httperr.ErrorCoder
			if !errors.As(err, &coder) || coder.HTTPErrorCode() != tc.wantCode {
				t.Fatalf("error = %v, want %s", err, tc.wantCode)
			}
			var provider httperr.CurrentIdentityProvider
			if !errors.As(err, &provider) {
				t.Fatalf("error = %v, want current identity", err)
			}
			identity, ok := provider.HTTPCurrentIdentity()
			if !ok || identity.ContentRevision == nil || *identity.ContentRevision != revision ||
				identity.BlockKey != "content-document" || identity.SourceHash != nil {
				t.Fatalf("current identity = %+v/%v, want revision %d/content-document", identity, ok, revision)
			}
		})
	}
}

func TestReusableTranslationDecisionMatrix(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	stallAfter := 2 * time.Hour
	for _, tc := range []struct {
		name     string
		item     *model.LinkTranslation
		force    bool
		reusable bool
	}{
		{name: "absent", reusable: false},
		{name: "fresh pending", item: &model.LinkTranslation{Status: model.TranslationStatusPending, UpdatedAt: now.Add(-time.Minute)}, reusable: true},
		{name: "stalled pending", item: &model.LinkTranslation{Status: model.TranslationStatusPending, UpdatedAt: now.Add(-3 * time.Hour)}},
		{name: "fresh processing", item: &model.LinkTranslation{Status: model.TranslationStatusProcessing, UpdatedAt: now.Add(-time.Minute)}, reusable: true},
		{name: "done", item: &model.LinkTranslation{Status: model.TranslationStatusDone}, reusable: true},
		{name: "forced done", item: &model.LinkTranslation{Status: model.TranslationStatusDone}, force: true},
		{name: "failed", item: &model.LinkTranslation{Status: model.TranslationStatusFailed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := reusableTranslation(tc.item, tc.force, now, stallAfter); got != tc.reusable {
				t.Fatalf("reusableTranslation() = %v, want %v", got, tc.reusable)
			}
		})
	}
}

func ptr[T any](value T) *T { return &value }
