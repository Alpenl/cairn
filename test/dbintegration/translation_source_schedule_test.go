package dbintegration

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/app/durablework"
	"webtag/internal/contentdoc"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service/linktranslation"
)

type rf5aScheduleHarness struct {
	pool         *pgxpool.Pool
	links        *repository.PGXLinkRepository
	translations *repository.PGXTranslationRepository
	queue        durablework.TranslationQueue
}

type rf5aSourceFixture struct {
	ctx       context.Context
	linkID    uuid.UUID
	revision  int64
	updatedAt time.Time
	summary   string
}

type rf5aCreateResult struct {
	item *model.LinkTranslation
	err  error
}

type rf5aFaultAfterRiverInsertQueue struct {
	delegate      durablework.TranslationQueue
	err           error
	translationID uuid.UUID
	riverJobID    int64
}

func (q *rf5aFaultAfterRiverInsertQueue) EnqueueTranslationTx(
	ctx context.Context,
	tx pgx.Tx,
	command model.TranslationScheduleCommand,
) (int64, error) {
	jobID, err := q.delegate.EnqueueTranslationTx(ctx, tx, command)
	if err != nil {
		return 0, err
	}
	q.translationID = command.Seed.TranslationID
	q.riverJobID = jobID
	return 0, q.err
}

func (q *rf5aFaultAfterRiverInsertQueue) TranslationJobsRollout() model.TranslationJobsRolloutStage {
	return q.delegate.TranslationJobsRollout()
}

func TestTranslationSourceScheduleRejectsRevisionChangedAcrossClientBarrier(t *testing.T) {
	harness := newRF5AScheduleHarness(t, true)
	fixture := harness.createSource(t, "stale-revision", model.SavedContent{
		Text: "Alpha beta", Format: model.ContentFormatPlain, Words: 2,
	}, "Stable summary")
	service := harness.service(true, harness.queue)

	observed := make(chan int64, 1)
	release := make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	result := make(chan rf5aCreateResult, 1)
	go func() {
		content, err := harness.links.GetContent(fixture.ctx, fixture.linkID)
		if err != nil || content == nil {
			result <- rf5aCreateResult{err: fmt.Errorf("observe saved source: content=%+v: %w", content, err)}
			return
		}
		observed <- content.Revision
		<-release
		item, createErr := service.Create(fixture.ctx, fixture.linkID, model.TranslationRequest{
			Scope: model.TranslationScopeSelection, BlockKey: "content",
			StartOffset: 0, EndOffset: 5, SourceText: "Alpha",
			ExpectedContentRevision: &content.Revision,
		})
		result <- rf5aCreateResult{item: item, err: createErr}
	}()

	var observedRevision int64
	select {
	case observedRevision = <-observed:
	case early := <-result:
		t.Fatalf("source observation failed before barrier: %v", early.err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for saved-source observation barrier")
	}
	if observedRevision != fixture.revision {
		t.Fatalf("observed content revision = %d, want fixture revision %d", observedRevision, fixture.revision)
	}
	currentRevision, stored, err := harness.links.ReplaceContentIfCurrent(
		fixture.ctx,
		fixture.linkID,
		fixture.updatedAt,
		model.SavedContent{Text: "Alpha beta replaced", Format: model.ContentFormatPlain, Words: 3},
	)
	if err != nil || !stored || currentRevision != observedRevision+1 {
		t.Fatalf("ReplaceContentIfCurrent() = revision %d, stored %v, error %v; want %d/true",
			currentRevision, stored, err, observedRevision+1)
	}
	close(release)
	released = true

	var create rf5aCreateResult
	select {
	case create = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stale schedule result after barrier release")
	}
	if create.item != nil {
		t.Fatalf("stale Create() returned product %+v", create.item)
	}
	identity := assertRF5AScheduleError(
		t,
		create.err,
		http.StatusConflict,
		httperr.CodeContentRevisionConflict,
	)
	if identity.ContentRevision == nil || *identity.ContentRevision != currentRevision || identity.BlockKey != "content" {
		t.Fatalf("current identity = %+v, want content revision %d/block content", identity, currentRevision)
	}
	assertRF5ANoScheduledWork(t, harness.pool, fixture.linkID)
}

func TestTranslationSourceScheduleRejectsSummaryChangedWithoutSavedRevisionBump(t *testing.T) {
	harness := newRF5AScheduleHarness(t, true)
	oldSummary := "**Alpha** beta"
	fixture := harness.createSource(t, "stale-summary", model.SavedContent{
		Text: "Saved body", Format: model.ContentFormatPlain, Words: 2,
	}, oldSummary)
	service := harness.service(true, harness.queue)
	oldHash := rf5aRenderedHash(t, oldSummary)

	newSummary := "**Gamma** delta"
	if err := harness.links.UpdateAnalysis(fixture.ctx, repository.UpdateLinkAnalysisParams{
		ID: fixture.linkID, Summary: &newSummary, Status: model.LinkStatusDone,
	}); err != nil {
		t.Fatalf("UpdateAnalysis(new summary): %v", err)
	}
	current, err := harness.links.GetByID(fixture.ctx, fixture.linkID)
	if err != nil || current == nil || current.ContentRevision != fixture.revision {
		t.Fatalf("summary update changed saved identity: link=%+v error=%v; want revision %d",
			current, err, fixture.revision)
	}

	item, err := service.Create(fixture.ctx, fixture.linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "summary",
		StartOffset: 0, EndOffset: 5, SourceText: "Alpha", ExpectedSourceHash: &oldHash,
	})
	if item != nil {
		t.Fatalf("stale summary Create() returned product %+v", item)
	}
	identity := assertRF5AScheduleError(t, err, http.StatusConflict, httperr.CodeSourceBlockConflict)
	newHash := rf5aRenderedHash(t, newSummary)
	if identity.SourceHash == nil || *identity.SourceHash != newHash || identity.BlockKey != "summary" ||
		identity.ContentRevision != nil {
		t.Fatalf("current summary identity = %+v, want hash %s/block summary and no saved revision",
			identity, newHash)
	}
	assertRF5ANoScheduledWork(t, harness.pool, fixture.linkID)
}

func TestTranslationSourceScheduleRejectsCanonicalTextAndOffsetMismatches(t *testing.T) {
	harness := newRF5AScheduleHarness(t, true)
	service := harness.service(true, harness.queue)

	for _, tc := range []struct {
		name        string
		blockKey    string
		startOffset int
		endOffset   int
		sourceText  string
	}{
		{name: "content text", blockKey: "content", startOffset: 0, endOffset: 5, sourceText: "Gamma"},
		{name: "content offsets", blockKey: "content", startOffset: 1, endOffset: 6, sourceText: "Alpha"},
		{name: "summary text", blockKey: "summary", startOffset: 0, endOffset: 5, sourceText: "Gamma"},
		{name: "summary offsets", blockKey: "summary", startOffset: 1, endOffset: 6, sourceText: "Alpha"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := harness.createSource(t, "mismatch-"+strings.ReplaceAll(tc.name, " ", "-"), model.SavedContent{
				Text: "Alpha beta", Format: model.ContentFormatPlain, Words: 2,
			}, "Alpha beta")
			req := model.TranslationRequest{
				Scope: model.TranslationScopeSelection, BlockKey: tc.blockKey,
				StartOffset: tc.startOffset, EndOffset: tc.endOffset, SourceText: tc.sourceText,
			}
			if tc.blockKey == "summary" {
				hash := rf5aRenderedHash(t, fixture.summary)
				req.ExpectedSourceHash = &hash
			} else {
				req.ExpectedContentRevision = &fixture.revision
			}

			item, err := service.Create(fixture.ctx, fixture.linkID, req)
			if item != nil {
				t.Fatalf("mismatched Create() returned product %+v", item)
			}
			identity := assertRF5AScheduleError(t, err, http.StatusConflict, httperr.CodeSourceBlockConflict)
			if identity.BlockKey != tc.blockKey {
				t.Fatalf("mismatch current identity = %+v, want block %s", identity, tc.blockKey)
			}
			if tc.blockKey == "content" && (identity.ContentRevision == nil ||
				*identity.ContentRevision != fixture.revision || identity.SourceHash != nil) {
				t.Fatalf("saved mismatch current identity = %+v, want revision %d", identity, fixture.revision)
			}
			if tc.blockKey == "summary" && (identity.SourceHash == nil ||
				*identity.SourceHash != rf5aRenderedHash(t, fixture.summary) || identity.ContentRevision != nil) {
				t.Fatalf("summary mismatch current identity = %+v, want canonical source hash", identity)
			}
			assertRF5ANoScheduledWork(t, harness.pool, fixture.linkID)
		})
	}
}

func TestTranslationSourceScheduleRollsBackProductAttemptAndJobAfterRealRiverInsert(t *testing.T) {
	harness := newRF5AScheduleHarness(t, true)
	fixture := harness.createSource(t, "river-rollback", model.SavedContent{
		Text: "Alpha beta", Format: model.ContentFormatPlain, Words: 2,
	}, "Alpha beta")
	fault := errors.New("injected failure after real River InsertTx")
	queue := &rf5aFaultAfterRiverInsertQueue{delegate: harness.queue, err: fault}
	service := harness.service(true, queue)

	item, err := service.Create(fixture.ctx, fixture.linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "Alpha",
		ExpectedContentRevision: &fixture.revision,
	})
	if item != nil || !errors.Is(err, fault) || queue.translationID == uuid.Nil || queue.riverJobID <= 0 {
		t.Fatalf("Create(fault after insert) = item %+v/error %v/translation %s/job %d",
			item, err, queue.translationID, queue.riverJobID)
	}
	var productRows, jobRows int
	if err := harness.pool.QueryRow(t.Context(), `SELECT count(*) FROM link_translations WHERE id=$1`,
		queue.translationID).Scan(&productRows); err != nil {
		t.Fatalf("count rolled-back product: %v", err)
	}
	if err := harness.pool.QueryRow(t.Context(), `SELECT count(*) FROM river_job WHERE id=$1`,
		queue.riverJobID).Scan(&jobRows); err != nil {
		t.Fatalf("count rolled-back River job: %v", err)
	}
	if productRows != 0 || jobRows != 0 {
		t.Fatalf("rolled-back identity retained product/job rows %d/%d, want 0/0", productRows, jobRows)
	}
	assertRF5ANoScheduledWork(t, harness.pool, fixture.linkID)
}

func TestTranslationSourceScheduleContractKeepsSameHashAcrossSavedRevisionsDistinct(t *testing.T) {
	harness := newRF5AScheduleHarness(t, true)
	fixture := harness.createSource(t, "contract-distinct-revisions", model.SavedContent{
		Text: "Alpha beta", Format: model.ContentFormatPlain, Words: 2,
	}, "Alpha beta")
	service := harness.service(true, harness.queue)

	first, err := service.Create(fixture.ctx, fixture.linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "Alpha",
		ExpectedContentRevision: &fixture.revision,
	})
	if err != nil || first == nil {
		t.Fatalf("Create(revision %d) = %+v, %v", fixture.revision, first, err)
	}
	secondRevision, stored, err := harness.links.ReplaceContentIfCurrent(
		fixture.ctx,
		fixture.linkID,
		fixture.updatedAt,
		model.SavedContent{Text: "Alpha beta revised", Format: model.ContentFormatPlain, Words: 3},
	)
	if err != nil || !stored || secondRevision != fixture.revision+1 {
		t.Fatalf("ReplaceContentIfCurrent() = revision %d/stored %v/error %v", secondRevision, stored, err)
	}
	second, err := service.Create(fixture.ctx, fixture.linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "Alpha",
		ExpectedContentRevision: &secondRevision,
	})
	if err != nil || second == nil {
		t.Fatalf("Create(revision %d) = %+v, %v", secondRevision, second, err)
	}

	wantHash := rf5aPlainHash("Alpha")
	if first.ID == second.ID || first.SourceHash != wantHash || second.SourceHash != wantHash ||
		first.SourceContentRevision == nil || *first.SourceContentRevision != fixture.revision ||
		second.SourceContentRevision == nil || *second.SourceContentRevision != secondRevision {
		t.Fatalf("contract identities = first %+v/second %+v; want distinct rows, hash %s, revisions %d/%d",
			first, second, wantHash, fixture.revision, secondRevision)
	}
	assertRF5AScheduledWorkCounts(t, harness.pool, fixture.linkID, 2, 2)
	var revisions, hashes, productIDs int
	if err := harness.pool.QueryRow(t.Context(), `SELECT
		count(DISTINCT source_content_revision), count(DISTINCT source_hash), count(DISTINCT id)
		FROM link_translations WHERE link_id=$1`, fixture.linkID).Scan(&revisions, &hashes, &productIDs); err != nil {
		t.Fatalf("read contract identity cardinality: %v", err)
	}
	if revisions != 2 || hashes != 1 || productIDs != 2 {
		t.Fatalf("contract cardinality = revisions %d/hashes %d/products %d, want 2/1/2",
			revisions, hashes, productIDs)
	}
}

func TestTranslationSourceScheduleSuccessKeepsResponseProductAttemptAndRiverArgsIdentical(t *testing.T) {
	harness := newRF5AScheduleHarness(t, true)
	fixture := harness.createSource(t, "identity-roundtrip", model.SavedContent{
		Text: "Alpha beta", Format: model.ContentFormatPlain, Words: 2,
	}, "Alpha beta")
	service := harness.service(true, harness.queue)

	response, err := service.Create(fixture.ctx, fixture.linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "Alpha",
		ExpectedContentRevision: &fixture.revision,
	})
	if err != nil || response == nil || response.CurrentRiverJobID == nil {
		t.Fatalf("Create(identity roundtrip) = %+v, %v", response, err)
	}
	if response.LinkID != fixture.linkID ||
		response.SourceContentRevision == nil || *response.SourceContentRevision != fixture.revision ||
		response.AttemptGeneration != 1 || *response.CurrentRiverJobID <= 0 {
		t.Fatalf("response identity = %+v, want link %s/revision %d/generation 1",
			response, fixture.linkID, fixture.revision)
	}
	persisted, err := harness.translations.GetByID(fixture.ctx, response.ID)
	if err != nil || persisted == nil || persisted.ID != response.ID ||
		persisted.SourceHash != response.SourceHash ||
		persisted.SourceContentRevision == nil || *persisted.SourceContentRevision != *response.SourceContentRevision ||
		persisted.AttemptGeneration != response.AttemptGeneration || persisted.CurrentRiverJobID == nil ||
		*persisted.CurrentRiverJobID != *response.CurrentRiverJobID {
		t.Fatalf("persisted current attempt = %+v, %v; response=%+v", persisted, err, response)
	}

	var riverID int64
	var kind, translationArg, generationArg, sourceHashArg, sourceRevisionArg, state string
	if err := harness.pool.QueryRow(t.Context(), `SELECT id, kind,
		args->>'translation_id', args->>'attempt_generation', args->>'source_hash',
		args->>'source_content_revision', state::text
		FROM river_job WHERE id=$1`, *response.CurrentRiverJobID).Scan(
		&riverID, &kind, &translationArg, &generationArg,
		&sourceHashArg, &sourceRevisionArg, &state,
	); err != nil {
		t.Fatalf("read current River attempt: %v", err)
	}
	if riverID != *response.CurrentRiverJobID || kind != model.TranslationJobKindV2 ||
		translationArg != response.ID.String() ||
		generationArg != strconv.FormatInt(response.AttemptGeneration, 10) ||
		sourceHashArg != response.SourceHash ||
		sourceRevisionArg != strconv.FormatInt(*response.SourceContentRevision, 10) || state != "available" {
		t.Fatalf("River attempt = id %d/kind %s/translation %s/generation %s/hash %s/revision %s/state %s; response=%+v",
			riverID, kind, translationArg, generationArg, sourceHashArg, sourceRevisionArg, state, response)
	}
}

func TestContentHistoryRestorePreservesTranslationHistoryAndAttemptIdentity(t *testing.T) {
	harness := newRF5AScheduleHarness(t, true)
	fixture := harness.createSource(t, "restore-translation-history", model.SavedContent{
		Text: "Alpha beta", Format: model.ContentFormatPlain, Words: 2,
	}, "Alpha beta")
	service := harness.service(true, harness.queue)

	createSaved := func(sourceText string, start, end int) *model.LinkTranslation {
		t.Helper()
		item, err := service.Create(fixture.ctx, fixture.linkID, model.TranslationRequest{
			Scope: model.TranslationScopeSelection, BlockKey: "content",
			StartOffset: start, EndOffset: end, SourceText: sourceText,
			ExpectedContentRevision: &fixture.revision,
		})
		if err != nil || item == nil || item.CurrentRiverJobID == nil {
			t.Fatalf("Create(saved %q) = %+v, %v", sourceText, item, err)
		}
		return item
	}
	oldAlpha := createSaved("Alpha", 0, 5)
	oldBeta := createSaved("beta", 6, 10)
	oldAlphaAttempt := model.TranslationAttempt{
		TranslationID:     oldAlpha.ID,
		AttemptGeneration: oldAlpha.AttemptGeneration, RiverJobID: *oldAlpha.CurrentRiverJobID,
		SourceHash: oldAlpha.SourceHash, SourceContentRevision: oldAlpha.SourceContentRevision,
	}
	oldBetaAttempt := model.TranslationAttempt{
		TranslationID:     oldBeta.ID,
		AttemptGeneration: oldBeta.AttemptGeneration, RiverJobID: *oldBeta.CurrentRiverJobID,
		SourceHash: oldBeta.SourceHash, SourceContentRevision: oldBeta.SourceContentRevision,
	}
	for _, attempt := range []model.TranslationAttempt{oldAlphaAttempt, oldBetaAttempt} {
		item, err := harness.translations.MarkProcessing(fixture.ctx, attempt)
		if err != nil || item == nil || item.Status != model.TranslationStatusProcessing {
			t.Fatalf("MarkProcessing(%s) = %+v, %v", attempt.TranslationID, item, err)
		}
	}

	summaryHash := rf5aRenderedHash(t, fixture.summary)
	summary, err := service.Create(fixture.ctx, fixture.linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "summary",
		StartOffset: 0, EndOffset: 5, SourceText: "Alpha", ExpectedSourceHash: &summaryHash,
	})
	if err != nil || summary == nil || summary.CurrentRiverJobID == nil {
		t.Fatalf("Create(summary) = %+v, %v", summary, err)
	}
	summaryAttempt := model.TranslationAttempt{
		TranslationID:     summary.ID,
		AttemptGeneration: summary.AttemptGeneration, RiverJobID: *summary.CurrentRiverJobID,
		SourceHash: summary.SourceHash, SourceContentRevision: summary.SourceContentRevision,
	}
	if item, err := harness.translations.MarkProcessing(fixture.ctx, summaryAttempt); err != nil || item == nil {
		t.Fatalf("MarkProcessing(summary) = %+v, %v", item, err)
	}
	if applied, err := harness.translations.Complete(fixture.ctx, summaryAttempt, "摘要译文", "test-model"); err != nil || !applied {
		t.Fatalf("Complete(summary) = %v, %v", applied, err)
	}

	legacyService := harness.service(false, harness.queue)
	legacy, err := legacyService.Create(fixture.ctx, fixture.linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 6, EndOffset: 10, SourceText: "beta",
	})
	if err != nil || legacy == nil || legacy.SourceContentRevision != nil {
		t.Fatalf("Create(legacy) = %+v, %v", legacy, err)
	}
	revisionBeforeRestore, stored, err := harness.links.ReplaceContentIfCurrent(
		fixture.ctx, fixture.linkID, fixture.updatedAt,
		model.SavedContent{Text: "Gamma delta", Format: model.ContentFormatPlain, Words: 2},
	)
	if err != nil || !stored || revisionBeforeRestore != fixture.revision+1 {
		t.Fatalf("ReplaceContentIfCurrent() = revision %d, stored %v, error %v", revisionBeforeRestore, stored, err)
	}
	var historyID int64
	if err := harness.pool.QueryRow(t.Context(), `SELECT id FROM reader_content_history
		WHERE link_id=$1 AND revision=$2`, fixture.linkID, fixture.revision).Scan(&historyID); err != nil {
		t.Fatalf("read original content history: %v", err)
	}

	reader := repository.NewPGXReaderVNextRepository(harness.pool)
	type restoreResult struct {
		revision int64
		err      error
	}
	type terminalResult struct {
		name    string
		applied bool
		err     error
	}
	start := make(chan struct{})
	restored := make(chan restoreResult, 1)
	terminals := make(chan terminalResult, 2)
	go func() {
		<-start
		revision, restoreErr := reader.RestoreContentHistory(fixture.ctx, fixture.linkID, historyID, revisionBeforeRestore)
		restored <- restoreResult{revision: revision, err: restoreErr}
	}()
	go func() {
		<-start
		applied, completeErr := harness.translations.Complete(fixture.ctx, oldAlphaAttempt, "旧代次完成", "test-model")
		terminals <- terminalResult{name: "complete", applied: applied, err: completeErr}
	}()
	go func() {
		<-start
		applied, failErr := harness.translations.Fail(fixture.ctx, oldBetaAttempt, "旧代次失败")
		terminals <- terminalResult{name: "fail", applied: applied, err: failErr}
	}()
	close(start)
	restore := <-restored
	if restore.err != nil || restore.revision != revisionBeforeRestore+1 {
		t.Fatalf("RestoreContentHistory() = revision %d, error %v", restore.revision, restore.err)
	}
	for range 2 {
		terminal := <-terminals
		if terminal.err != nil || !terminal.applied {
			t.Fatalf("%s(old attempt racing restore) = %v, %v", terminal.name, terminal.applied, terminal.err)
		}
	}
	restoredRevision := restore.revision

	fresh, err := service.Create(fixture.ctx, fixture.linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "Alpha",
		ExpectedContentRevision: &restoredRevision,
	})
	if err != nil || fresh == nil || fresh.ID == oldAlpha.ID || fresh.SourceContentRevision == nil ||
		*fresh.SourceContentRevision != restoredRevision {
		t.Fatalf("Create(restored revision) = %+v, %v", fresh, err)
	}

	list, err := service.List(fixture.ctx, fixture.linkID)
	if err != nil || list.CurrentContentRevision != restoredRevision {
		t.Fatalf("List() = %+v, %v", list, err)
	}
	items := make(map[uuid.UUID]model.LinkTranslation, len(list.Items))
	for _, item := range list.Items {
		items[item.ID] = item
	}
	assertItem := func(id uuid.UUID, status model.TranslationStatus, stale bool) model.LinkTranslation {
		t.Helper()
		item, ok := items[id]
		if !ok || item.Status != status || item.Stale != stale {
			t.Fatalf("translation %s = %+v, want status=%s stale=%v", id, item, status, stale)
		}
		return item
	}
	assertItem(oldAlpha.ID, model.TranslationStatusDone, true)
	assertItem(oldBeta.ID, model.TranslationStatusFailed, true)
	assertItem(fresh.ID, model.TranslationStatusPending, false)
	assertItem(summary.ID, model.TranslationStatusDone, false)
	legacyItem := assertItem(legacy.ID, model.TranslationStatusPending, false)
	if legacyItem.SourceContentRevision != nil {
		t.Fatalf("legacy source revision = %v, want nil", *legacyItem.SourceContentRevision)
	}

	freshBeforeConflict, err := harness.translations.GetByID(fixture.ctx, fresh.ID)
	if err != nil || freshBeforeConflict == nil {
		t.Fatalf("GetByID(fresh before conflict) = %+v, %v", freshBeforeConflict, err)
	}
	_, err = reader.RestoreContentHistory(fixture.ctx, fixture.linkID, historyID, revisionBeforeRestore)
	if !errors.Is(err, repository.ErrRevisionConflict) {
		t.Fatalf("stale RestoreContentHistory() error = %v, want ErrRevisionConflict", err)
	}
	freshAfterConflict, err := harness.translations.GetByID(fixture.ctx, fresh.ID)
	if err != nil || freshAfterConflict == nil || freshAfterConflict.Status != freshBeforeConflict.Status ||
		freshAfterConflict.AttemptGeneration != freshBeforeConflict.AttemptGeneration ||
		freshAfterConflict.CurrentRiverJobID == nil || freshBeforeConflict.CurrentRiverJobID == nil ||
		*freshAfterConflict.CurrentRiverJobID != *freshBeforeConflict.CurrentRiverJobID ||
		freshAfterConflict.SourceContentRevision == nil || *freshAfterConflict.SourceContentRevision != restoredRevision {
		t.Fatalf("fresh attempt changed after failed restore: before=%+v after=%+v error=%v", freshBeforeConflict, freshAfterConflict, err)
	}
}

func newRF5AScheduleHarness(t *testing.T, _ bool) *rf5aScheduleHarness {
	t.Helper()
	pool := StartPostgres(t)

	links := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	return &rf5aScheduleHarness{
		pool:         pool,
		links:        links,
		translations: translations,
		queue:        newRiverQueue(t, pool, newRecordingProcessor(pool)),
	}
}

func (h *rf5aScheduleHarness) service(strict bool, queue durablework.TranslationQueue) *linktranslation.Service {
	scheduler := durablework.NewTranslationScheduler(durablework.TranslationSchedulerOptions{
		Transactions:         h.pool,
		Products:             h.translations,
		Queue:                queue,
		StrictSourceIdentity: strict,
	})
	return linktranslation.NewService(linktranslation.ServiceOptions{
		Translations:   h.translations,
		Scheduler:      scheduler,
		RequestTimeout: time.Second,
	})
}

func (h *rf5aScheduleHarness) createSource(
	t *testing.T,
	slug string,
	content model.SavedContent,
	summary string,
) rf5aSourceFixture {
	t.Helper()
	ctx := t.Context()
	rawURL := "https://rf5a-schedule.example.com/" + slug + "/" + uuid.NewString()
	link, err := h.links.Create(ctx, repository.CreateLinkParams{
		URL: rawURL, SourceKind: "url", SourceKey: rawURL, Status: model.LinkStatusDone,
	})
	if err != nil || link == nil {
		t.Fatalf("Create(%s) = %+v, %v", slug, link, err)
	}
	if err := h.links.UpdateAnalysis(ctx, repository.UpdateLinkAnalysisParams{
		ID: link.ID, Summary: &summary, Status: model.LinkStatusDone,
	}); err != nil {
		t.Fatalf("UpdateAnalysis(%s): %v", slug, err)
	}
	parsed, err := h.links.GetByID(ctx, link.ID)
	if err != nil || parsed == nil {
		t.Fatalf("GetByID(%s) = %+v, %v", slug, parsed, err)
	}
	revision, stored, err := h.links.UpdateContentIfCurrent(ctx, link.ID, parsed.UpdatedAt, content)
	if err != nil || !stored || revision <= 0 {
		t.Fatalf("UpdateContentIfCurrent(%s) = revision %d, stored %v, error %v", slug, revision, stored, err)
	}
	return rf5aSourceFixture{
		ctx: ctx, linkID: link.ID, revision: revision,
		updatedAt: parsed.UpdatedAt, summary: summary,
	}
}

func assertRF5AScheduleError(t *testing.T, err error, wantStatus int, wantCode string) httperr.ConflictIdentity {
	t.Helper()
	var status httperr.StatusCarrier
	var code httperr.ErrorCoder
	if !errors.As(err, &status) || status.HTTPStatus() != wantStatus ||
		!errors.As(err, &code) || code.HTTPErrorCode() != wantCode {
		t.Fatalf("schedule error = %v, want HTTP %d/%s", err, wantStatus, wantCode)
	}
	var identityProvider httperr.CurrentIdentityProvider
	if errors.As(err, &identityProvider) {
		if identity, ok := identityProvider.HTTPCurrentIdentity(); ok {
			return identity
		}
	}
	return httperr.ConflictIdentity{}
}

func assertRF5ANoScheduledWork(t *testing.T, pool *pgxpool.Pool, linkID uuid.UUID) {
	t.Helper()
	var products int
	var totalGeneration int64
	if err := pool.QueryRow(t.Context(), `SELECT count(*), COALESCE(sum(attempt_generation), 0)
		FROM link_translations WHERE link_id = $1`, linkID).Scan(&products, &totalGeneration); err != nil {
		t.Fatalf("read translation work for link %s: %v", linkID, err)
	}
	var jobs int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM river_job
		WHERE kind IN ($1, $2)`, model.TranslationJobKindLegacy, model.TranslationJobKindV2).Scan(&jobs); err != nil {
		t.Fatalf("count River translation jobs: %v", err)
	}
	if products != 0 || totalGeneration != 0 || jobs != 0 {
		t.Fatalf("scheduled work = products %d/generation %d/jobs %d, want 0/0/0",
			products, totalGeneration, jobs)
	}
}

func assertRF5AScheduledWorkCounts(
	t *testing.T,
	pool *pgxpool.Pool,
	linkID uuid.UUID,
	wantProducts int,
	wantJobs int,
) {
	t.Helper()
	var products, jobs int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM link_translations WHERE link_id=$1`,
		linkID).Scan(&products); err != nil {
		t.Fatalf("count translation products for link %s: %v", linkID, err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM river_job
		WHERE kind IN ($1, $2) AND args->>'translation_id' IN (
			SELECT id::text FROM link_translations WHERE link_id=$3
		)`, model.TranslationJobKindLegacy, model.TranslationJobKindV2, linkID).Scan(&jobs); err != nil {
		t.Fatalf("count River translation jobs for link %s: %v", linkID, err)
	}
	if products != wantProducts || jobs != wantJobs {
		t.Fatalf("scheduled work for link %s = products %d/jobs %d, want %d/%d",
			linkID, products, jobs, wantProducts, wantJobs)
	}
}

func rf5aRenderedHash(t *testing.T, markdown string) string {
	t.Helper()
	projection, err := contentdoc.RenderedBlockProjection(model.ContentFormatMarkdown, markdown)
	if err != nil {
		t.Fatalf("project RF5A summary %q: %v", markdown, err)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(projection)))
}

func rf5aPlainHash(text string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
}
