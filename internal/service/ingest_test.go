package service

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

func TestIngestServiceCreatesSyntheticPendingLinkForTextSource(t *testing.T) {
	t.Parallel()

	linkStore := &repotest.ObservableLinkStore{
		CreateFunc: func(_ context.Context, params repository.CreateLinkParams) (*model.Link, error) {
			return &model.Link{
				ID:        uuid.MustParse("d1111111-1111-1111-1111-111111111111"),
				URL:       params.URL,
				Status:    params.Status,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}, nil
		},
	}
	jobStore := &repotest.ObservableJobStore{
		CreateFunc: func(_ context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
			return &model.ParseJob{
				ID:        uuid.MustParse("d2222222-2222-2222-2222-222222222222"),
				LinkID:    linkID,
				Status:    model.JobStatusPending,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}, nil
		},
	}
	queue := &submitFakeQueue{}
	service := newTestIngestService(linkStore, jobStore, queue, &submitFakeLocker{})

	got, err := service.Ingest(context.Background(), dto.IngestRequest{
		Sources: []dto.IngestSource{
			{Kind: "text", Text: "Hello multimodal ingest"},
		},
	})
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}

	if len(linkStore.GetBySourceKeyCalls) != 1 {
		t.Fatalf("GetBySourceKey calls = %d, want 1", len(linkStore.GetBySourceKeyCalls))
	}
	if linkStore.GetBySourceKeyCalls[0] == "" {
		t.Fatal("GetBySourceKey called with empty source key")
	}

	if len(linkStore.CreateCalls) != 1 {
		t.Fatalf("create calls = %d, want 1", len(linkStore.CreateCalls))
	}

	createParams := linkStore.CreateCalls[0]
	if !strings.HasPrefix(createParams.URL, "webtag://ingest/") {
		t.Fatalf("Create URL = %q, want synthetic ingest URL", createParams.URL)
	}
	if createParams.Status != model.LinkStatusPending {
		t.Fatalf("Create status = %q, want pending", createParams.Status)
	}
	assertStringFieldIfPresent(t, createParams, "SourceKind", "text")
	assertStringFieldIfPresent(t, createParams, "SourceKey", linkStore.GetBySourceKeyCalls[0])
	assertStringFieldIfPresent(t, createParams, "InputText", "Hello multimodal ingest")

	if len(queue.ids) != 1 {
		t.Fatalf("queued ids = %#v, want one job", queue.ids)
	}

	syntheticJob, _ := jobStore.CreateFunc(context.Background(), queue.ids[0])
	wantJobID := syntheticJob.ID.String()
	want := dto.SubmitResponse{
		JobID:  &wantJobID,
		LinkID: queue.ids[0].String(),
		Status: string(model.LinkStatusPending),
	}
	if !submitResponseEqual(got, want) {
		t.Fatalf("Ingest() = %#v, want %#v", got, want)
	}
}

func TestBrowserCapturePromotesUserNoteToDescription(t *testing.T) {
	t.Parallel()

	normalized, err := normalizeIngestRequest(dto.IngestRequest{
		Sources: []dto.IngestSource{{
			Kind:  "browser_capture",
			URL:   "https://example.com/article",
			Title: "Article",
			Text:  "Captured body",
			Metadata: map[string]any{
				"note":        "  Read this before the review  ",
				"captured_at": "2026-07-11T10:00:00Z",
			},
		}},
	}, captureDestinationLibrary)
	if err != nil {
		t.Fatalf("normalizeIngestRequest(, captureDestinationLibrary) error = %v", err)
	}

	params, err := normalized.toLinkCapture()
	if err != nil {
		t.Fatalf("toCreateLinkParams() error = %v", err)
	}
	if params.Description == nil || *params.Description != "Read this before the review" {
		t.Fatalf("Description = %#v, want promoted trimmed browser note", params.Description)
	}
}

func TestBrowserCaptureUsesURLAsStableSourceKey(t *testing.T) {
	t.Parallel()

	normalize := func(capturedAt string) normalizedIngest {
		t.Helper()
		got, err := normalizeIngestRequest(dto.IngestRequest{
			Sources: []dto.IngestSource{{
				Kind:  "browser_capture",
				URL:   "https://example.com/article",
				Title: "Article",
				Text:  "Captured body",
				Metadata: map[string]any{
					"captured_at": capturedAt,
				},
			}},
		}, captureDestinationLibrary)
		if err != nil {
			t.Fatalf("normalizeIngestRequest(, captureDestinationLibrary) error = %v", err)
		}
		return got
	}

	first := normalize("2026-07-11T10:00:00Z")
	second := normalize("2026-07-11T11:00:00Z")
	if first.sourceKey != "https://example.com/article" || second.sourceKey != first.sourceKey {
		t.Fatalf("source keys = %q and %q, want canonical URL", first.sourceKey, second.sourceKey)
	}
}

func TestSoleURLIngestUsesURLAsStableSourceKey(t *testing.T) {
	t.Parallel()

	normalized, err := normalizeIngestRequest(dto.IngestRequest{Sources: []dto.IngestSource{{
		Kind: "url",
		URL:  " https://example.com/sole-url ",
	}}}, captureDestinationLibrary)
	if err != nil {
		t.Fatalf("normalizeIngestRequest(, captureDestinationLibrary) error = %v", err)
	}
	if normalized.sourceKind != "url" {
		t.Fatalf("sourceKind = %q, want url", normalized.sourceKind)
	}
	if normalized.sourceKey != "https://example.com/sole-url" {
		t.Fatalf("sourceKey = %q, want canonical URL identity", normalized.sourceKey)
	}
	if normalized.storedURL != normalized.sourceKey || normalized.realURL != normalized.sourceKey || normalized.lockURL != normalized.sourceKey {
		t.Fatalf("URL identities diverged: normalized = %#v", normalized)
	}
}

func TestURLBackedMultimodalIngestUsesURLAsStableSourceKey(t *testing.T) {
	t.Parallel()

	submitted := "HTTPS://WWW.Example.com//article/?utm_source=share#selection"
	normalized, err := normalizeIngestRequest(dto.IngestRequest{Sources: []dto.IngestSource{
		{Kind: "url", URL: submitted},
		{Kind: "text", Text: "Selected passage"},
	}}, captureDestinationLibrary)
	if err != nil {
		t.Fatalf("normalizeIngestRequest(, captureDestinationLibrary) error = %v", err)
	}
	if normalized.sourceKind != "multimodal" {
		t.Fatalf("sourceKind = %q, want multimodal", normalized.sourceKind)
	}
	if normalized.sourceKey != "https://example.com/article" || normalized.realURL != normalized.sourceKey || normalized.lockURL != normalized.sourceKey {
		t.Fatalf("URL identities diverged: normalized = %#v", normalized)
	}
	if normalized.storedURL != submitted {
		t.Fatalf("storedURL = %q, want submitted display URL %q", normalized.storedURL, submitted)
	}
}

func TestIngestServiceURLOnlyReusesRichCaptureAcrossStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     model.LinkStatus
		sourceKind string
		withJob    bool
	}{
		{name: "done browser capture", status: model.LinkStatusDone, sourceKind: "browser_capture", withJob: true},
		{name: "failed multimodal capture", status: model.LinkStatusFailed, sourceKind: "multimodal", withJob: true},
		{name: "pending capture without repair", status: model.LinkStatusPending, sourceKind: "browser_capture", withJob: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			linkID := uuid.New()
			jobID := uuid.New()
			title := "Captured title"
			body := "Captured body"
			link := &model.Link{
				ID:         linkID,
				URL:        "https://example.com/url-only-reuse",
				SourceKind: tt.sourceKind,
				SourceKey:  "ingest:legacy-capture",
				InputTitle: &title,
				InputText:  &body,
				Status:     tt.status,
			}
			links := &repotest.ObservableLinkStore{
				ByURL: map[string]*model.Link{link.URL: link},
				UpdateStateFunc: func(_ context.Context, params repository.UpdateLinkStateParams) error {
					link.Status = params.Status
					return nil
				},
			}
			latest := map[uuid.UUID]*model.ParseJob{}
			if tt.withJob {
				latest[linkID] = &model.ParseJob{ID: jobID, LinkID: linkID, Status: model.JobStatus(tt.status)}
			}
			jobs := &repotest.ObservableJobStore{
				LatestByLinkID: latest,
				CreateResult:   &model.ParseJob{ID: uuid.New(), LinkID: linkID, Status: model.JobStatusPending},
			}
			queue := &submitFakeQueue{}
			submitter := &submitFakeSubmitter{links: links, jobs: jobs}
			service := newFakeIngestService(links, submitter, jobs, queue, &submitFakeLocker{})

			got, err := service.Ingest(context.Background(), dto.IngestRequest{Sources: []dto.IngestSource{{
				Kind: "url",
				URL:  link.URL,
			}}})
			if err != nil {
				t.Fatalf("Ingest() error = %v", err)
			}
			if got.LinkID != linkID.String() || got.Status != string(tt.status) {
				t.Fatalf("Ingest() = %#v, want existing %s rich capture", got, tt.status)
			}
			if tt.withJob && (got.JobID == nil || *got.JobID != jobID.String()) {
				t.Fatalf("Ingest() JobID = %#v, want %s", got.JobID, jobID)
			}
			if !tt.withJob && got.JobID != nil {
				t.Fatalf("Ingest() JobID = %#v, want nil without an existing attempt", got.JobID)
			}
			if len(submitter.requeueCaptures) != 0 || len(jobs.CreateCalls) != 0 || len(queue.ids) != 0 {
				t.Fatalf(
					"URL-only ingest created work: requeues=%d jobs=%d queued=%d",
					len(submitter.requeueCaptures),
					len(jobs.CreateCalls),
					len(queue.ids),
				)
			}
			if link.SourceKind != tt.sourceKind || link.SourceKey != "ingest:legacy-capture" || link.InputTitle == nil || *link.InputTitle != title || link.InputText == nil || *link.InputText != body {
				t.Fatalf("URL-only ingest changed captured input: link=%#v", link)
			}
		})
	}
}

func TestBrowserCapturePersistsStructuredHTMLAlongsideReadableText(t *testing.T) {
	t.Parallel()

	normalized, err := normalizeIngestRequest(dto.IngestRequest{
		Sources: []dto.IngestSource{{
			Kind: "browser_capture",
			URL:  "https://example.com/article",
			Text: "Readable body",
			HTML: "<html><body>Duplicated raw body</body></html>",
			ImageURLs: []string{
				"https://cdn.example.com/hero.png",
				"https://cdn.example.com/chart.png",
			},
		}},
	}, captureDestinationLibrary)
	if err != nil {
		t.Fatalf("normalizeIngestRequest(, captureDestinationLibrary) error = %v", err)
	}
	params, err := normalized.toLinkCapture()
	if err != nil {
		t.Fatalf("toCreateLinkParams() error = %v", err)
	}
	if params.InputText == nil || *params.InputText != "Readable body" {
		t.Fatalf("InputText = %#v, want readable body", params.InputText)
	}
	if params.InputHTML == nil || *params.InputHTML != "<html><body>Duplicated raw body</body></html>" {
		t.Fatalf("InputHTML = %#v, want retained structured snapshot", params.InputHTML)
	}
	if len(params.InputImages) != 0 {
		t.Fatalf("InputImages = %#v, want no implicit vision inputs", params.InputImages)
	}
}

func TestCaptureChangedIgnoresVolatileMetadataButDetectsUserContent(t *testing.T) {
	t.Parallel()

	title := "Article"
	body := "Captured body"
	note := "Keep this"
	link := &model.Link{
		SourceKind:  "browser_capture",
		SourceKey:   "https://example.com/article",
		InputTitle:  &title,
		InputText:   &body,
		Description: &note,
		SourceMetadata: map[string]any{
			"browser_capture": map[string]any{"captured_at": "old"},
		},
	}
	base := LinkCapture{
		SourceKind: "browser_capture",
		SourceKey:  "https://example.com/article",
		InputTitle: &title,
		InputText:  &body,
		SourceMetadata: map[string]any{
			"browser_capture": map[string]any{"captured_at": "new"},
		},
	}

	if captureChanged(link, base) {
		t.Fatal("captureChanged() = true for metadata-only change")
	}

	changedBody := base
	newBody := "Updated captured body"
	changedBody.InputText = &newBody
	if !captureChanged(link, changedBody) {
		t.Fatal("captureChanged() = false for updated body")
	}

	changedNote := base
	newNote := "New note"
	changedNote.Description = &newNote
	if !captureChanged(link, changedNote) {
		t.Fatal("captureChanged() = false for explicit note update")
	}
}

func TestCaptureChangedTreatsInputImagesAsSet(t *testing.T) {
	t.Parallel()

	link := &model.Link{
		SourceKind:  "multimodal",
		SourceKey:   "https://example.com/article",
		InputImages: []string{"https://cdn.example.com/a.png", "https://cdn.example.com/b.png"},
	}
	reordered := LinkCapture{
		SourceKind:  link.SourceKind,
		SourceKey:   link.SourceKey,
		InputImages: []string{"https://cdn.example.com/b.png", "https://cdn.example.com/a.png"},
	}
	if captureChanged(link, reordered) {
		t.Fatal("captureChanged() = true when only InputImages order changed")
	}

	changed := reordered
	changed.InputImages = []string{"https://cdn.example.com/b.png", "https://cdn.example.com/c.png"}
	if !captureChanged(link, changed) {
		t.Fatal("captureChanged() = false after the InputImages set changed")
	}
}

func TestIngestServiceUsesRealURLForMixedSourcesAndDedupesBySourceKey(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("e1111111-1111-1111-1111-111111111111")
	jobID := uuid.MustParse("e2222222-2222-2222-2222-222222222222")
	linkStore := &repotest.ObservableLinkStore{}
	linkStore.CreateFunc = func(_ context.Context, params repository.CreateLinkParams) (*model.Link, error) {
		link := &model.Link{
			ID:             linkID,
			URL:            params.URL,
			SourceKind:     params.SourceKind,
			SourceKey:      params.SourceKey,
			InputTitle:     params.InputTitle,
			InputText:      params.InputText,
			InputHTML:      params.InputHTML,
			InputImages:    params.InputImages,
			SourceMetadata: params.SourceMetadata,
			Description:    params.Description,
			Status:         params.Status,
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
		}
		if linkStore.BySourceKey == nil {
			linkStore.BySourceKey = map[string]*model.Link{}
		}
		sourceKey := readStringField(params, "SourceKey")
		if sourceKey != "" {
			linkStore.BySourceKey[sourceKey] = link
		}
		if linkStore.ByURL == nil {
			linkStore.ByURL = map[string]*model.Link{}
		}
		linkStore.ByURL[params.URL] = link
		return link, nil
	}
	jobStore := &repotest.ObservableJobStore{
		CreateFunc: func(_ context.Context, createdLinkID uuid.UUID) (*model.ParseJob, error) {
			return &model.ParseJob{
				ID:        jobID,
				LinkID:    createdLinkID,
				Status:    model.JobStatusPending,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}, nil
		},
		LatestByLinkID: map[uuid.UUID]*model.ParseJob{},
	}
	queue := &submitFakeQueue{}
	service := newTestIngestService(linkStore, jobStore, queue, &submitFakeLocker{})

	req := dto.IngestRequest{
		Sources: []dto.IngestSource{
			{Kind: "url", URL: "https://example.com/article"},
			{Kind: "text", Text: "captured note"},
		},
	}

	first, err := service.Ingest(context.Background(), req)
	if err != nil {
		t.Fatalf("first Ingest() error = %v", err)
	}

	if len(linkStore.CreateCalls) != 1 {
		t.Fatalf("create calls after first ingest = %d, want 1", len(linkStore.CreateCalls))
	}
	if linkStore.CreateCalls[0].URL != "https://example.com/article" {
		t.Fatalf("Create URL = %q, want real page URL", linkStore.CreateCalls[0].URL)
	}
	assertStringFieldIfPresent(t, linkStore.CreateCalls[0], "SourceKind", "multimodal")

	jobStore.LatestByLinkID[linkID] = &model.ParseJob{
		ID:        jobID,
		LinkID:    linkID,
		Status:    model.JobStatusPending,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	queue.ids = nil

	second, err := service.Ingest(context.Background(), req)
	if err != nil {
		t.Fatalf("second Ingest() error = %v", err)
	}

	if len(linkStore.CreateCalls) != 1 {
		t.Fatalf("create calls after second ingest = %d, want 1", len(linkStore.CreateCalls))
	}
	if len(linkStore.GetBySourceKeyCalls) != 2 {
		t.Fatalf("GetBySourceKey calls = %d, want 2", len(linkStore.GetBySourceKeyCalls))
	}
	if linkStore.GetBySourceKeyCalls[0] != linkStore.GetBySourceKeyCalls[1] {
		t.Fatalf("source keys = %#v, want deterministic identical key", linkStore.GetBySourceKeyCalls)
	}
	if len(queue.ids) != 0 {
		t.Fatalf("queued ids after second ingest = %#v, want none", queue.ids)
	}
	if !submitResponseEqual(first, second) {
		t.Fatalf("responses differ for identical ingest: first=%#v second=%#v", first, second)
	}
}

func TestIngestServiceRequeuesOnlyMeaningfulBrowserCaptureChanges(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("e3111111-1111-1111-1111-111111111111")
	oldJobID := uuid.MustParse("e3222222-2222-2222-2222-222222222222")
	newJobID := uuid.MustParse("e3333333-3333-3333-3333-333333333333")
	title := "Article"
	body := "Captured body"
	note := "Keep this"
	link := &model.Link{
		ID:          linkID,
		URL:         "https://example.com/article",
		SourceKind:  "browser_capture",
		SourceKey:   "https://example.com/article",
		InputTitle:  &title,
		InputText:   &body,
		Description: &note,
		Status:      model.LinkStatusDone,
	}
	links := &repotest.ObservableLinkStore{
		BySourceKey: map[string]*model.Link{link.SourceKey: link},
		UpdateStateFunc: func(_ context.Context, params repository.UpdateLinkStateParams) error {
			link.Status = params.Status
			return nil
		},
	}
	jobs := &repotest.ObservableJobStore{
		LatestByLinkID: map[uuid.UUID]*model.ParseJob{
			linkID: {ID: oldJobID, LinkID: linkID, Status: model.JobStatusDone},
		},
		CreateFunc: func(_ context.Context, gotLinkID uuid.UUID) (*model.ParseJob, error) {
			return &model.ParseJob{ID: newJobID, LinkID: gotLinkID, Status: model.JobStatusPending}, nil
		},
	}
	queue := &submitFakeQueue{}
	submitter := &submitFakeSubmitter{links: links, jobs: jobs}
	service := newFakeIngestService(links, submitter, jobs, queue, &submitFakeLocker{})

	request := func(capturedAt, text, userNote string) dto.IngestRequest {
		metadata := map[string]any{"captured_at": capturedAt}
		if userNote != "" {
			metadata["note"] = userNote
		}
		return dto.IngestRequest{Sources: []dto.IngestSource{{
			Kind: "browser_capture", URL: link.URL, Title: title, Text: text, Metadata: metadata,
		}}}
	}

	unchanged, err := service.Ingest(context.Background(), request("2026-07-11T11:00:00Z", body, ""))
	if err != nil {
		t.Fatalf("unchanged Ingest() error = %v", err)
	}
	if unchanged.Status != string(model.LinkStatusDone) || unchanged.JobID == nil || *unchanged.JobID != oldJobID.String() {
		t.Fatalf("unchanged Ingest() = %#v, want existing done job", unchanged)
	}
	if len(submitter.requeueCaptures) != 0 || len(queue.ids) != 0 {
		t.Fatalf("metadata-only ingest requeued: captures=%d queued=%d", len(submitter.requeueCaptures), len(queue.ids))
	}

	changedBody := "Updated captured body"
	changedRequest := request("2026-07-11T12:00:00Z", changedBody, "Updated note")
	changedRequest.Sources[0].URL = "HTTPS://WWW.Example.com//article/?utm_source=recapture#frag"
	changed, err := service.Ingest(context.Background(), changedRequest)
	if err != nil {
		t.Fatalf("changed Ingest() error = %v", err)
	}
	if changed.Status != string(model.LinkStatusPending) || changed.JobID == nil || *changed.JobID != newJobID.String() {
		t.Fatalf("changed Ingest() = %#v, want new pending job", changed)
	}
	if len(submitter.requeueCaptures) != 1 || submitter.requeueCaptures[0] == nil {
		t.Fatalf("requeue captures = %#v, want one normalized capture", submitter.requeueCaptures)
	}
	capture := submitter.requeueCaptures[0]
	if capture.URL != changedRequest.Sources[0].URL || capture.SourceKey != link.SourceKey {
		t.Fatalf("requeue URL = %q SourceKey = %q, want display %q identity %q", capture.URL, capture.SourceKey, changedRequest.Sources[0].URL, link.SourceKey)
	}
	if capture.InputText == nil || *capture.InputText != changedBody {
		t.Fatalf("requeue InputText = %#v, want %q", capture.InputText, changedBody)
	}
	if capture.Description == nil || *capture.Description != "Updated note" {
		t.Fatalf("requeue Description = %#v, want updated note", capture.Description)
	}
	if len(queue.ids) != 1 || queue.ids[0] != linkID {
		t.Fatalf("queued ids = %#v, want changed link once", queue.ids)
	}
}

func TestIngestServiceReusesFailedJobForUnchangedBrowserCapture(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("e4111111-1111-1111-1111-111111111111")
	failedJobID := uuid.MustParse("e4222222-2222-2222-2222-222222222222")
	title := "Article"
	body := "Captured body"
	link := &model.Link{
		ID:         linkID,
		URL:        "https://example.com/failed-capture",
		SourceKind: "browser_capture",
		SourceKey:  "https://example.com/failed-capture",
		InputTitle: &title,
		InputText:  &body,
		Status:     model.LinkStatusFailed,
	}
	links := &repotest.ObservableLinkStore{
		BySourceKey: map[string]*model.Link{link.SourceKey: link},
	}
	jobs := &repotest.ObservableJobStore{
		LatestByLinkID: map[uuid.UUID]*model.ParseJob{
			linkID: {ID: failedJobID, LinkID: linkID, Status: model.JobStatusFailed},
		},
	}
	queue := &submitFakeQueue{}
	submitter := &submitFakeSubmitter{links: links, jobs: jobs}
	service := newFakeIngestService(links, submitter, jobs, queue, &submitFakeLocker{})

	got, err := service.Ingest(context.Background(), dto.IngestRequest{
		Sources: []dto.IngestSource{{
			Kind: "browser_capture", URL: link.URL, Title: title, Text: body,
			Metadata: map[string]any{"captured_at": "2026-07-11T13:00:00Z"},
		}},
	})
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	if got.Status != string(model.LinkStatusFailed) || got.JobID == nil || *got.JobID != failedJobID.String() {
		t.Fatalf("Ingest() = %#v, want existing failed job", got)
	}
	if len(submitter.requeueCaptures) != 0 {
		t.Fatalf("requeue captures = %d, want 0", len(submitter.requeueCaptures))
	}
	if len(jobs.CreateCalls) != 0 {
		t.Fatalf("created jobs = %#v, want none", jobs.CreateCalls)
	}
	if len(queue.ids) != 0 {
		t.Fatalf("queued ids = %#v, want none", queue.ids)
	}
}

func TestIngestServiceMultisourceBrowserCaptureRequeuesOnlyForContentChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		title       string
		body        string
		wantRequeue bool
	}{
		{name: "captured_at only", title: "Article", body: "Captured body", wantRequeue: false},
		{name: "browser body changed", title: "Article", body: "Updated captured body", wantRequeue: true},
		{name: "browser title changed", title: "Updated article", body: "Captured body", wantRequeue: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			linkID := uuid.New()
			oldJobID := uuid.New()
			newJobID := uuid.New()
			originalTitle := "Article"
			originalBody := "Captured body\n\nSupplemental context"
			link := &model.Link{
				ID:         linkID,
				URL:        "https://example.com/multisource-capture",
				SourceKind: "multimodal",
				SourceKey:  "https://example.com/multisource-capture",
				InputTitle: &originalTitle,
				InputText:  &originalBody,
				Status:     model.LinkStatusDone,
			}
			links := &repotest.ObservableLinkStore{
				BySourceKey: map[string]*model.Link{link.SourceKey: link},
				UpdateStateFunc: func(_ context.Context, params repository.UpdateLinkStateParams) error {
					link.Status = params.Status
					return nil
				},
			}
			jobs := &repotest.ObservableJobStore{
				LatestByLinkID: map[uuid.UUID]*model.ParseJob{
					linkID: {ID: oldJobID, LinkID: linkID, Status: model.JobStatusDone},
				},
				CreateFunc: func(_ context.Context, gotLinkID uuid.UUID) (*model.ParseJob, error) {
					return &model.ParseJob{ID: newJobID, LinkID: gotLinkID, Status: model.JobStatusPending}, nil
				},
			}
			queue := &submitFakeQueue{}
			submitter := &submitFakeSubmitter{links: links, jobs: jobs}
			service := newFakeIngestService(links, submitter, jobs, queue, &submitFakeLocker{})

			got, err := service.Ingest(context.Background(), dto.IngestRequest{
				Sources: []dto.IngestSource{
					{
						Kind: "browser_capture", URL: link.URL, Title: tt.title, Text: tt.body,
						Metadata: map[string]any{"captured_at": "2026-07-11T14:00:00Z"},
					},
					{Kind: "text", Text: "Supplemental context"},
				},
			})
			if err != nil {
				t.Fatalf("Ingest() error = %v", err)
			}

			if !tt.wantRequeue {
				if got.Status != string(model.LinkStatusDone) || got.JobID == nil || *got.JobID != oldJobID.String() {
					t.Fatalf("Ingest() = %#v, want existing done job", got)
				}
				if len(submitter.requeueCaptures) != 0 || len(jobs.CreateCalls) != 0 || len(queue.ids) != 0 {
					t.Fatalf("captured_at-only ingest created work: requeues=%d jobs=%d queued=%d", len(submitter.requeueCaptures), len(jobs.CreateCalls), len(queue.ids))
				}
				return
			}

			if got.Status != string(model.LinkStatusPending) || got.JobID == nil || *got.JobID != newJobID.String() {
				t.Fatalf("Ingest() = %#v, want new pending job", got)
			}
			if len(submitter.requeueCaptures) != 1 || submitter.requeueCaptures[0] == nil {
				t.Fatalf("requeue captures = %#v, want one content requeue", submitter.requeueCaptures)
			}
			if len(jobs.CreateCalls) != 1 || len(queue.ids) != 1 || queue.ids[0] != linkID {
				t.Fatalf("created/queued work = %#v/%#v, want link once", jobs.CreateCalls, queue.ids)
			}
		})
	}
}

func TestIngestServiceAcceptsImageDataURLAndStoresSyntheticURL(t *testing.T) {
	t.Parallel()

	linkStore := &repotest.ObservableLinkStore{
		CreateFunc: func(_ context.Context, params repository.CreateLinkParams) (*model.Link, error) {
			return &model.Link{
				ID:        uuid.MustParse("f1111111-1111-1111-1111-111111111111"),
				URL:       params.URL,
				Status:    params.Status,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}, nil
		},
	}
	jobStore := &repotest.ObservableJobStore{
		CreateFunc: func(_ context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
			return &model.ParseJob{
				ID:        uuid.MustParse("f2222222-2222-2222-2222-222222222222"),
				LinkID:    linkID,
				Status:    model.JobStatusPending,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}, nil
		},
	}
	service := newTestIngestService(linkStore, jobStore, &submitFakeQueue{}, &submitFakeLocker{})

	_, err := service.Ingest(context.Background(), dto.IngestRequest{
		Sources: []dto.IngestSource{
			{Kind: "image", URL: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAUA"},
		},
	})
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}

	if len(linkStore.CreateCalls) != 1 {
		t.Fatalf("create calls = %d, want 1", len(linkStore.CreateCalls))
	}
	if !strings.HasPrefix(linkStore.CreateCalls[0].URL, "webtag://ingest/") {
		t.Fatalf("Create URL = %q, want synthetic ingest URL", linkStore.CreateCalls[0].URL)
	}
	assertStringFieldIfPresent(t, linkStore.CreateCalls[0], "SourceKind", "image")
}

func TestIngestServiceRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	service := newTestIngestService(&repotest.ObservableLinkStore{}, &repotest.ObservableJobStore{}, &submitFakeQueue{}, &submitFakeLocker{})

	tests := []struct {
		name string
		req  dto.IngestRequest
		want string
	}{
		{
			name: "no sources",
			req:  dto.IngestRequest{},
			want: "at least one source is required",
		},
		{
			name: "empty text source",
			req: dto.IngestRequest{
				Sources: []dto.IngestSource{{Kind: "text", Text: "   "}},
			},
			want: "text source requires non-empty text",
		},
		{
			name: "unsafe url source",
			req: dto.IngestRequest{
				Sources: []dto.IngestSource{{Kind: "url", URL: "http://127.0.0.1/private"}},
			},
			want: "unsafe url target",
		},
		{
			name: "invalid image locator",
			req: dto.IngestRequest{
				Sources: []dto.IngestSource{{Kind: "image", URL: "file:///tmp/test.png"}},
			},
			want: "image source requires a remote http/https URL or data URL",
		},
		{
			name: "empty browser capture",
			req: dto.IngestRequest{
				Sources: []dto.IngestSource{{Kind: "browser_capture"}},
			},
			want: "browser_capture source requires at least one meaningful field",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := service.Ingest(context.Background(), tt.req)
			var statusErr *httperr.Error
			if !errors.As(err, &statusErr) {
				t.Fatalf("Ingest() error = %v, want StatusError", err)
			}
			if statusErr.HTTPStatus() != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d", statusErr.HTTPStatus(), http.StatusUnprocessableEntity)
			}
			if statusErr.HTTPMessage() != tt.want {
				t.Fatalf("message = %q, want %q", statusErr.HTTPMessage(), tt.want)
			}
		})
	}
}

func TestIngestServiceRejectsBrowserCaptureMetadataBounds(t *testing.T) {
	t.Parallel()

	tooManyKeys := make(map[string]any, maxIngestMetadataKeys+1)
	for i := 0; i <= maxIngestMetadataKeys; i++ {
		tooManyKeys["k"+strconv.Itoa(i)] = "v"
	}

	longKey := strings.Repeat("k", maxIngestMetadataKeyLength+1)
	longValue := strings.Repeat("v", maxIngestMetadataValueLength+1)

	tests := []struct {
		name string
		meta map[string]any
		want string
	}{
		{
			name: "too many keys",
			meta: tooManyKeys,
			want: "browser_capture metadata exceeds key count limit",
		},
		{
			name: "key too long",
			meta: map[string]any{longKey: "v"},
			want: "browser_capture metadata key length exceeds limit",
		},
		{
			name: "string value too long",
			meta: map[string]any{"k": longValue},
			want: "browser_capture metadata string value length exceeds limit",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			service := newTestIngestService(&repotest.ObservableLinkStore{}, &repotest.ObservableJobStore{}, &submitFakeQueue{}, &submitFakeLocker{})
			_, err := service.Ingest(context.Background(), dto.IngestRequest{
				Sources: []dto.IngestSource{{
					Kind:     "browser_capture",
					Title:    "anchor",
					Metadata: tt.meta,
				}},
			})
			var statusErr *httperr.Error
			if !errors.As(err, &statusErr) {
				t.Fatalf("Ingest() error = %v, want StatusError", err)
			}
			if statusErr.HTTPStatus() != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d", statusErr.HTTPStatus(), http.StatusUnprocessableEntity)
			}
			if statusErr.HTTPMessage() != tt.want {
				t.Fatalf("message = %q, want %q", statusErr.HTTPMessage(), tt.want)
			}
		})
	}
}

func TestIngestServiceAcceptsBrowserCaptureMetadataAtLimits(t *testing.T) {
	t.Parallel()

	atLimitKeys := make(map[string]any, maxIngestMetadataKeys)
	// Reserve two of the slots for the edge-length entries so the total
	// stays at exactly maxIngestMetadataKeys.
	atLimitKeys["edge_key"] = strings.Repeat("v", maxIngestMetadataValueLength)
	atLimitKeys[strings.Repeat("k", maxIngestMetadataKeyLength)] = "v"
	for i := 0; len(atLimitKeys) < maxIngestMetadataKeys; i++ {
		atLimitKeys["k"+strconv.Itoa(i)] = "v"
	}

	tests := []struct {
		name string
		meta map[string]any
	}{
		{name: "empty metadata", meta: map[string]any{}},
		{name: "metadata at all limits", meta: atLimitKeys},
		// Locks the contract that the validator bounds only string values:
		// numbers, bools, nested maps, and slices pass through unchecked.
		// Future callers rely on storing rich JSON shapes here.
		{name: "non-string values pass through", meta: map[string]any{
			"count":   12345,
			"enabled": true,
			"nested":  map[string]any{"deep": map[string]any{"deeper": "ok"}},
			"list":    []any{1, "two", false},
		}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			linkStore := &repotest.ObservableLinkStore{
				CreateFunc: func(_ context.Context, params repository.CreateLinkParams) (*model.Link, error) {
					return &model.Link{
						ID:        uuid.MustParse("a1111111-1111-1111-1111-111111111111"),
						URL:       params.URL,
						Status:    params.Status,
						CreatedAt: time.Now().UTC(),
						UpdatedAt: time.Now().UTC(),
					}, nil
				},
			}
			jobStore := &repotest.ObservableJobStore{
				CreateFunc: func(_ context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
					return &model.ParseJob{
						ID:        uuid.MustParse("a2222222-2222-2222-2222-222222222222"),
						LinkID:    linkID,
						Status:    model.JobStatusPending,
						CreatedAt: time.Now().UTC(),
						UpdatedAt: time.Now().UTC(),
					}, nil
				},
			}
			service := newTestIngestService(linkStore, jobStore, &submitFakeQueue{}, &submitFakeLocker{})
			_, err := service.Ingest(context.Background(), dto.IngestRequest{
				Sources: []dto.IngestSource{{
					Kind:     "browser_capture",
					Title:    "anchor",
					Metadata: tt.meta,
				}},
			})
			if err != nil {
				t.Fatalf("Ingest() error = %v, want nil", err)
			}
		})
	}
}
