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
	seed model.TranslationAttemptSeed,
) (int64, error) {
	jobID, err := q.delegate.EnqueueTranslationTx(ctx, tx, seed)
	if err != nil {
		return 0, err
	}
	q.translationID = seed.TranslationID
	q.riverJobID = jobID
	return 0, q.err
}

func TestTranslationSourceScheduleRejectsRevisionChangedAcrossClientBarrier(t *testing.T) {
	harness := newRF5AScheduleHarness(t)
	fixture := harness.createSource(t, "stale-revision", model.SavedContent{
		Text: "Alpha beta", Format: model.ContentFormatPlain, Words: 2,
	}, "Stable summary")
	service := harness.service(harness.queue)

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
	currentRevision, stored, err := harness.links.ReplaceContentIfCurrentWithRevision(
		fixture.ctx,
		fixture.linkID,
		fixture.updatedAt,
		observedRevision,
		model.SavedContent{Text: "Alpha beta replaced", Format: model.ContentFormatPlain, Words: 3},
	)
	if err != nil || !stored || currentRevision != observedRevision+1 {
		t.Fatalf("ReplaceContentIfCurrentWithRevision() = revision %d, stored %v, error %v; want %d/true",
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
	harness := newRF5AScheduleHarness(t)
	oldSummary := "**Alpha** beta"
	fixture := harness.createSource(t, "stale-summary", model.SavedContent{
		Text: "Saved body", Format: model.ContentFormatPlain, Words: 2,
	}, oldSummary)
	service := harness.service(harness.queue)
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
	harness := newRF5AScheduleHarness(t)
	service := harness.service(harness.queue)

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
	harness := newRF5AScheduleHarness(t)
	fixture := harness.createSource(t, "river-rollback", model.SavedContent{
		Text: "Alpha beta", Format: model.ContentFormatPlain, Words: 2,
	}, "Alpha beta")
	fault := errors.New("injected failure after real River InsertTx")
	queue := &rf5aFaultAfterRiverInsertQueue{delegate: harness.queue, err: fault}
	service := harness.service(queue)

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
	harness := newRF5AScheduleHarness(t)
	fixture := harness.createSource(t, "contract-distinct-revisions", model.SavedContent{
		Text: "Alpha beta", Format: model.ContentFormatPlain, Words: 2,
	}, "Alpha beta")
	service := harness.service(harness.queue)

	first, err := service.Create(fixture.ctx, fixture.linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "Alpha",
		ExpectedContentRevision: &fixture.revision,
	})
	if err != nil || first == nil {
		t.Fatalf("Create(revision %d) = %+v, %v", fixture.revision, first, err)
	}
	secondRevision, stored, err := harness.links.ReplaceContentIfCurrentWithRevision(
		fixture.ctx,
		fixture.linkID,
		fixture.updatedAt,
		fixture.revision,
		model.SavedContent{Text: "Alpha beta revised", Format: model.ContentFormatPlain, Words: 3},
	)
	if err != nil || !stored || secondRevision != fixture.revision+1 {
		t.Fatalf("ReplaceContentIfCurrentWithRevision() = revision %d/stored %v/error %v", secondRevision, stored, err)
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
	harness := newRF5AScheduleHarness(t)
	fixture := harness.createSource(t, "identity-roundtrip", model.SavedContent{
		Text: "Alpha beta", Format: model.ContentFormatPlain, Words: 2,
	}, "Alpha beta")
	service := harness.service(harness.queue)

	response, err := service.Create(fixture.ctx, fixture.linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "content",
		StartOffset: 0, EndOffset: 5, SourceText: "Alpha",
		ExpectedContentRevision: &fixture.revision,
	})
	if err != nil || response == nil {
		t.Fatalf("Create(identity roundtrip) = %+v, %v", response, err)
	}
	if response.LinkID != fixture.linkID ||
		response.SourceContentRevision == nil || *response.SourceContentRevision != fixture.revision ||
		response.AttemptGeneration != 1 {
		t.Fatalf("response identity = %+v, want link %s/revision %d/generation 1",
			response, fixture.linkID, fixture.revision)
	}
	persisted, err := harness.translations.GetByID(fixture.ctx, response.ID)
	if err != nil || persisted == nil || persisted.ID != response.ID ||
		persisted.SourceHash != response.SourceHash ||
		persisted.SourceContentRevision == nil || *persisted.SourceContentRevision != *response.SourceContentRevision ||
		persisted.AttemptGeneration != response.AttemptGeneration {
		t.Fatalf("persisted current attempt = %+v, %v; response=%+v", persisted, err, response)
	}

	var riverID int64
	var kind, translationArg, generationArg, sourceHashArg, sourceRevisionArg, state string
	if err := harness.pool.QueryRow(t.Context(), `SELECT id, kind,
		args->>'translation_id', args->>'attempt_generation', args->>'source_hash',
		args->>'source_content_revision', state::text
		FROM river_job
		WHERE kind=$1 AND args->>'translation_id'=$2`, model.TranslationJobKind, response.ID.String()).Scan(
		&riverID, &kind, &translationArg, &generationArg,
		&sourceHashArg, &sourceRevisionArg, &state,
	); err != nil {
		t.Fatalf("read current River attempt: %v", err)
	}
	if riverID <= 0 || kind != model.TranslationJobKind ||
		translationArg != response.ID.String() ||
		generationArg != strconv.FormatInt(response.AttemptGeneration, 10) ||
		sourceHashArg != response.SourceHash ||
		sourceRevisionArg != strconv.FormatInt(*response.SourceContentRevision, 10) || state != "available" {
		t.Fatalf("River attempt = id %d/kind %s/translation %s/generation %s/hash %s/revision %s/state %s; response=%+v",
			riverID, kind, translationArg, generationArg, sourceHashArg, sourceRevisionArg, state, response)
	}
}

func newRF5AScheduleHarness(t *testing.T) *rf5aScheduleHarness {
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

func (h *rf5aScheduleHarness) service(queue durablework.TranslationQueue) *linktranslation.Service {
	scheduler := durablework.NewTranslationScheduler(durablework.TranslationSchedulerOptions{
		Transactions: h.pool,
		Products:     h.translations,
		Queue:        queue,
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
	status, ok := httperr.As(err)
	code, coded := status.(httperr.ErrorCoder)
	if !ok || !coded || status.HTTPStatus() != wantStatus || code.HTTPErrorCode() != wantCode {
		t.Fatalf("schedule error = %v, want HTTP %d/%s", err, wantStatus, wantCode)
	}
	if identityProvider, present := status.(httperr.CurrentIdentityProvider); present {
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
		WHERE kind = $1`, model.TranslationJobKind).Scan(&jobs); err != nil {
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
		WHERE kind = $1 AND args->>'translation_id' IN (
			SELECT id::text FROM link_translations WHERE link_id=$2
		)`, model.TranslationJobKind, linkID).Scan(&jobs); err != nil {
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
