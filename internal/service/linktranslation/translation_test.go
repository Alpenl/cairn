package linktranslation

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/contentdoc"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service/translator"
)

type translationListReaderStub struct {
	snapshot *repository.TranslationListSnapshot
	err      error
}

func (s *translationListReaderStub) ReadListSnapshot(context.Context, uuid.UUID) (*repository.TranslationListSnapshot, error) {
	if s.snapshot == nil {
		return nil, s.err
	}
	snapshot := *s.snapshot
	snapshot.Items = append([]model.LinkTranslation(nil), s.snapshot.Items...)
	return &snapshot, s.err
}

type translationSchedulerStub struct {
	linkID     uuid.UUID
	request    model.TranslationRequest
	stallAfter time.Duration
	result     *model.LinkTranslation
	err        error
	calls      int
}

func (s *translationSchedulerStub) Schedule(
	_ context.Context,
	linkID uuid.UUID,
	req model.TranslationRequest,
	stallAfter time.Duration,
) (*model.LinkTranslation, error) {
	s.calls++
	s.linkID = linkID
	s.request = req
	s.stallAfter = stallAfter
	return s.result, s.err
}

func translationDoneLink() *model.Link {
	return &model.Link{
		ID: uuid.New(), Status: model.LinkStatusDone, ContentRevision: 8,
		UpdatedAt: time.Now().UTC(),
	}
}

func translationItem(linkID uuid.UUID, scope model.TranslationScope) model.LinkTranslation {
	now := time.Now().UTC()
	return model.LinkTranslation{
		ID: uuid.New(), LinkID: linkID, Scope: scope, BlockKey: "summary",
		SourceText: "hello", SourceFormat: model.TranslationFormatPlain,
		TargetLanguage: model.TranslationTargetChinese,
		SourceHash:     hashTranslationSource("hello"), Status: model.TranslationStatusDone,
		CreatedAt: now, UpdatedAt: now,
	}
}

func translationListSnapshot(
	link *model.Link,
	content *model.SavedContent,
	items ...model.LinkTranslation,
) *repository.TranslationListSnapshot {
	source := repository.TranslationSourceSnapshot{
		Status:          link.Status,
		LibraryKind:     link.LibraryKind,
		ContentRevision: link.ContentRevision,
		Summary:         link.Summary,
	}
	if content != nil {
		source.Content = &content.Text
		source.ContentDocument = content.Document
		source.ContentFormat = content.Format
	}
	return &repository.TranslationListSnapshot{Source: source, Items: items}
}

func TestTranslationServiceForwardsCreateToDurableScheduler(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	expectedRevision := int64(8)
	expectedHash := hashTranslationSource("summary")
	want := &model.LinkTranslation{ID: uuid.New(), LinkID: linkID, Status: model.TranslationStatusPending}
	scheduler := &translationSchedulerStub{result: want}
	svc := NewService(ServiceOptions{
		Scheduler:      scheduler,
		RequestTimeout: 60 * time.Second,
	})
	req := model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "summary",
		StartOffset: 0, EndOffset: 5, SourceText: "hello",
		ExpectedContentRevision: &expectedRevision,
		ExpectedSourceHash:      &expectedHash,
		Force:                   true,
	}

	got, err := svc.Create(context.Background(), linkID, req)
	if err != nil || got != want {
		t.Fatalf("Create() = %+v, %v", got, err)
	}
	if scheduler.calls != 1 || scheduler.linkID != linkID || scheduler.request.Scope != req.Scope ||
		scheduler.request.BlockKey != req.BlockKey || scheduler.request.SourceText != req.SourceText ||
		scheduler.request.ExpectedContentRevision == nil || *scheduler.request.ExpectedContentRevision != expectedRevision ||
		scheduler.request.ExpectedSourceHash == nil || *scheduler.request.ExpectedSourceHash != expectedHash ||
		!scheduler.request.Force {
		t.Fatalf("scheduler command = %+v", scheduler)
	}
	if scheduler.stallAfter != translationStallAfter(60*time.Second) {
		t.Fatalf("stallAfter = %v, want %v", scheduler.stallAfter, translationStallAfter(60*time.Second))
	}
}

func TestTranslationServicePropagatesDurableSchedulerFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("durable schedule failed")
	svc := NewService(ServiceOptions{Scheduler: &translationSchedulerStub{err: wantErr}})
	_, err := svc.Create(context.Background(), uuid.New(), model.TranslationRequest{Scope: model.TranslationScopeFull})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Create() error = %v, want %v", err, wantErr)
	}
}

func TestTranslationServiceFailsClosedWithoutDurableScheduler(t *testing.T) {
	t.Parallel()

	svc := NewService(ServiceOptions{})
	_, err := svc.Create(context.Background(), uuid.New(), model.TranslationRequest{Scope: model.TranslationScopeFull})
	if err == nil || err.Error() != "translation scheduler is not configured" {
		t.Fatalf("Create() error = %v", err)
	}
}

func TestTranslationServiceListProjectsCurrentStaleAndLegacyItems(t *testing.T) {
	t.Parallel()

	link := translationDoneLink()
	summary := "hello"
	link.Summary = &summary
	currentContent := &model.SavedContent{Text: "current source", Format: model.ContentFormatPlain, Revision: link.ContentRevision}
	currentRevision := link.ContentRevision
	oldRevision := link.ContentRevision - 1
	translated := "translated"

	current := translationItem(link.ID, model.TranslationScopeFull)
	current.BlockKey = "content"
	current.SourceHash = hashTranslationSource(currentContent.Text)
	current.SourceContentRevision = &currentRevision
	current.TranslatedText = &translated

	sameHashOldRevision := current
	sameHashOldRevision.ID = uuid.New()
	sameHashOldRevision.SourceContentRevision = &oldRevision

	legacyCurrentHash := current
	legacyCurrentHash.ID = uuid.New()
	legacyCurrentHash.SourceContentRevision = nil

	summaryLegacy := translationItem(link.ID, model.TranslationScopeSelection)
	summaryLegacy.StartOffset = 0
	summaryLegacy.EndOffset = len(summary)
	summaryLegacy.SourceContentRevision = nil

	svc := NewService(ServiceOptions{
		Translations: &translationListReaderStub{snapshot: translationListSnapshot(
			link,
			currentContent,
			current, sameHashOldRevision, legacyCurrentHash, summaryLegacy,
		)},
	})

	got, err := svc.List(context.Background(), link.ID)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got.CurrentContentRevision != link.ContentRevision || len(got.Items) != 4 {
		t.Fatalf("List() = %+v", got)
	}
	wantSummaryHash := hashTranslationSource(summary)
	if got.CurrentSummarySourceHash == nil || *got.CurrentSummarySourceHash != wantSummaryHash {
		t.Fatalf("current summary source hash = %v, want %q", got.CurrentSummarySourceHash, wantSummaryHash)
	}
	if got.Items[0].Stale {
		t.Fatalf("current saved translation marked stale: %+v", got.Items[0])
	}
	if !got.Items[1].Stale {
		t.Fatalf("old revision with matching hash marked current: %+v", got.Items[1])
	}
	if got.Items[2].Stale {
		t.Fatalf("legacy full row with matching integrity hash marked stale: %+v", got.Items[2])
	}
	if got.Items[3].Stale {
		t.Fatalf("summary legacy row tied to saved revision: %+v", got.Items[3])
	}
}

func TestTranslationServiceListMarksChangedLegacyFullHashStale(t *testing.T) {
	t.Parallel()

	link := translationDoneLink()
	current := &model.SavedContent{Text: "new source", Format: model.ContentFormatPlain}
	old := translationItem(link.ID, model.TranslationScopeFull)
	old.SourceHash = hashTranslationSource("old source")
	svc := NewService(ServiceOptions{
		Translations: &translationListReaderStub{snapshot: translationListSnapshot(link, current, old)},
	})

	got, err := svc.List(context.Background(), link.ID)
	if err != nil || len(got.Items) != 1 || !got.Items[0].Stale {
		t.Fatalf("List() = %+v, %v, want stale legacy full row", got, err)
	}
}

func TestTranslationServiceListKeepsDocumentOnlyMarkdownFullTranslationCurrent(t *testing.T) {
	t.Parallel()

	link := translationDoneLink()
	document := "# Saved\n\nDocument only"
	revision := link.ContentRevision
	item := translationItem(link.ID, model.TranslationScopeFull)
	item.BlockKey = "content-document"
	item.SourceText = document
	item.SourceFormat = model.TranslationFormatMarkdown
	item.SourceHash = hashTranslationSource(document)
	item.SourceContentRevision = &revision
	svc := NewService(ServiceOptions{
		Translations: &translationListReaderStub{snapshot: &repository.TranslationListSnapshot{
			Source: repository.TranslationSourceSnapshot{
				Status:          link.Status,
				ContentRevision: revision,
				ContentDocument: &document,
				ContentFormat:   model.ContentFormatMarkdown,
			},
			Items: []model.LinkTranslation{item},
		}},
	})

	got, err := svc.List(context.Background(), link.ID)
	if err != nil || len(got.Items) != 1 || got.Items[0].Stale {
		t.Fatalf("List() = %+v, %v, want document-only Markdown full translation current", got, err)
	}
}

func TestTranslationServiceListMarksChangedSummaryStaleWithoutSavedRevisionChange(t *testing.T) {
	t.Parallel()

	link := translationDoneLink()
	currentSummary := "**new** summary"
	link.Summary = &currentSummary
	old := translationItem(link.ID, model.TranslationScopeSelection)
	old.BlockKey = "summary"
	old.SourceHash = hashTranslationSource("old summary")
	svc := NewService(ServiceOptions{
		Translations: &translationListReaderStub{snapshot: translationListSnapshot(link, nil, old)},
	})

	got, err := svc.List(context.Background(), link.ID)
	if err != nil || len(got.Items) != 1 || !got.Items[0].Stale {
		t.Fatalf("List() = %+v, %v, want stale summary row", got, err)
	}
	if got.CurrentContentRevision != link.ContentRevision {
		t.Fatalf("current revision = %d, want unchanged %d", got.CurrentContentRevision, link.ContentRevision)
	}
	projection, err := contentdoc.RenderedBlockProjection(model.ContentFormatMarkdown, currentSummary)
	if err != nil {
		t.Fatalf("project summary: %v", err)
	}
	wantSummaryHash := hashTranslationSource(projection)
	if got.CurrentSummarySourceHash == nil || *got.CurrentSummarySourceHash != wantSummaryHash {
		t.Fatalf("current summary source hash = %v, want %q", got.CurrentSummarySourceHash, wantSummaryHash)
	}
}

func TestTranslationServiceListKeepsMatchingHistoricalSummarySelectionCurrent(t *testing.T) {
	t.Parallel()

	link := translationDoneLink()
	currentSummary := "Alpha **beta** gamma"
	link.Summary = &currentSummary
	historical := translationItem(link.ID, model.TranslationScopeSelection)
	historical.BlockKey = "summary"
	historical.StartOffset = 6
	historical.EndOffset = 10
	historical.SourceText = "beta"
	historical.SourceHash = hashTranslationSource(historical.SourceText)
	historical.SourceContentRevision = nil
	svc := NewService(ServiceOptions{
		Translations: &translationListReaderStub{snapshot: translationListSnapshot(link, nil, historical)},
	})

	got, err := svc.List(context.Background(), link.ID)
	if err != nil || len(got.Items) != 1 || got.Items[0].Stale {
		t.Fatalf("List() = %+v, %v, want matching historical summary selection current", got, err)
	}
}

func TestTranslationServiceListMarksMismatchedHistoricalSummarySelectionStale(t *testing.T) {
	t.Parallel()

	link := translationDoneLink()
	currentSummary := "Alpha zeta beta gamma"
	link.Summary = &currentSummary
	historical := translationItem(link.ID, model.TranslationScopeSelection)
	historical.BlockKey = "summary"
	historical.StartOffset = 6
	historical.EndOffset = 10
	historical.SourceText = "beta"
	historical.SourceHash = hashTranslationSource(historical.SourceText)
	historical.SourceContentRevision = nil
	svc := NewService(ServiceOptions{
		Translations: &translationListReaderStub{snapshot: translationListSnapshot(link, nil, historical)},
	})

	got, err := svc.List(context.Background(), link.ID)
	if err != nil || len(got.Items) != 1 || !got.Items[0].Stale {
		t.Fatalf("List() = %+v, %v, want mismatched historical summary selection stale", got, err)
	}
}

func TestTranslationServiceListKeepsHistoricalWholeSummarySelectionCurrentAfterTailChange(t *testing.T) {
	t.Parallel()

	link := translationDoneLink()
	currentSummary := "Alpha beta!"
	link.Summary = &currentSummary
	historical := translationItem(link.ID, model.TranslationScopeSelection)
	historical.BlockKey = "summary"
	historical.StartOffset = 0
	historical.EndOffset = len("Alpha beta")
	historical.SourceText = "Alpha beta"
	historical.SourceHash = hashTranslationSource(historical.SourceText)
	historical.SourceContentRevision = nil
	svc := NewService(ServiceOptions{
		Translations: &translationListReaderStub{snapshot: translationListSnapshot(link, nil, historical)},
	})

	got, err := svc.List(context.Background(), link.ID)
	if err != nil || len(got.Items) != 1 || got.Items[0].Stale {
		t.Fatalf("List() = %+v, %v, want matching historical whole-summary selection current", got, err)
	}
}

func TestTranslationServiceListClassifiesVerifiedSummaryBlockIdentity(t *testing.T) {
	t.Parallel()

	const oldProjection = "Alpha beta"
	for _, tc := range []struct {
		name      string
		summary   string
		wantStale bool
	}{
		{name: "unchanged block is current", summary: oldProjection},
		{name: "tail change is stale", summary: oldProjection + "!", wantStale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			link := translationDoneLink()
			link.Summary = &tc.summary
			verified := translationItem(link.ID, model.TranslationScopeSelection)
			verified.BlockKey = "summary"
			verified.StartOffset = 0
			verified.EndOffset = len(oldProjection)
			verified.SourceText = oldProjection
			verified.SourceHash = contentdoc.RenderedSourceBlockPersistenceHash("summary", oldProjection)
			verified.SourceContentRevision = nil
			svc := NewService(ServiceOptions{
				Translations: &translationListReaderStub{snapshot: translationListSnapshot(link, nil, verified)},
			})

			got, err := svc.List(context.Background(), link.ID)
			if err != nil || len(got.Items) != 1 || got.Items[0].Stale != tc.wantStale {
				t.Fatalf("List() = %+v, %v, want stale=%v", got, err, tc.wantStale)
			}
		})
	}
}

func TestTranslationServiceListEnvelopeCarriesRevisionWhenEmpty(t *testing.T) {
	t.Parallel()

	link := translationDoneLink()
	svc := NewService(ServiceOptions{
		Translations: &translationListReaderStub{snapshot: translationListSnapshot(link, nil)},
	})
	got, err := svc.List(context.Background(), link.ID)
	if err != nil || got.CurrentContentRevision != link.ContentRevision ||
		got.CurrentSummarySourceHash != nil || len(got.Items) != 0 {
		t.Fatalf("List() = %+v, %v", got, err)
	}
}

func TestTranslationServiceListReturnsNotFoundForMissingSnapshot(t *testing.T) {
	t.Parallel()

	svc := NewService(ServiceOptions{Translations: &translationListReaderStub{}})
	_, err := svc.List(context.Background(), uuid.New())
	statusErr, ok := httperr.As(err)
	coder, coded := statusErr.(httperr.ErrorCoder)
	if !ok || !coded || statusErr.HTTPStatus() != 404 || coder.HTTPErrorCode() != httperr.CodeLinkNotFound {
		t.Fatalf("List() error = %v, want 404 %s", err, httperr.CodeLinkNotFound)
	}
}

func TestTranslationServiceListRejectsSiteSnapshot(t *testing.T) {
	t.Parallel()

	link := translationDoneLink()
	kind := model.LibraryKindSite
	link.LibraryKind = &kind
	svc := NewService(ServiceOptions{
		Translations: &translationListReaderStub{snapshot: translationListSnapshot(link, nil)},
	})
	_, err := svc.List(context.Background(), link.ID)
	statusErr, ok := httperr.As(err)
	coder, coded := statusErr.(httperr.ErrorCoder)
	if !ok || !coded || statusErr.HTTPStatus() != 409 || coder.HTTPErrorCode() != httperr.CodeSiteOriginalContentForbidden {
		t.Fatalf("List() error = %v, want 409 %s", err, httperr.CodeSiteOriginalContentForbidden)
	}
}

func TestTranslationStallThresholdExceedsJobTimeout(t *testing.T) {
	t.Parallel()

	for _, requestTimeout := range []time.Duration{
		time.Second,
		30 * time.Second,
		60 * time.Second,
		121 * time.Second,
		5 * time.Minute,
	} {
		t.Run(fmt.Sprint(requestTimeout), func(t *testing.T) {
			t.Parallel()
			jobTimeout := translator.DefaultJobTimeout(requestTimeout)
			if got := translationStallAfter(requestTimeout); got <= jobTimeout {
				t.Fatalf("stall threshold %v must exceed job timeout %v", got, jobTimeout)
			}
		})
	}
}
