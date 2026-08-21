package dbintegration

import (
	"slices"
	"testing"

	"webtag/internal/model"
	"webtag/internal/repository"
)

func TestStaleReadingParseCompletesLifecycleWithoutOverwritingReaderMetadata(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)
	reader := repository.NewPGXReaderVNextRepository(pool)
	linkID := seedReaderVNextSavedLink(t, pool,
		"https://reader-vnext.example/stale-terminal-parser", "Original parser title", "body", "Original parser summary")
	setTags(t, pool, linkID, []string{"legacy-parser"})
	before := linkMetadataReadLink(t, links, ctx, linkID)

	attempt, err := requeueLinkForTest(ctx, pool, links, linkID, nil)
	if err != nil {
		t.Fatalf("requeue parse attempt: %v", err)
	}
	if attempt.ExpectedMetadataRevision <= 0 || attempt.ExpectedMetadataRevision != before.MetadataRevision {
		t.Fatalf("attempt expected metadata revision = %d, want live revision %d", attempt.ExpectedMetadataRevision, before.MetadataRevision)
	}
	// Keep the Link terminal while the Reader edit races with the queued
	// attempt. This is the state in which the Reader API accepts a user-owned
	// metadata edit; the worker moves the same generation back to processing
	// only after that edit commits.
	if err := links.UpdateState(ctx, repository.UpdateLinkStateParams{ID: linkID, Status: model.LinkStatusDone}); err != nil {
		t.Fatalf("restore terminal state for metadata edit: %v", err)
	}

	replacementTitle := "Reader replacement title"
	replacementSummary := "Reader replacement summary"
	replacementTags := []string{"reader-replacement", "current"}
	replacement, err := reader.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{
		LinkID:           linkID,
		Title:            &replacementTitle,
		Summary:          &replacementSummary,
		Tags:             replacementTags,
		ExpectedRevision: attempt.ExpectedMetadataRevision,
	})
	if err != nil {
		t.Fatalf("UpdateLinkMetadata: %v", err)
	}
	if replacement.MetadataRevision != attempt.ExpectedMetadataRevision+1 || !replacement.TagsChanged {
		t.Fatalf("metadata replacement = %#v, want revision %d with changed tags", replacement, attempt.ExpectedMetadataRevision+1)
	}
	if err := links.UpdateState(ctx, repository.UpdateLinkStateParams{ID: linkID, Status: model.LinkStatusPending}); err != nil {
		t.Fatalf("reopen parse attempt after metadata edit: %v", err)
	}
	if err := links.MarkParseProcessing(ctx, attempt); err != nil {
		t.Fatalf("mark parse attempt processing: %v", err)
	}

	parserTitle := "Stale parser title"
	parserSummary := "Stale parser summary"
	expectedKind := model.LibraryKindReading
	completion, err := links.CompleteReadingParse(ctx, repository.CompleteReadingParseParams{
		Analysis: repository.UpdateLinkAnalysisParams{
			ID:                       linkID,
			ExpectedParseGeneration:  attempt.Generation,
			ExpectedMetadataRevision: attempt.ExpectedMetadataRevision,
			Title:                    &parserTitle,
			Summary:                  &parserSummary,
			Tags:                     []string{"stale-parser"},
			Status:                   model.LinkStatusDone,
		},
		Classification: repository.UpdateLibraryClassificationParams{
			ID: linkID, Kind: model.LibraryKindReading,
		},
		ExpectedLibraryKind:       &expectedKind,
		ExpectedLibraryKindLocked: false,
	})
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
		after.LibraryKind == nil || *after.LibraryKind != model.LibraryKindReading || after.LibraryKindLocked {
		t.Fatalf("terminal lifecycle/classification = %#v, want done automatic reading", after)
	}
}
