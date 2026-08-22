package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/errsafe"
	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
	analyzerpkg "webtag/internal/service/analyzer"
)

type pipelineRegressionCase struct {
	Name                    string   `json:"name"`
	URL                     string   `json:"url"`
	FetcherType             string   `json:"fetcher_type"`
	FetchBody               string   `json:"fetch_body"`
	FetchError              string   `json:"fetch_error"`
	Summary                 string   `json:"summary"`
	Tags                    []string `json:"tags"`
	WantStatus              string   `json:"want_status"`
	WantLowConfidenceReason string   `json:"want_low_confidence_reason"`
}

func TestParsePipelineRunProcessesLinkAndEnsuresTreeOnSuccess(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("11111111-aaaa-1111-aaaa-111111111111")
	rootID := uuid.MustParse("33333333-cccc-3333-cccc-333333333333")
	now := time.Now().UTC()

	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {
			ID:        linkID,
			URL:       "https://example.com/articles/12345",
			Status:    model.LinkStatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
	})
	tagStore := &pipelineFakeTagStore{tags: []string{"Go", "AI"}}
	treeStore := newPipelineTreeStore(
		map[string]*model.Link{
			"https://example.com/": {
				ID:        rootID,
				URL:       "https://example.com/",
				Status:    model.LinkStatusDone,
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	)
	events := make([]string, 0, 8)
	fetcher := pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
		events = append(events, "fetch")
		return fetcher.Content{
			URL:         "https://example.com/articles/12345",
			Title:       "Fetched title",
			Body:        "Fetched body",
			FetcherType: "basic",
		}, nil
	})
	analyzer := pipelineAnalyzerFunc(func(context.Context, analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
		events = append(events, "analyze")
		return analyzerpkg.AnalysisResult{Summary: "分析摘要", Tags: []string{"Go", "AI"}}, nil
	})

	// Replace the noop write hooks installed by the helpers with
	// event-recording variants so we can assert the persistence
	// ordering from the captured slice. The Observable wrapper
	// records the params independently.
	linkStore.MarkParseProcessingFunc = func(_ context.Context, _ model.ParseAttempt) error {
		events = append(events, "state:processing")
		return nil
	}
	// 终态写入挂在 CompleteReadingParse 上——这是生产 reading 路径实际使用的
	// 事务。此前这里挂的是 CompleteParseFunc（legacy 路径），因而断言的是一条
	// 生产根本不会走的分支。
	linkStore.CompleteReadingParseFunc = func(_ context.Context, params repository.CompleteReadingParseParams) (repository.CompleteReadingParseResult, error) {
		events = append(events, fmt.Sprintf("state:%s", params.Analysis.Status))
		return repository.CompleteReadingParseResult{MetadataRevision: 1, MetadataApplied: true}, nil
	}
	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Tags:             tagStore,
		Tree:             treeStore,
		Fetcher:          fetcher,
		Analyzer:         analyzer,
	})

	if err := pipeline.Run(context.Background(), pipelineAttempt(linkID)); err != nil {
		if !errors.Is(err, errsafe.ErrAlreadyPersisted) {
			t.Fatalf("Run() error = %v, want errsafe.ErrAlreadyPersisted", err)
		}
	}

	wantOrder := []string{
		"state:processing",
		"fetch",
		"analyze",
		"state:done",
	}
	assertEventOrder(t, events, wantOrder)

	if len(linkStore.UpdateAnalysisCalls) != 1 {
		t.Fatalf("analysis updates = %d, want 1", len(linkStore.UpdateAnalysisCalls))
	}

	update := linkStore.UpdateAnalysisCalls[0]
	if update.Status != model.LinkStatusDone {
		t.Fatalf("final link status = %q, want done", update.Status)
	}
	if update.Title == nil || *update.Title != "Fetched title" {
		t.Fatalf("title = %#v, want fetched title", update.Title)
	}
	if update.Summary == nil || *update.Summary != "分析摘要" {
		t.Fatalf("summary = %#v, want 分析摘要", update.Summary)
	}
	if len(update.Tags) != 2 || update.Tags[0] != "Go" || update.Tags[1] != "AI" {
		t.Fatalf("tags = %#v, want [Go AI]", update.Tags)
	}
	if update.Domain == nil || *update.Domain != "example.com" {
		t.Fatalf("domain = %#v, want example.com", update.Domain)
	}
	if update.ContentType == nil || *update.ContentType != string(model.ContentTypeArticle) {
		t.Fatalf("content type = %#v, want article", update.ContentType)
	}
	if update.ParentID == nil {
		t.Fatal("parent id = nil, want ensured parent")
	}
}

func TestParsePipelineRunMarksLinkFailedWhenFetchFails(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("55555555-eeee-5555-eeee-555555555555")
	now := time.Now().UTC()

	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {
			ID:        linkID,
			URL:       "https://example.com/fail",
			Status:    model.LinkStatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
	})
	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Tags:             &pipelineFakeTagStore{},
		Tree:             newPipelineTreeStore(nil),
		Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
			return fetcher.Content{}, &fetcher.FetchError{URL: "https://example.com/fail", Reason: "dial tcp: lookup example.com: no such host"}
		}),
		Analyzer: pipelineAnalyzerFunc(func(context.Context, analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
			t.Fatal("Analyze() should not be called on fetch failure")
			return analyzerpkg.AnalysisResult{}, nil
		}),
	})

	if err := pipeline.Run(context.Background(), pipelineAttempt(linkID)); !errors.Is(err, errsafe.ErrAlreadyPersisted) {
		t.Fatalf("Run() error = %v, want errsafe.ErrAlreadyPersisted", err)
	}

	if len(linkStore.MarkFailedCalls) == 0 {
		t.Fatal("missing atomic failure state update")
	}
	lastFailure := linkStore.MarkFailedCalls[len(linkStore.MarkFailedCalls)-1]
	if lastFailure.Attempt != pipelineAttempt(linkID) {
		t.Fatalf("failure attempt = %#v, want %#v", lastFailure.Attempt, pipelineAttempt(linkID))
	}
	if lastFailure.ErrorMessage == "" {
		t.Fatal("failure error message is empty")
	}
}

func TestParsePipelineRunTracksLowConfidenceOutputs(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("77777777-1111-7777-1111-777777777777")
	rootID := uuid.MustParse("99999999-3333-9999-3333-999999999999")
	now := time.Now().UTC()
	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {
			ID:        linkID,
			URL:       "https://example.com/posts/12345",
			Status:    model.LinkStatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
	})
	treeStore := newPipelineTreeStore(
		map[string]*model.Link{
			"https://example.com/": {
				ID:        rootID,
				URL:       "https://example.com/",
				Status:    model.LinkStatusDone,
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	)

	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Tags:             &pipelineFakeTagStore{tags: []string{"Go"}},
		Tree:             treeStore,
		Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
			return fetcher.Content{
				URL:         "https://example.com/posts/12345",
				Title:       "Fetched title",
				Body:        "Fetched body",
				FetcherType: "basic+search",
			}, nil
		}),
		Analyzer: pipelineAnalyzerFunc(func(context.Context, analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
			return analyzerpkg.AnalysisResult{Summary: "低置信摘要", Tags: []string{"Go"}}, nil
		}),
	})

	if err := pipeline.Run(context.Background(), pipelineAttempt(linkID)); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	last := linkStore.UpdateAnalysisCalls[len(linkStore.UpdateAnalysisCalls)-1]
	if !last.IsLowConfidence || last.LowConfidenceReason == nil || *last.LowConfidenceReason != lowConfidenceReasonSearchFallback {
		t.Fatalf("low confidence result = %#v, want %q", last, lowConfidenceReasonSearchFallback)
	}
}

func TestParsePipelineRunMarksGenericTitleAsLowConfidence(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("12121212-1111-1212-1111-121212121212")
	now := time.Now().UTC()
	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {
			ID:        linkID,
			URL:       "https://example.com/post",
			Status:    model.LinkStatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
	})
	treeStore := newPipelineTreeStore(nil)

	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Tags:             &pipelineFakeTagStore{tags: []string{"Go"}},
		Tree:             treeStore,
		Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
			return fetcher.Content{
				URL:         "https://example.com/post",
				Title:       "Home",
				Body:        strings.Repeat("A", 120),
				FetcherType: "basic",
			}, nil
		}),
		Analyzer: pipelineAnalyzerFunc(func(context.Context, analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
			return analyzerpkg.AnalysisResult{Summary: "标题偏弱摘要", Tags: []string{"Go"}}, nil
		}),
	})

	if err := pipeline.Run(context.Background(), pipelineAttempt(linkID)); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	last := linkStore.UpdateAnalysisCalls[len(linkStore.UpdateAnalysisCalls)-1]
	if !last.IsLowConfidence {
		t.Fatal("expected low confidence analysis for generic title")
	}
	if last.LowConfidenceReason == nil || *last.LowConfidenceReason != "title_quality" {
		t.Fatalf("low confidence reason = %#v, want title_quality", last.LowConfidenceReason)
	}
}

func TestParsePipelineRunMarksExplicitWeakQualitySignalAsLowConfidence(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("56565656-1111-5656-1111-565656565656")
	now := time.Now().UTC()
	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {
			ID:        linkID,
			URL:       "https://example.com/post",
			Status:    model.LinkStatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
	})
	treeStore := newPipelineTreeStore(nil)

	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Tags:             &pipelineFakeTagStore{tags: []string{"Go"}},
		Tree:             treeStore,
		Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
			return fetcher.Content{
				URL:         "https://example.com/post",
				Title:       "Useful title",
				Body:        strings.Repeat("A", 120),
				FetcherType: "basic",
				Metadata: map[string]any{
					"quality_signal": "weak",
				},
			}, nil
		}),
		Analyzer: pipelineAnalyzerFunc(func(context.Context, analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
			return analyzerpkg.AnalysisResult{Summary: "抓取质量偏弱", Tags: []string{"Go"}}, nil
		}),
	})

	if err := pipeline.Run(context.Background(), pipelineAttempt(linkID)); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	last := linkStore.UpdateAnalysisCalls[len(linkStore.UpdateAnalysisCalls)-1]
	if !last.IsLowConfidence {
		t.Fatal("expected low confidence analysis for explicit weak quality signal")
	}
	if last.LowConfidenceReason == nil || *last.LowConfidenceReason != "fetch_quality" {
		t.Fatalf("low confidence reason = %#v, want fetch_quality", last.LowConfidenceReason)
	}
}

func TestParsePipelineRunSkipsFetcherForIngestSources(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("9aaaaaaa-1111-9aaa-1111-aaaaaaaaaaaa")
	now := time.Now().UTC()

	inputTitle := "(19) X 上的 GitHubDaily：“想验证自己的选股思路，现成炒股软件改不动，自己搭环境又太折腾。tickflow-stock-panel 是个自托管的 A 股量化工作台。” / X"
	generatedTitle := "tickflow A股量化工作台"
	inputText := "Captured page text body for the analyzer."
	inputHTML := "<p>Captured HTML fragment</p>"
	images := []string{"https://cdn.example.com/screen-1.png"}

	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {
			ID:          linkID,
			URL:         "https://x.com/GitHub_Daily/status/1941500000000000000",
			SourceKind:  "browser_capture",
			SourceKey:   "ingest:abc",
			InputTitle:  &inputTitle,
			InputText:   &inputText,
			InputHTML:   &inputHTML,
			InputImages: images,
			Status:      model.LinkStatusPending,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	})
	treeStore := newPipelineTreeStore(nil)

	fetcherCalls := 0
	var captured analyzerpkg.AnalyzeRequest

	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Tags:             &pipelineFakeTagStore{tags: []string{"news"}},
		Tree:             treeStore,
		Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
			fetcherCalls++
			return fetcher.Content{}, errors.New("fetcher should not run for ingest sources")
		}),
		Analyzer: pipelineAnalyzerFunc(func(_ context.Context, req analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
			captured = req
			return analyzerpkg.AnalysisResult{Title: generatedTitle, Summary: "ingest summary", Tags: []string{"news"}}, nil
		}),
	})

	if err := pipeline.Run(context.Background(), pipelineAttempt(linkID)); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if fetcherCalls != 0 {
		t.Fatalf("fetcher invocations = %d, want 0 for ingest source", fetcherCalls)
	}
	if captured.Content.FetcherType != "ingest" {
		t.Fatalf("analyzer Content.FetcherType = %q, want ingest", captured.Content.FetcherType)
	}
	if !strings.Contains(captured.Content.Body, inputText) {
		t.Fatalf("analyzer Content.Body = %q, want to include input_text", captured.Content.Body)
	}
	if strings.Contains(captured.Content.Body, inputHTML) {
		t.Fatalf("analyzer Content.Body = %q, must not append input_html when input_text is present", captured.Content.Body)
	}
	if captured.Content.Title != inputTitle {
		t.Fatalf("analyzer Content.Title = %q, want %q", captured.Content.Title, inputTitle)
	}
	if len(captured.Content.ImageURLs) != 1 || captured.Content.ImageURLs[0] != images[0] {
		t.Fatalf("analyzer Content.ImageURLs = %v, want %v", captured.Content.ImageURLs, images)
	}
	if len(linkStore.UpdateAnalysisCalls) != 1 {
		t.Fatalf("analysis updates = %d, want 1", len(linkStore.UpdateAnalysisCalls))
	}
	if got := linkStore.UpdateAnalysisCalls[0].Title; got == nil || *got != generatedTitle {
		t.Fatalf("persisted title = %#v, want generated short title %q", got, generatedTitle)
	}
	if linkStore.UpdateAnalysisCalls[0].IsLowConfidence {
		t.Fatalf("generated short title should recover title quality: %+v", linkStore.UpdateAnalysisCalls[0])
	}
}

func TestParsePipelineRunContinuesWhenExistingTagsCannotBeLoaded(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	now := time.Now().UTC()
	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {
			ID: linkID, URL: "https://example.com/articles/tag-failure",
			Status: model.LinkStatusPending, CreatedAt: now, UpdatedAt: now,
		},
	})

	var logs bytes.Buffer
	var gotExistingTags []string
	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Tags:             &pipelineFakeTagStore{err: errors.New("tag database unavailable")},
		Tree:             newPipelineTreeStore(nil),
		Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
			return fetcher.Content{
				URL: "https://example.com/articles/tag-failure", Title: "Useful title",
				Body: strings.Repeat("content ", 40), FetcherType: "basic",
			}, nil
		}),
		Analyzer: pipelineAnalyzerFunc(func(_ context.Context, req analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
			gotExistingTags = append([]string(nil), req.ExistingTags...)
			return analyzerpkg.AnalysisResult{Summary: "summary", Tags: []string{"new-tag"}}, nil
		}),
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})

	if err := pipeline.Run(context.Background(), pipelineAttempt(linkID)); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(gotExistingTags) != 0 {
		t.Fatalf("analyzer ExistingTags = %v, want empty", gotExistingTags)
	}
	if len(linkStore.UpdateAnalysisCalls) != 1 || linkStore.UpdateAnalysisCalls[0].Status != model.LinkStatusDone {
		t.Fatalf("analysis updates = %#v, want one done update", linkStore.UpdateAnalysisCalls)
	}
	if !strings.Contains(logs.String(), "WARN") || !strings.Contains(logs.String(), "tag database unavailable") {
		t.Fatalf("logs = %q, want WARN containing tag load error", logs.String())
	}
}

func TestParsePipelineRunLimitsExistingTagsWithoutReordering(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	now := time.Now().UTC()
	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {
			ID: linkID, URL: "https://example.com/articles/many-tags",
			Status: model.LinkStatusPending, CreatedAt: now, UpdatedAt: now,
		},
	})
	existing := make([]string, 75)
	for i := range existing {
		existing[i] = fmt.Sprintf("tag-%02d", i)
	}

	var got []string
	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Tags:             &pipelineFakeTagStore{tags: existing},
		Tree:             newPipelineTreeStore(nil),
		Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
			return fetcher.Content{
				URL: "https://example.com/articles/many-tags", Title: "Useful title",
				Body: strings.Repeat("content ", 40), FetcherType: "basic",
			}, nil
		}),
		Analyzer: pipelineAnalyzerFunc(func(_ context.Context, req analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
			got = append([]string(nil), req.ExistingTags...)
			return analyzerpkg.AnalysisResult{Summary: "summary", Tags: []string{"tag-00"}}, nil
		}),
	})

	if err := pipeline.Run(context.Background(), pipelineAttempt(linkID)); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(got) != 50 {
		t.Fatalf("len(analyzer ExistingTags) = %d, want 50", len(got))
	}
	for i, tag := range got {
		if tag != existing[i] {
			t.Fatalf("analyzer ExistingTags[%d] = %q, want %q", i, tag, existing[i])
		}
	}
}

func TestParsePipelineRunContinuesWithoutParentWhenTreeLookupFails(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	now := time.Now().UTC()
	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {
			ID: linkID, URL: "https://example.com/articles/tree-failure",
			Status: model.LinkStatusPending, CreatedAt: now, UpdatedAt: now,
		},
	})
	treeStore := newPipelineTreeStore(nil)
	treeStore.LookupByURLsFunc = func(context.Context, []string) (map[string]*model.Link, error) {
		return nil, errors.New("tree database unavailable")
	}

	var logs bytes.Buffer
	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Tags:             &pipelineFakeTagStore{},
		Tree:             treeStore,
		Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
			return fetcher.Content{
				URL: "https://example.com/articles/tree-failure", Title: "Useful title",
				Body: strings.Repeat("content ", 40), FetcherType: "basic",
			}, nil
		}),
		Analyzer: pipelineAnalyzerFunc(func(context.Context, analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
			return analyzerpkg.AnalysisResult{Summary: "summary", Tags: []string{"tree"}}, nil
		}),
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})

	if err := pipeline.Run(context.Background(), pipelineAttempt(linkID)); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linkStore.UpdateAnalysisCalls) != 1 {
		t.Fatalf("analysis updates = %d, want 1", len(linkStore.UpdateAnalysisCalls))
	}
	got := linkStore.UpdateAnalysisCalls[0]
	if got.Status != model.LinkStatusDone || got.ParentID != nil {
		t.Fatalf("analysis status/parent = %q/%v, want done/nil", got.Status, got.ParentID)
	}
	if len(linkStore.MarkFailedCalls) != 0 {
		t.Fatalf("failed state writes = %d, want 0", len(linkStore.MarkFailedCalls))
	}
	if !strings.Contains(logs.String(), "WARN") || !strings.Contains(logs.String(), "tree database unavailable") {
		t.Fatalf("logs = %q, want WARN containing tree lookup error", logs.String())
	}
}

func TestParsePipelineRegressionCorpus(t *testing.T) {
	t.Parallel()

	cases := loadPipelineRegressionCorpus(t)
	for _, tt := range cases {
		tt := tt
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			linkID := uuid.New()
			rootID := uuid.New()
			now := time.Now().UTC()

			linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
				linkID: {
					ID:        linkID,
					URL:       tt.URL,
					Status:    model.LinkStatusPending,
					CreatedAt: now,
					UpdatedAt: now,
				},
			})
			treeStore := newPipelineTreeStore(
				map[string]*model.Link{
					"https://example.com/": {
						ID:        rootID,
						URL:       "https://example.com/",
						Status:    model.LinkStatusDone,
						CreatedAt: now,
						UpdatedAt: now,
					},
				},
			)

			pipeline := NewParsePipeline(ParsePipelineOptions{
				Links:            linkStore,
				ReadingCompleter: linkStore,
				SiteCompleter:    linkStore,
				Tags:             &pipelineFakeTagStore{tags: []string{"Go", "AI", "Docs"}},
				Tree:             treeStore,
				Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
					if tt.FetchError != "" {
						return fetcher.Content{}, &fetcher.FetchError{URL: tt.URL, Reason: tt.FetchError}
					}
					return fetcher.Content{
						URL:         tt.URL,
						Title:       "Fetched title",
						Body:        tt.FetchBody,
						FetcherType: tt.FetcherType,
					}, nil
				}),
				Analyzer: pipelineAnalyzerFunc(func(context.Context, analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
					if tt.FetchError != "" {
						t.Fatal("Analyze() should not be called when fetch fails")
					}
					return analyzerpkg.AnalysisResult{Summary: tt.Summary, Tags: append([]string(nil), tt.Tags...)}, nil
				}),
			})

			err := pipeline.Run(context.Background(), pipelineAttempt(linkID))

			switch tt.WantStatus {
			case string(model.LinkStatusDone):
				if err != nil {
					t.Fatalf("Run() error = %v, want success", err)
				}
				if len(linkStore.UpdateAnalysisCalls) == 0 {
					t.Fatal("expected successful analysis update")
				}
				last := linkStore.UpdateAnalysisCalls[len(linkStore.UpdateAnalysisCalls)-1]
				if last.Status != model.LinkStatusDone {
					t.Fatalf("analysis status = %q, want done", last.Status)
				}
				if tt.WantLowConfidenceReason != "" {
					if !last.IsLowConfidence {
						t.Fatal("expected low confidence analysis")
					}
					if last.LowConfidenceReason == nil || *last.LowConfidenceReason != tt.WantLowConfidenceReason {
						t.Fatalf("low confidence reason = %#v, want %q", last.LowConfidenceReason, tt.WantLowConfidenceReason)
					}
				}
			case string(model.LinkStatusFailed):
				if !errors.Is(err, errsafe.ErrAlreadyPersisted) {
					t.Fatalf("Run() error = %v, want errsafe.ErrAlreadyPersisted", err)
				}
				if len(linkStore.MarkFailedCalls) == 0 {
					t.Fatal("expected failure state update")
				}
				last := linkStore.MarkFailedCalls[len(linkStore.MarkFailedCalls)-1]
				if last.Attempt != pipelineAttempt(linkID) {
					t.Fatalf("failed attempt = %#v, want %#v", last.Attempt, pipelineAttempt(linkID))
				}
			default:
				t.Fatalf("unsupported want status %q", tt.WantStatus)
			}
		})
	}
}

func loadPipelineRegressionCorpus(t *testing.T) []pipelineRegressionCase {
	t.Helper()

	data, err := os.ReadFile("testdata/pipeline_regression_corpus.json")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var cases []pipelineRegressionCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("pipeline regression corpus is empty")
	}
	return cases
}

// newPipelineLinkStore wires an ObservableLinkStore with the no-op
// write hooks the pipeline tests need. Tests that want to observe
// state transitions in order (the "events ordering" idiom) replace
// UpdateStateFunc / UpdateAnalysisFunc with their own recorder; the
// observable still records the params into UpdateStateCalls /
// UpdateAnalysisCalls automatically (the wrapper records before
// dispatching to the hook).
func newPipelineLinkStore(byID map[uuid.UUID]*model.Link) *repotest.ObservableLinkStore {
	for _, link := range byID {
		if link.ParseGeneration == 0 {
			link.ParseGeneration = 1
		}
		if link.MetadataRevision == 0 {
			link.MetadataRevision = 1
		}
	}
	return &repotest.ObservableLinkStore{
		ByID:                    byID,
		MarkParseProcessingFunc: func(context.Context, model.ParseAttempt) error { return nil },
		MarkParseFailedFunc:     func(context.Context, model.ParseAttempt, string) error { return nil },
	}
}

func pipelineAttempt(linkID uuid.UUID) model.ParseAttempt {
	return model.ParseAttempt{LinkID: linkID, Generation: 1, ExpectedMetadataRevision: 1}
}

func newPipelineTreeStore(lookups map[string]*model.Link) *repotest.ObservableTreeStore {
	return &repotest.ObservableTreeStore{
		Lookups: lookups,
	}
}

type pipelineFakeTagStore struct {
	tags []string
	err  error
}

func (s *pipelineFakeTagStore) ListDistinct(context.Context) ([]string, error) {
	return append([]string(nil), s.tags...), s.err
}

func (s *pipelineFakeTagStore) ListCounts(context.Context) ([]repository.TagCount, error) {
	return nil, s.err
}

type pipelineFetcherFunc func(context.Context, string) (fetcher.Content, error)

func (fn pipelineFetcherFunc) Fetch(ctx context.Context, rawURL string) (fetcher.Content, error) {
	return fn(ctx, rawURL)
}

type pipelineAnalyzerFunc func(context.Context, analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error)

func (fn pipelineAnalyzerFunc) Analyze(ctx context.Context, req analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
	return fn(ctx, req)
}

func TestParsePipelineRecomputesTerminalDecisionAfterIntentRaceWithoutReanalyzing(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	rawURL := "https://intent-race.example/article"
	initial := &model.Link{
		ID: linkID, URL: rawURL, Status: model.LinkStatusPending, MetadataRevision: 1, ParseGeneration: 1,
	}
	site := model.LibraryKindSite
	siteIntent := *initial
	siteIntent.LibraryKind = &site
	siteIntent.LibraryKindLocked = true
	reading := model.LibraryKindReading
	readingIntent := *initial
	readingIntent.LibraryKind = &reading
	readingIntent.LibraryKindLocked = true

	linkReads := 0
	links := newPipelineLinkStore(nil)
	links.GetByIDFunc = func(context.Context, uuid.UUID) (*model.Link, error) {
		linkReads++
		switch linkReads {
		case 1:
			return initial, nil
		case 2:
			return &siteIntent, nil
		default:
			return &readingIntent, nil
		}
	}
	links.CompleteSiteParseFunc = func(context.Context, repository.CompleteSiteParseParams) (repository.SiteAggregateResult, error) {
		return repository.SiteAggregateResult{}, repository.ErrLibrarySelectionChanged
	}
	links.CompleteReadingParseFunc = func(context.Context, repository.CompleteReadingParseParams) (repository.CompleteReadingParseResult, error) {
		return repository.CompleteReadingParseResult{MetadataRevision: 1, MetadataApplied: true}, nil
	}
	analyzerCalls := 0
	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            links,
		ReadingCompleter: links,
		SiteCompleter:    links,
		Tags:             &pipelineFakeTagStore{},
		Tree:             newPipelineTreeStore(nil),
		Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
			return fetcher.Content{URL: rawURL, Title: "Intent race", Body: "article body", FetcherType: "basic"}, nil
		}),
		Analyzer: pipelineAnalyzerFunc(func(context.Context, analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
			analyzerCalls++
			return analyzerpkg.AnalysisResult{
				Title: "Intent race", Summary: "summary", LibraryKind: model.LibraryKindReading,
			}, nil
		}),
	})

	if err := pipeline.Run(context.Background(), pipelineAttempt(linkID)); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if analyzerCalls != 1 {
		t.Fatalf("analyzer calls = %d, want 1", analyzerCalls)
	}
	if len(links.CompleteSiteParseCalls) != 1 || len(links.CompleteReadingParseCalls) != 1 {
		t.Fatalf("terminal attempts site/reading = %d/%d, want 1/1", len(links.CompleteSiteParseCalls), len(links.CompleteReadingParseCalls))
	}
	first := links.CompleteSiteParseCalls[0].Params
	if first.ExpectedLibraryKind == nil || *first.ExpectedLibraryKind != model.LibraryKindSite ||
		!first.ExpectedLibraryKindLocked || first.Classification.Kind != model.LibraryKindSite || !first.Classification.Locked {
		t.Fatalf("first terminal decision = %#v", first)
	}
	second := links.CompleteReadingParseCalls[0].Params
	if second.ExpectedLibraryKind == nil || *second.ExpectedLibraryKind != model.LibraryKindReading ||
		!second.ExpectedLibraryKindLocked || second.Classification.Kind != model.LibraryKindReading || !second.Classification.Locked {
		t.Fatalf("recomputed terminal decision = %#v", second)
	}
}

func assertEventOrder(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) < len(want) {
		t.Fatalf("events = %#v, want at least %#v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events[%d] = %q, want %q; full events=%#v", i, got[i], want[i], got)
		}
	}
}

// TestParsePipelineRunUsesReadingCompleterWithClassification 是阶段1的回归锁：
// 全链路 Run 必须终结于 CompleteReadingParse 并携带完整分类参数和代次栅栏。
func TestParsePipelineRunUsesReadingCompleterWithClassification(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("77777777-aaaa-7777-aaaa-777777777777")
	now := time.Now().UTC()

	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {ID: linkID, URL: "https://example.com/posts/9", Status: model.LinkStatusPending, CreatedAt: now, UpdatedAt: now},
	})

	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Tags:             &pipelineFakeTagStore{},
		Tree:             newPipelineTreeStore(map[string]*model.Link{}),
		Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
			return fetcher.Content{URL: "https://example.com/posts/9", Title: "标题", Body: strings.Repeat("正文内容 ", 200), FetcherType: "basic"}, nil
		}),
		Analyzer: pipelineAnalyzerFunc(func(context.Context, analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
			return analyzerpkg.AnalysisResult{Summary: "摘要", Tags: []string{"Go"}, LibraryKind: model.LibraryKindReading}, nil
		}),
	})

	if err := pipeline.Run(context.Background(), pipelineAttempt(linkID)); err != nil && !errors.Is(err, errsafe.ErrAlreadyPersisted) {
		t.Fatalf("Run() error = %v", err)
	}

	if len(linkStore.CompleteReadingParseCalls) != 1 {
		t.Fatalf("CompleteReadingParse 调用 = %d, want 1", len(linkStore.CompleteReadingParseCalls))
	}
	got := linkStore.CompleteReadingParseCalls[0]
	if got.Params.Analysis.ExpectedParseGeneration != 1 || got.Params.Analysis.ExpectedMetadataRevision != 1 {
		t.Fatalf("terminal fences = generation %d metadata %d, want 1/1", got.Params.Analysis.ExpectedParseGeneration, got.Params.Analysis.ExpectedMetadataRevision)
	}
	if got.Params.Analysis.Status != model.LinkStatusDone {
		t.Fatalf("status = %v, want done", got.Params.Analysis.Status)
	}
	classification := got.Params.Classification
	if classification.Kind != model.LibraryKindReading {
		t.Fatalf("classification kind = %v, want reading", classification.Kind)
	}
	if classification.ID != linkID {
		t.Fatalf("classification link id = %v, want %v", classification.ID, linkID)
	}
}

// TestParsePipelineRunUsesSiteCompleterWithAggregateParams 是 site 分支的全链路锁。
//
// reading 分支早有同类用例（见上），site 分支一直没有——后果是 site 终态的落库
// 参数在 service 层从未被严格校验过。验证方式：把 siteAggregateParams 改成产出
// 空 Name/EntryName（那会让生产每条 site 解析都失败于 aggregate site 的守卫），
// 在本用例存在之前，全仓 37 个包无一变红。
//
// 关键在于用 repotest.ObservableLinkStore 而非局部 fake：它的 CompleteSiteParse
// 直接调用 repository.ValidateAggregateSiteParams，
// 即生产实现本身，因此这条用例等价于让生产守卫参与 service 层测试。
func TestParsePipelineRunUsesSiteCompleterWithAggregateParams(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("99999999-cccc-9999-cccc-999999999999")
	now := time.Now().UTC()

	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {ID: linkID, URL: "https://tool.example.com/", Status: model.LinkStatusPending, CreatedAt: now, UpdatedAt: now},
	})

	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Tags:             &pipelineFakeTagStore{},
		Tree:             newPipelineTreeStore(map[string]*model.Link{}),
		Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
			return fetcher.Content{URL: "https://tool.example.com/", Title: "工具站", Body: strings.Repeat("站点介绍内容 ", 200), FetcherType: "basic"}, nil
		}),
		Analyzer: pipelineAnalyzerFunc(func(context.Context, analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
			return analyzerpkg.AnalysisResult{
				LibraryKind:  model.LibraryKindSite,
				SiteName:     "Example Tool",
				SiteIntro:    "有用的集成工具",
				EntryName:    "首页",
				EntryPurpose: "了解产品",
				Tags:         []string{"Tool"},
			}, nil
		}),
	})

	if err := pipeline.Run(context.Background(), pipelineAttempt(linkID)); err != nil && !errors.Is(err, errsafe.ErrAlreadyPersisted) {
		t.Fatalf("Run() error = %v", err)
	}

	if len(linkStore.CompleteSiteParseCalls) != 1 {
		t.Fatalf("CompleteSiteParse 调用 = %d, want 1", len(linkStore.CompleteSiteParseCalls))
	}
	if len(linkStore.CompleteReadingParseCalls) != 0 {
		t.Fatalf("site 结果不应走 reading 终态，实际 %d 次", len(linkStore.CompleteReadingParseCalls))
	}

	got := linkStore.CompleteSiteParseCalls[0]
	if got.Params.Classification.Kind != model.LibraryKindSite {
		t.Fatalf("classification kind = %q, want site", got.Params.Classification.Kind)
	}
	// 聚合参数逐项断言——生产的 ValidateAggregateSiteParams 要求这五项齐备，
	// fake 已代为把关，这里再显式钉住取值来源。
	site := got.Params.Site
	if site.LinkID != linkID {
		t.Fatalf("site link id = %v, want %v", site.LinkID, linkID)
	}
	if strings.TrimSpace(site.IdentityKey) == "" || strings.TrimSpace(site.NormalizedURL) == "" {
		t.Fatalf("site identity 未派生：key=%q url=%q", site.IdentityKey, site.NormalizedURL)
	}
	if site.Name != "Example Tool" || site.EntryName != "首页" {
		t.Fatalf("site name/entry = %q/%q, want Example Tool/首页", site.Name, site.EntryName)
	}
}
