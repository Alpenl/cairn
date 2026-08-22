package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"webtag/internal/errsafe"
	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/repository/repotest"
	analyzerpkg "webtag/internal/service/analyzer"
)

// urlDirectHarness wires a minimal pipeline with URLDirect enabled and a
// fetcher that records whether it was called (so a test can assert the
// grok-direct path skipped local fetching vs fell back to it).
func urlDirectHarness(t *testing.T, sourceKind string, analyzer analyzerpkg.Analyzer) (*ParsePipeline, model.ParseAttempt, *bool, *repotest.ObservableLinkStore) {
	t.Helper()
	linkID := uuid.MustParse("a1a1a1a1-0000-0000-0000-000000000001")
	attempt := model.ParseAttempt{LinkID: linkID, Generation: 1, ExpectedMetadataRevision: 1}
	now := time.Now().UTC()

	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {
			ID: linkID, URL: "https://x.com/example/status/2073708506319098344", SourceKind: sourceKind,
			Status: model.LinkStatusPending, MetadataRevision: 1, ParseGeneration: 1, CreatedAt: now, UpdatedAt: now,
		},
	})
	tagStore := &pipelineFakeTagStore{tags: []string{"Go"}}
	treeStore := newPipelineTreeStore(map[string]*model.Link{
		"https://example.com/": {ID: uuid.New(), URL: "https://example.com/", Status: model.LinkStatusDone, CreatedAt: now, UpdatedAt: now},
	})

	fetched := false
	fetch := pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
		fetched = true
		return fetcher.Content{URL: "https://x.com/example/status/2073708506319098344", Title: "Fetched title", Body: "Fetched body", FetcherType: "basic"}, nil
	})

	p := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Tags:             tagStore,
		Tree:             treeStore,
		Fetcher:          fetch,
		Analyzer:         analyzer,
		URLDirect:        true,
	})
	return p, attempt, &fetched, linkStore
}

func TestURLDirectPrefersFetchedContentForStructuredArticle(t *testing.T) {
	t.Parallel()
	linkID := uuid.MustParse("a1a1a1a1-0000-0000-0000-000000000001")

	var gotRequest analyzerpkg.AnalyzeRequest
	analyzer := pipelineAnalyzerFunc(func(_ context.Context, req analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
		gotRequest = req
		return analyzerpkg.AnalysisResult{
			Accessible: true,
			Title:      "API design",
			Summary:    "A structured article summary.",
			Tags:       []string{"API", "接口设计"},
		}, nil
	})

	pipeline, attempt, fetched, linkStore := urlDirectHarness(t, "url", analyzer)
	linkStore.ByID[linkID].URL = "https://www.seangoedecke.com/good-api-design/"
	if err := pipeline.Run(context.Background(), attempt); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !*fetched {
		t.Fatal("structured article should be fetched locally before Grok analysis")
	}
	if gotRequest.URLDirect {
		t.Fatal("structured article was sent through unreliable URL-direct analysis")
	}
	if gotRequest.Content.Body != "Fetched body" {
		t.Fatalf("analyzer body = %q, want fetched content", gotRequest.Content.Body)
	}
}

// TestURLDirectAccessibleSkipsFetcher: when the model reports it could fetch
// the URL, the local fetcher must NOT run and the link is persisted with the
// model's title/summary/tags and fetcher_type "grok".
func TestURLDirectAccessibleSkipsFetcher(t *testing.T) {
	t.Parallel()

	var gotURLDirect bool
	analyzer := pipelineAnalyzerFunc(func(_ context.Context, req analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
		gotURLDirect = req.URLDirect
		return analyzerpkg.AnalysisResult{
			Accessible: true,
			Title:      "Grok 抓到的标题",
			Summary:    "- 要点一\n- 要点二",
			Tags:       []string{"Go", "错误处理"},
		}, nil
	})

	p, attempt, fetched, linkStore := urlDirectHarness(t, "url", analyzer)
	if err := p.Run(context.Background(), attempt); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !gotURLDirect {
		t.Error("analyzer was not called in URLDirect mode")
	}
	if *fetched {
		t.Error("local fetcher ran despite accessible URL-direct result; should have been skipped")
	}
	if len(linkStore.UpdateAnalysisCalls) != 1 {
		t.Fatalf("analysis updates = %d, want 1", len(linkStore.UpdateAnalysisCalls))
	}
	got := linkStore.UpdateAnalysisCalls[0]
	if got.Status != model.LinkStatusDone {
		t.Fatalf("status = %q, want done", got.Status)
	}
	if got.Title == nil || *got.Title != "Grok 抓到的标题" {
		t.Errorf("title = %#v, want model title", got.Title)
	}
	if got.FetcherType == nil || *got.FetcherType != "grok" {
		t.Errorf("fetcher_type = %#v, want grok", got.FetcherType)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "Go" {
		t.Errorf("tags = %#v, want [Go 错误处理]", got.Tags)
	}
}

func TestURLDirectLoadsExistingTagsForCanonicalReuse(t *testing.T) {
	t.Parallel()

	var gotExistingTags []string
	analyzer := pipelineAnalyzerFunc(func(_ context.Context, req analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
		gotExistingTags = append([]string(nil), req.ExistingTags...)
		return analyzerpkg.AnalysisResult{
			Accessible: true,
			Title:      "Canonical tag reuse",
			Summary:    "A summary long enough to persist without a fallback.",
			Tags:       []string{"Go", "错误处理"},
		}, nil
	})

	pipeline, attempt, fetched, _ := urlDirectHarness(t, "url", analyzer)
	if err := pipeline.Run(context.Background(), attempt); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if *fetched {
		t.Fatal("local fetcher ran despite an accessible URL-direct result")
	}
	if len(gotExistingTags) != 1 || gotExistingTags[0] != "Go" {
		t.Fatalf("URL-direct ExistingTags = %#v, want [Go]", gotExistingTags)
	}
}

func TestURLDirectDoesNotPersistSocialPostBodyAsTitle(t *testing.T) {
	t.Parallel()
	linkID := uuid.MustParse("a1a1a1a1-0000-0000-0000-000000000001")
	const longPostTitle = "(19) X 上的 GitHubDaily：“想验证自己的选股思路，现成炒股软件改不动，自己搭环境又太折腾。tickflow-stock-panel 是个自托管的 A 股量化工作台，选股、监控、回测三件事装进一个面板。” / X"

	analyzer := pipelineAnalyzerFunc(func(_ context.Context, req analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
		return analyzerpkg.AnalysisResult{
			Accessible: true,
			Title:      longPostTitle,
			Summary:    "tickflow-stock-panel 是一个自托管的 A 股量化工作台，整合选股、监控和回测功能。",
			Tags:       []string{"A股", "量化交易"},
		}, nil
	})

	pipeline, attempt, fetched, linkStore := urlDirectHarness(t, "url", analyzer)
	linkStore.ByID[linkID].URL = "https://x.com/GitHub_Daily/status/1941500000000000000"
	if err := pipeline.Run(context.Background(), attempt); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if *fetched {
		t.Fatal("local fetcher ran despite an accessible URL-direct result")
	}
	if len(linkStore.UpdateAnalysisCalls) != 1 {
		t.Fatalf("analysis updates = %d, want 1", len(linkStore.UpdateAnalysisCalls))
	}

	got := linkStore.UpdateAnalysisCalls[0].Title
	if got == nil {
		t.Fatal("persisted title is nil; want a generated short title")
	}
	if *got == longPostTitle || strings.Contains(*got, "现成炒股软件改不动") {
		t.Fatalf("persisted title copied the social post body: %q", *got)
	}
	if runes := utf8.RuneCountInString(*got); runes > 36 {
		t.Fatalf("persisted title has %d runes, want at most 36: %q", runes, *got)
	}
}

// TestURLDirectInaccessibleFallsBackToFetcher: when the model reports it
// could NOT access the URL, the pipeline must fall back to the local fetcher
// and still complete via the fetched-content analyze path.
func TestURLDirectInaccessibleFallsBackToFetcher(t *testing.T) {
	t.Parallel()

	calls := 0
	analyzer := pipelineAnalyzerFunc(func(_ context.Context, req analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
		calls++
		if req.URLDirect {
			return analyzerpkg.AnalysisResult{Accessible: false}, nil
		}
		// fallback fetched-content analyze
		return analyzerpkg.AnalysisResult{Accessible: true, Summary: "回退摘要", Tags: []string{"Go", "AI"}}, nil
	})

	p, attempt, fetched, linkStore := urlDirectHarness(t, "", analyzer)
	if err := p.Run(context.Background(), attempt); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !*fetched {
		t.Error("local fetcher did not run despite inaccessible URL-direct result; fallback failed")
	}
	if calls != 2 {
		t.Errorf("analyzer calls = %d, want 2 (url-direct then fallback)", calls)
	}
	if len(linkStore.UpdateAnalysisCalls) != 1 {
		t.Fatalf("analysis updates = %d, want 1", len(linkStore.UpdateAnalysisCalls))
	}
	got := linkStore.UpdateAnalysisCalls[0]
	if got.FetcherType == nil || *got.FetcherType != "basic" {
		t.Errorf("fetcher_type = %#v, want basic (fallback path)", got.FetcherType)
	}
}

func TestURLDirectSecurityFailureDoesNotFallbackOrResubmit(t *testing.T) {
	t.Parallel()

	calls := 0
	analyzer := pipelineAnalyzerFunc(func(context.Context, analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
		calls++
		return analyzerpkg.AnalysisResult{}, fetcher.ErrUnsafeRedirect
	})
	pipeline, attempt, fetched, linkStore := urlDirectHarness(t, "url", analyzer)

	err := pipeline.Run(context.Background(), attempt)
	if !errors.Is(err, errsafe.ErrAlreadyPersisted) || !errors.Is(err, fetcher.ErrUnsafeRedirect) {
		t.Fatalf("Run() error = %v, want persisted unsafe redirect", err)
	}
	if *fetched {
		t.Fatal("local fetcher ran after a permanent provider security rejection")
	}
	if calls != 1 {
		t.Fatalf("analyzer calls=%d, want one URL-direct attempt", calls)
	}
	if len(linkStore.MarkFailedCalls) != 1 {
		t.Fatalf("failed state writes=%d, want 1", len(linkStore.MarkFailedCalls))
	}
}

func TestURLDirectFailureLogRedactsUpstreamSecrets(t *testing.T) {
	t.Parallel()
	const secret = "sk-abcdef0123456789ABCDEF"

	calls := 0
	analyzer := pipelineAnalyzerFunc(func(_ context.Context, req analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
		calls++
		if req.URLDirect {
			return analyzerpkg.AnalysisResult{}, errors.New("upstream rejected key " + secret)
		}
		return analyzerpkg.AnalysisResult{Summary: "fallback summary", Tags: []string{"safe"}}, nil
	})

	pipeline, attempt, fetched, _ := urlDirectHarness(t, "url", analyzer)
	var logs bytes.Buffer
	pipeline.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if err := pipeline.Run(context.Background(), attempt); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !*fetched || calls != 2 {
		t.Fatalf("fallback fetched/calls = %v/%d, want true/2", *fetched, calls)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("url-direct fallback log leaked upstream secret: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "sk-<redacted>") {
		t.Fatalf("url-direct fallback log missing redaction marker: %q", logs.String())
	}
}

func TestURLDirectUsesStoredContentForNonURLSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sourceKind string
		inputText  string
		images     []string
	}{
		{name: "browser capture", sourceKind: "browser_capture", inputText: "captured browser text"},
		{name: "text", sourceKind: "text", inputText: "pasted text"},
		{name: "image", sourceKind: "image", images: []string{"https://cdn.example.com/image.png"}},
		{name: "multimodal", sourceKind: "multimodal", inputText: "multimodal text", images: []string{"https://cdn.example.com/context.png"}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			linkID := uuid.New()
			attempt := model.ParseAttempt{LinkID: linkID, Generation: 1, ExpectedMetadataRevision: 1}
			now := time.Now().UTC()
			title := "Stored title"
			var inputText *string
			if tt.inputText != "" {
				inputText = &tt.inputText
			}

			linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
				linkID: {
					ID:               linkID,
					URL:              "https://example.com/articles/captured",
					SourceKind:       tt.sourceKind,
					InputTitle:       &title,
					InputText:        inputText,
					InputImages:      append([]string(nil), tt.images...),
					Status:           model.LinkStatusPending,
					MetadataRevision: 1,
					ParseGeneration:  1,
					CreatedAt:        now,
					UpdatedAt:        now,
				},
			})
			fetcherCalls := 0
			analyzerCalls := 0
			pipeline := NewParsePipeline(ParsePipelineOptions{
				Links:            linkStore,
				ReadingCompleter: linkStore,
				SiteCompleter:    linkStore,
				Tags:             &pipelineFakeTagStore{},
				Tree:             newPipelineTreeStore(nil),
				Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
					fetcherCalls++
					return fetcher.Content{}, nil
				}),
				Analyzer: pipelineAnalyzerFunc(func(_ context.Context, req analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
					analyzerCalls++
					if req.URLDirect {
						t.Fatal("non-URL source was analyzed through URL-direct")
					}
					if req.Content.SourceKind != tt.sourceKind {
						t.Fatalf("Content.SourceKind = %q, want %q", req.Content.SourceKind, tt.sourceKind)
					}
					if req.Content.Body != tt.inputText {
						t.Fatalf("Content.Body = %q, want %q", req.Content.Body, tt.inputText)
					}
					if len(req.Content.ImageURLs) != len(tt.images) {
						t.Fatalf("Content.ImageURLs = %v, want %v", req.Content.ImageURLs, tt.images)
					}
					return analyzerpkg.AnalysisResult{Summary: "stored content summary", Tags: []string{"saved"}}, nil
				}),
				URLDirect: true,
			})

			if err := pipeline.Run(context.Background(), attempt); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if fetcherCalls != 0 {
				t.Fatalf("fetcher calls = %d, want 0", fetcherCalls)
			}
			if analyzerCalls != 1 {
				t.Fatalf("analyzer calls = %d, want 1", analyzerCalls)
			}
		})
	}
}
