package dbintegration

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/concept"
	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

func TestStaleReadingParseCompletesLifecycleWithoutOverwritingReaderMetadata(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)
	jobs := repository.NewPGXJobRepository(pool)
	reader := repository.NewPGXReaderVNextRepository(pool)
	linkID := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/stale-terminal-parser", "Original parser title", "body", "Original parser summary")
	setTags(t, pool, linkID, []string{"legacy-parser"})
	before := linkMetadataReadLink(t, links, ctx, linkID)

	job, err := jobs.Create(ctx, linkID)
	if err != nil {
		t.Fatalf("Create parse job: %v", err)
	}
	if job.ExpectedMetadataRevision <= 0 || job.ExpectedMetadataRevision != before.MetadataRevision {
		t.Fatalf("job expected metadata revision = %d, want live revision %d", job.ExpectedMetadataRevision, before.MetadataRevision)
	}
	if err := jobs.UpdateState(ctx, repository.UpdateJobStateParams{ID: job.ID, Status: model.JobStatusProcessing}); err != nil {
		t.Fatalf("mark parse job processing: %v", err)
	}

	replacementTitle := "Reader replacement title"
	replacementSummary := "Reader replacement summary"
	replacementTags := []string{"reader-replacement", "current"}
	replacement, err := reader.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{
		LinkID:           linkID,
		Title:            &replacementTitle,
		Summary:          &replacementSummary,
		Tags:             replacementTags,
		ExpectedRevision: job.ExpectedMetadataRevision,
	})
	if err != nil {
		t.Fatalf("UpdateLinkMetadata: %v", err)
	}
	if replacement.MetadataRevision != job.ExpectedMetadataRevision+1 || !replacement.TagsChanged {
		t.Fatalf("metadata replacement = %#v, want revision %d with changed tags", replacement, job.ExpectedMetadataRevision+1)
	}

	parserTitle := "Stale parser title"
	parserSummary := "Stale parser summary"
	completion, err := links.CompleteReadingParse(ctx, repository.CompleteReadingParseParams{
		Analysis: repository.UpdateLinkAnalysisParams{
			ID:                       linkID,
			ExpectedMetadataRevision: job.ExpectedMetadataRevision,
			Title:                    &parserTitle,
			Summary:                  &parserSummary,
			Tags:                     []string{"stale-parser"},
			Status:                   model.LinkStatusDone,
		},
		Classification: repository.UpdateLibraryClassificationParams{
			ID: linkID, Kind: model.LibraryKindReading, Source: model.LibraryKindSourceAuto,
		},
		ExpectedRequestedLibraryKind:       model.RequestedLibraryKindAuto,
		ExpectedRequestedLibraryKindSource: model.RequestedLibraryKindSourceAuto,
	}, job.ID)
	if err != nil {
		t.Fatalf("CompleteReadingParse: %v", err)
	}
	if completion.MetadataApplied || completion.MetadataRevision != replacement.MetadataRevision {
		t.Fatalf("stale completion = %#v, want metadata unchanged at revision %d", completion, replacement.MetadataRevision)
	}

	after := linkMetadataReadLink(t, links, ctx, linkID)
	if after.Title == nil || *after.Title != replacementTitle ||
		after.Summary == nil || *after.Summary != replacementSummary ||
		!slices.Equal(after.Tags, replacementTags) ||
		after.MetadataRevision != replacement.MetadataRevision {
		t.Fatalf("stale parser replaced Reader metadata: %#v", after)
	}
	if after.Status != model.LinkStatusDone ||
		after.LibraryKind == nil || *after.LibraryKind != model.LibraryKindReading ||
		after.LibraryKindSource == nil || *after.LibraryKindSource != model.LibraryKindSourceAuto {
		t.Fatalf("terminal lifecycle/classification = %#v, want done automatic reading", after)
	}
	completedJob, err := jobs.GetByID(ctx, job.ID)
	if err != nil || completedJob == nil {
		t.Fatalf("GetByID completed job = %#v, %v", completedJob, err)
	}
	if completedJob.Status != model.JobStatusDone {
		t.Fatalf("completed job status = %q, want done", completedJob.Status)
	}
}

func TestStaleConceptAttachmentDoesNotOutliveReaderMetadataCAS(t *testing.T) {
	pool := StartPostgres(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	links := repository.NewPGXLinkRepository(pool)
	concepts := repository.NewPGXConceptRepository(pool)
	linkID := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/stale-concept-attachment", "Original title", "body", "Original summary")
	legacyTags := []string{"legacy-concept-tag"}
	setTags(t, pool, linkID, legacyTags)
	before := linkMetadataReadLink(t, links, ctx, linkID)

	conceptID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO concept
		(id,primary_name,display_name)
		VALUES ($1,'legacy-concept','Legacy canonical display')`, conceptID); err != nil {
		t.Fatalf("insert concept fixture: %v", err)
	}

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin Link blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	var lockedID uuid.UUID
	if err := blocker.QueryRow(ctx, `SELECT id FROM links WHERE id=$1 FOR UPDATE`, linkID).Scan(&lockedID); err != nil {
		t.Fatalf("lock Link fixture: %v", err)
	}
	// Queue both participants behind the installation activity fence. The Reader
	// CAS is started and observed first, so it owns the fence through its Link
	// mutation and commit; the delayed concept attach cannot even evaluate the
	// Link tuple until that CAS has published the replacement.
	activityGate, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire Reader activity gate: %v", err)
	}
	if _, err := activityGate.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('reader-activity',0))`); err != nil {
		activityGate.Release()
		t.Fatalf("hold Reader activity gate: %v", err)
	}
	activityGateReleased := false
	releaseActivityGate := func() {
		if activityGateReleased {
			return
		}
		activityGateReleased = true
		if _, err := activityGate.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended('reader-activity',0))`); err != nil {
			t.Errorf("release Reader activity gate: %v", err)
		}
		activityGate.Release()
	}
	t.Cleanup(releaseActivityGate)

	type metadataResult struct {
		update model.ReaderLinkMetadataUpdate
		err    error
	}
	type attachmentResult struct {
		attached bool
		err      error
	}

	replacementTitle := "Reader wins the concept race"
	replacementSummary := "Reader replacement survives stale concept enrichment"
	replacementTags := []string{"reader-current-tag"}
	readerApplication := "webtag_reader_metadata_fence_" + uuid.NewString()
	readerPool := openNamedPool(t, readerApplication)
	readerCAS := repository.NewPGXReaderVNextRepository(readerPool)
	metadataDone := make(chan metadataResult, 1)
	go func() {
		update, updateErr := readerCAS.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{
			LinkID:           linkID,
			Title:            &replacementTitle,
			Summary:          &replacementSummary,
			Tags:             replacementTags,
			ExpectedRevision: before.MetadataRevision,
		})
		metadataDone <- metadataResult{update: update, err: updateErr}
	}()
	waitForPostgresLock(t, ctx, pool, readerApplication)

	attachmentApplication := "webtag_stale_concept_attachment_" + uuid.NewString()
	attachmentPool := openNamedPool(t, attachmentApplication)
	attachmentDone := make(chan attachmentResult, 1)
	go func() {
		tx, beginErr := attachmentPool.Begin(ctx)
		if beginErr != nil {
			attachmentDone <- attachmentResult{err: beginErr}
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, lockErr := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('reader-activity',0))`); lockErr != nil {
			attachmentDone <- attachmentResult{err: lockErr}
			return
		}
		staleConcepts := repository.NewPGXConceptRepository(tx)
		attached, attachErr := staleConcepts.AttachLinkConceptsBatch(ctx, linkID, before.MetadataRevision, legacyTags, []concept.AttachLinkConceptItem{{
			ConceptID:  conceptID,
			SurfaceTag: "legacy-concept-tag",
		}})
		if attachErr == nil {
			attachErr = tx.Commit(ctx)
		}
		attachmentDone <- attachmentResult{attached: attached, err: attachErr}
	}()
	waitForPostgresLock(t, ctx, pool, attachmentApplication)
	releaseActivityGate()

	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("release Link blocker: %v", err)
	}

	var metadata metadataResult
	select {
	case metadata = <-metadataDone:
	case <-ctx.Done():
		t.Fatalf("Reader metadata CAS did not finish: %v", ctx.Err())
	}
	if metadata.err != nil {
		t.Fatalf("Reader metadata CAS: %v", metadata.err)
	}
	if metadata.update.MetadataRevision != before.MetadataRevision+1 || !metadata.update.TagsChanged {
		t.Fatalf("Reader metadata CAS result = %#v, want revision %d with changed tags", metadata.update, before.MetadataRevision+1)
	}

	var attachment attachmentResult
	select {
	case attachment = <-attachmentDone:
	case <-ctx.Done():
		t.Fatalf("stale concept attachment did not finish: %v", ctx.Err())
	}
	if attachment.err != nil {
		t.Fatalf("AttachLinkConceptsBatch: %v", attachment.err)
	}
	if attachment.attached {
		t.Fatal("stale concept attachment reported applied after Reader metadata CAS")
	}

	displayNames, err := concepts.ListDisplayNamesByLinkIDs(ctx, []uuid.UUID{linkID})
	if err != nil {
		t.Fatalf("ListDisplayNamesByLinkIDs: %v", err)
	}
	if names := displayNames[linkID]; len(names) != 0 {
		t.Fatalf("stale concept edge remains visible as display names %q", names)
	}

	read := service.NewLinkReadService(service.LinkReadServiceOptions{Links: links, ConceptDisplay: concepts})
	detail, err := read.GetWithContent(ctx, linkID.String(), false)
	if err != nil {
		t.Fatalf("GetWithContent: %v", err)
	}
	if detail.Title == nil || *detail.Title != replacementTitle ||
		detail.Summary == nil || *detail.Summary != replacementSummary ||
		!slices.Equal(detail.Tags, replacementTags) {
		t.Fatalf("detail metadata after stale concept race = %#v", detail)
	}
	list, err := read.List(ctx, dto.ListLinksRequest{Limit: 20})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, item := range list.Items {
		if item.ID != linkID.String() {
			continue
		}
		if item.Title == nil || *item.Title != replacementTitle ||
			item.Summary == nil || *item.Summary != replacementSummary ||
			!slices.Equal(item.Tags, replacementTags) {
			t.Fatalf("list metadata after stale concept race = %#v", item)
		}
		return
	}
	t.Fatalf("List did not return raced link %s: %#v", linkID, list.Items)
}
