package dbintegration

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

func TestTranslationSourceSchedulePrioritizesGenerationCASAfterSavedBodyTransitions(t *testing.T) {
	harness := newRF5AScheduleHarness(t, true)
	service := harness.service(true, harness.queue)

	for _, tc := range []struct {
		name         string
		initial      model.SavedContent
		request      func(int64) model.TranslationRequest
		transition   func(*testing.T, rf5aSourceFixture) int64
		wantBlockKey string
	}{
		{
			name: "capture requeue clears markdown saved body",
			initial: func() model.SavedContent {
				document := "# Captured source\n\nOld body."
				return model.SavedContent{
					Text: "Captured source old body.", Document: &document,
					Format: model.ContentFormatMarkdown, Words: 4,
				}
			}(),
			request: func(revision int64) model.TranslationRequest {
				return model.TranslationRequest{
					Scope: model.TranslationScopeFull, ExpectedContentRevision: &revision,
				}
			},
			transition: func(t *testing.T, fixture rf5aSourceFixture) int64 {
				t.Helper()
				link, err := harness.links.GetByID(fixture.ctx, fixture.linkID)
				if err != nil || link == nil {
					t.Fatalf("GetByID(before capture requeue) = %+v, %v", link, err)
				}
				captureText := "replacement browser capture"
				job, err := harness.links.RequeueExisting(fixture.ctx, fixture.linkID, &repository.CreateLinkParams{
					URL: link.URL, SourceKind: "browser_capture", SourceKey: link.URL,
					InputText: &captureText, Status: model.LinkStatusPending,
					RequestedLibraryKind: model.RequestedLibraryKindReading,
				})
				if err != nil || job == nil {
					t.Fatalf("RequeueExistingTx(capture) = %+v, %v", job, err)
				}
				current, err := harness.links.GetByID(fixture.ctx, fixture.linkID)
				if err != nil || current == nil || current.Status != model.LinkStatusPending {
					t.Fatalf("GetByID(after capture requeue) = %+v, %v", current, err)
				}
				return current.ContentRevision
			},
			wantBlockKey: "content",
		},
		{
			name: "reading to site conversion clears markdown saved body",
			initial: func() model.SavedContent {
				document := "# Reading source\n\nOld body."
				return model.SavedContent{
					Text: "Reading source old body.", Document: &document,
					Format: model.ContentFormatMarkdown, Words: 4,
				}
			}(),
			request: func(revision int64) model.TranslationRequest {
				return model.TranslationRequest{
					Scope: model.TranslationScopeFull, ExpectedContentRevision: &revision,
				}
			},
			transition: func(t *testing.T, fixture rf5aSourceFixture) int64 {
				t.Helper()
				if err := harness.links.UpdateLibraryClassification(fixture.ctx, repository.UpdateLibraryClassificationParams{
					ID: fixture.linkID, Kind: model.LibraryKindReading,
					Source: model.LibraryKindSourceUser,
				}); err != nil {
					t.Fatalf("UpdateLibraryClassification(reading): %v", err)
				}
				result, err := harness.links.ConvertLink(fixture.ctx, repository.ConvertLinkParams{
					LinkID: fixture.linkID, TargetKind: model.LibraryKindSite,
					ExpectedContentRevision: fixture.revision,
				})
				if err != nil || result.Kind != model.LibraryKindSite || result.ContentRevision <= fixture.revision {
					t.Fatalf("ConvertLink(reading to site) = %+v, %v", result, err)
				}
				return result.ContentRevision
			},
			wantBlockKey: "content",
		},
		{
			name: "plain body replacement installs markdown canonical block",
			initial: model.SavedContent{
				Text: "Plain saved source", Format: model.ContentFormatPlain, Words: 3,
			},
			request: func(revision int64) model.TranslationRequest {
				return model.TranslationRequest{
					Scope: model.TranslationScopeSelection, BlockKey: "content",
					StartOffset: 0, EndOffset: 5, SourceText: "Plain",
					ExpectedContentRevision: &revision,
				}
			},
			transition: func(t *testing.T, fixture rf5aSourceFixture) int64 {
				t.Helper()
				document := "# Current document\n\nMarkdown body."
				revision, stored, err := harness.links.ReplaceContentIfCurrent(
					fixture.ctx,
					fixture.linkID,
					fixture.updatedAt,
					model.SavedContent{
						Text: "Current document Markdown body.", Document: &document,
						Format: model.ContentFormatMarkdown, Words: 4,
					},
				)
				if err != nil || !stored || revision <= fixture.revision {
					t.Fatalf("ReplaceContentIfCurrent(markdown) = revision %d, stored %v, error %v",
						revision, stored, err)
				}
				return revision
			},
			wantBlockKey: "content-document",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := harness.createSource(t, "cas-precedence", tc.initial, "Stable summary")
			observed := make(chan int64, 1)
			release := make(chan struct{})
			result := make(chan rf5aCreateResult, 1)
			released := false
			t.Cleanup(func() {
				if !released {
					close(release)
				}
			})

			go func() {
				content, err := harness.links.GetContent(fixture.ctx, fixture.linkID)
				if err != nil || content == nil {
					result <- rf5aCreateResult{err: err}
					return
				}
				observed <- content.Revision
				<-release
				item, createErr := service.Create(
					fixture.ctx,
					fixture.linkID,
					tc.request(content.Revision),
				)
				result <- rf5aCreateResult{item: item, err: createErr}
			}()

			var observedRevision int64
			select {
			case observedRevision = <-observed:
			case early := <-result:
				t.Fatalf("source observation failed before transition: %v", early.err)
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for source observation barrier")
			}
			if observedRevision != fixture.revision {
				t.Fatalf("observed revision = %d, want %d", observedRevision, fixture.revision)
			}

			currentRevision := tc.transition(t, fixture)
			if currentRevision <= observedRevision {
				t.Fatalf("transition revision = %d, want greater than observed %d", currentRevision, observedRevision)
			}
			close(release)
			released = true

			var create rf5aCreateResult
			select {
			case create = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for stale schedule after transition")
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
			if identity.ContentRevision == nil || *identity.ContentRevision != currentRevision ||
				identity.BlockKey != tc.wantBlockKey || identity.SourceHash != nil {
				t.Fatalf("current identity = %+v, want revision %d/block %s",
					identity, currentRevision, tc.wantBlockKey)
			}
			assertRF5ANoScheduledWork(t, harness.pool, fixture.linkID)
		})
	}
}

func TestTranslationSourceSchedulePrioritizesGenerationCASAfterSiteCompletion(t *testing.T) {
	harness := newRF5AScheduleHarness(t, true)
	generationHarness := &savedGenerationHarness{
		PGXLinkRepository: harness.links,
		pool:              harness.pool,
	}
	fixture := newAutomaticSavedGenerationFixture(t, generationHarness, "translation-site-complete", true)
	before := readSavedGeneration(t, generationHarness, fixture.linkID)
	if !before.body.present || before.body.format != model.ContentFormatMarkdown {
		t.Fatalf("site completion fixture = %+v, want saved markdown body", before)
	}
	mustMarkGenerationParseProcessing(t, generationHarness, fixture)

	observed := make(chan int64, 1)
	release := make(chan struct{})
	result := make(chan rf5aCreateResult, 1)
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	service := harness.service(true, harness.queue)
	go func() {
		content, err := harness.links.GetContent(t.Context(), fixture.linkID)
		if err != nil || content == nil {
			result <- rf5aCreateResult{err: err}
			return
		}
		observed <- content.Revision
		<-release
		item, createErr := service.Create(t.Context(), fixture.linkID, model.TranslationRequest{
			Scope: model.TranslationScopeFull, ExpectedContentRevision: &content.Revision,
		})
		result <- rf5aCreateResult{item: item, err: createErr}
	}()

	var observedRevision int64
	select {
	case observedRevision = <-observed:
	case early := <-result:
		t.Fatalf("source observation failed before site completion: %v", early.err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pre-completion source observation")
	}
	if observedRevision != before.revision {
		t.Fatalf("observed revision = %d, want %d", observedRevision, before.revision)
	}

	aggregate, err := harness.links.CompleteSiteParse(
		t.Context(),
		generationSiteParseParams(fixture, true),
		fixture.jobID,
	)
	if err != nil || aggregate.SiteID == uuid.Nil || aggregate.EntryID == uuid.Nil {
		t.Fatalf("CompleteSiteParse() = %+v, %v", aggregate, err)
	}
	current := readSavedGeneration(t, generationHarness, fixture.linkID)
	if current.revision != observedRevision+1 || current.body.present ||
		!current.hasKind || current.kind != model.LibraryKindSite {
		t.Fatalf("completed site source = %+v, want revision %d/no body/site",
			current, observedRevision+1)
	}
	close(release)
	released = true

	var create rf5aCreateResult
	select {
	case create = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stale schedule after site completion")
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
	if identity.ContentRevision == nil || *identity.ContentRevision != current.revision ||
		identity.BlockKey != "content" || identity.SourceHash != nil {
		t.Fatalf("current identity = %+v, want revision %d/block content", identity, current.revision)
	}
	assertRF5ANoScheduledWork(t, harness.pool, fixture.linkID)
}
