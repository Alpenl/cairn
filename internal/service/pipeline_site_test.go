package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
	"webtag/internal/service/analyzer"
	"webtag/internal/service/urlmeta"
)

type recordingSiteCompleter struct {
	params repository.CompleteSiteParseParams
	jobID  uuid.UUID
	calls  int
}

type recordingReadingCompleter struct {
	params repository.CompleteReadingParseParams
	jobID  uuid.UUID
	calls  int
}

// CompleteReadingParse 复刻 PGXLinkRepository.CompleteReadingParse 的前置校验。
// 这两个 recording fake 只在本文件使用，若比生产宽松，本文件的用例就会对
// 「归属非法」这类回归完全免疫——空归属那次回归正是被同类宽松 fake 掩盖的。
func (r *recordingReadingCompleter) CompleteReadingParse(_ context.Context, params repository.CompleteReadingParseParams, jobID uuid.UUID) (repository.CompleteReadingParseResult, error) {
	// 校验先于记录，与 repotest.ObservableLinkStore 保持同一口径：生产在守卫
	// 失败时回滚整个事务，不留下任何写入痕迹。
	// 直接复用 repotest 的校验，与 ObservableLinkStore 同一份实现——它本身又
	// 直接调用生产的 repository.Validate* 函数，因此三者不可能漂移。
	if err := repotest.ValidateReadingParse(params); err != nil {
		return repository.CompleteReadingParseResult{}, err
	}
	r.params, r.jobID, r.calls = params, jobID, r.calls+1
	revision := params.Analysis.ExpectedMetadataRevision
	if revision <= 0 {
		return repository.CompleteReadingParseResult{MetadataRevision: revision, MetadataApplied: false}, nil
	}
	return repository.CompleteReadingParseResult{MetadataRevision: revision, MetadataApplied: true}, nil
}

// CompleteSiteParse 同样复刻生产的两条前置校验，理由见 recordingReadingCompleter。
func (r *recordingSiteCompleter) CompleteSiteParse(_ context.Context, params repository.CompleteSiteParseParams, jobID uuid.UUID) (repository.SiteAggregateResult, error) {
	// 校验先于记录，理由同 recordingReadingCompleter。
	// 同样复用 repotest 的校验，理由见 recordingReadingCompleter。
	if err := repotest.ValidateSiteParse(params); err != nil {
		return repository.SiteAggregateResult{}, err
	}
	r.params, r.jobID, r.calls = params, jobID, r.calls+1
	return repository.SiteAggregateResult{SiteID: uuid.New(), EntryID: uuid.New()}, nil
}

func TestCollectionFinalizerCompletesSiteThroughAtomicCompleter(t *testing.T) {
	completer := &recordingSiteCompleter{}
	finalizer := collectionFinalizer{siteCompleter: completer}
	linkID, jobID := uuid.New(), uuid.New()
	result := analyzer.AnalysisResult{LibraryKind: model.LibraryKindSite, ClassificationConfidence: .91, ClassificationReason: "ai_site_resolution", ClassificationExplanation: "交互工具为主", SiteName: "Example Tool", SiteIntro: "用于演示的网站工具。", EntryName: "工具主页", EntryPurpose: "在线使用工具", Tags: []string{"工具"}}
	err := finalizer.Finalize(context.Background(), collectionFinalizationRequest{LinkID: linkID, JobID: jobID, URL: "https://www.example.com/tool?utm_source=x", URLMeta: urlmeta.URLMetadata{Domain: "example.com", ContentType: model.ContentTypeHomepage}, Content: fetcher.Content{URL: "https://www.example.com/tool?utm_source=x", FetcherType: "http"}, Result: result, FetcherType: "http", RequestedKind: model.RequestedLibraryKindAuto, RecordFail: func(string, error) { t.Fatal("site completion should not fail") }})
	if err != nil {
		t.Fatal(err)
	}
	if completer.calls != 1 || completer.jobID != jobID {
		t.Fatalf("site completer calls=%d job=%s", completer.calls, completer.jobID)
	}
	if got := completer.params.Site; got.IdentityKey != "v1:host:example.com" || got.NormalizedURL != "https://example.com/tool" {
		t.Fatalf("site params = %#v", got)
	}
	if got := completer.params.Classification; got.Kind != model.LibraryKindSite || got.Source != model.LibraryKindSourceAuto || got.Locked {
		t.Fatalf("classification = %#v", got)
	}
}

func TestCollectionFinalizerKeepsExplicitSiteLocked(t *testing.T) {
	completer := &recordingSiteCompleter{}
	finalizer := collectionFinalizer{siteCompleter: completer}
	result := analyzer.AnalysisResult{LibraryKind: model.LibraryKindSite, ClassificationConfidence: 1, ClassificationReason: "explicit_site", ClassificationExplanation: "用户选择", SiteName: "Example", SiteIntro: "简介", EntryName: "首页", EntryPurpose: "用途"}
	err := finalizer.Finalize(context.Background(), collectionFinalizationRequest{LinkID: uuid.New(), JobID: uuid.New(), URL: "https://example.com", Content: fetcher.Content{URL: "https://example.com"}, Result: result, FetcherType: "http", RequestedKind: model.RequestedLibraryKindSite, RecordFail: func(string, error) { t.Fatal("site completion should not fail") }})
	if err != nil {
		t.Fatal(err)
	}
	if got := completer.params.Classification; got.Source != model.LibraryKindSourceUser || !got.Locked {
		t.Fatalf("classification = %#v", got)
	}
}

func TestCollectionFinalizerShadowsAutomaticSiteWhenAutoFlagDisabled(t *testing.T) {
	sites := &recordingSiteCompleter{}
	readings := &recordingReadingCompleter{}
	finalizer := collectionFinalizer{siteCompleter: sites, readingCompleter: readings, disableSiteAutoClassification: true}
	result := analyzer.AnalysisResult{LibraryKind: model.LibraryKindSite, ClassificationConfidence: .91, ClassificationReason: "ai_site_resolution", ClassificationExplanation: "interactive tool", SiteName: "Example Tool", SiteIntro: "Useful integrations", EntryName: "Home", EntryPurpose: "Use it"}
	err := finalizer.Finalize(context.Background(), collectionFinalizationRequest{LinkID: uuid.New(), JobID: uuid.New(), URL: "https://example.com", Content: fetcher.Content{URL: "https://example.com"}, Result: result, FetcherType: "http", RequestedKind: model.RequestedLibraryKindAuto, RecordFail: func(string, error) { t.Fatal("shadow completion should not fail") }})
	if err != nil {
		t.Fatal(err)
	}
	if sites.calls != 0 || readings.calls != 1 {
		t.Fatalf("site/reading completions = %d/%d, want 0/1", sites.calls, readings.calls)
	}
	got := readings.params.Classification
	if got.Kind != model.LibraryKindReading || got.PredictedKind == nil || *got.PredictedKind != model.LibraryKindSite || got.Locked {
		t.Fatalf("shadow classification = %#v", got)
	}
	if readings.params.Analysis.Summary == nil || *readings.params.Analysis.Summary != "Useful integrations" {
		t.Fatalf("shadow reading summary = %#v", readings.params.Analysis.Summary)
	}
}

func TestFinalizeAnalysisRuleIsAutomaticNotUserLocked(t *testing.T) {
	completer := &recordingSiteCompleter{}
	pipeline := &ParsePipeline{siteCompleter: completer}
	result := analyzer.AnalysisResult{LibraryKind: model.LibraryKindSite, ClassificationConfidence: .8, SiteName: "Example", SiteIntro: "简介", EntryName: "首页", EntryPurpose: "用途"}
	link := &model.Link{ID: uuid.New(), URL: "https://example.com"}
	err := pipeline.finalizeAnalysis(context.Background(), parseInputForTest(link), uuid.New(), 1, urlmeta.URLMetadata{}, fetcher.Content{URL: "https://example.com"}, result, nil, "http", model.RequestedLibraryKindAuto, &ClassificationRuleMatch{TargetKind: model.LibraryKindSite, Reason: "personal_rule_host"}, func(string, error) { t.Fatal("rule completion should not fail") })
	if err != nil {
		t.Fatal(err)
	}
	if got := completer.params.Classification; got.Source != model.LibraryKindSourceAuto || got.Locked || got.Reason == nil || *got.Reason != "personal_rule_host" {
		t.Fatalf("classification = %#v", got)
	}
}

func TestCollectionFinalizerReadingCompleterRunsPostWriteEnrichment(t *testing.T) {
	readings := &recordingReadingCompleter{}
	invalidator := &pipelineCacheInvalidator{}
	embedder := &recordingEmbedder{enabled: true, vec: []float32{0.25, 0.75}}
	writer := &recordingLinkEmbeddingWriter{}
	attacher := &recordingConceptAttacher{}
	finalizer := collectionFinalizer{
		readingCompleter:    readings,
		tagCacheInvalidator: invalidator,
		contentEmbedder:     embedder,
		linkEmbeddingWriter: writer,
		conceptAttacher:     attacher,
	}
	linkID, jobID := uuid.New(), uuid.New()
	result := analyzer.AnalysisResult{LibraryKind: model.LibraryKindReading, ClassificationConfidence: .73, ClassificationReason: "ai_reading", ClassificationExplanation: "article-like page", Title: "Reader Modules", Summary: "Explains reader module boundaries.", Tags: []string{"Go"}}

	err := finalizer.Finalize(context.Background(), collectionFinalizationRequest{
		LinkID:                   linkID,
		JobID:                    jobID,
		ExpectedMetadataRevision: 1,
		URL:                      "https://example.com/articles/finalizer",
		URLMeta:                  urlmeta.URLMetadata{Domain: "example.com", ContentType: model.ContentTypeArticle},
		Content:                  fetcher.Content{URL: "https://example.com/articles/finalizer", Title: "Reader Modules", Body: "The finalizer keeps the post-write enrichment path behind one terminal seam.", FetcherType: "http"},
		Result:                   result,
		FetcherType:              "http",
		RequestedKind:            model.RequestedLibraryKindAuto,
		RecordFail:               func(stage string, err error) { t.Fatalf("%s failed: %v", stage, err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if readings.calls != 1 || readings.jobID != jobID {
		t.Fatalf("reading completer calls=%d job=%s", readings.calls, readings.jobID)
	}
	if invalidator.calls != 1 {
		t.Fatalf("tag invalidations = %d, want 1", invalidator.calls)
	}
	if len(writer.calls) != 1 || writer.calls[0].id != linkID || writer.calls[0].parseJobID != jobID {
		t.Fatalf("embedding writes = %#v", writer.calls)
	}
	if len(attacher.calls) != 1 || attacher.calls[0].linkID != linkID || len(attacher.calls[0].tags) != 1 || attacher.calls[0].tags[0] != "Go" {
		t.Fatalf("concept attach calls = %#v", attacher.calls)
	}
}

// TestLockedSiteSurvivesReadingAnalysis 是 HIGH-2 的回归。
//
// 用户把阅读转成站点（convertReadingToSiteSQL 写 library_kind='site',
// locked=true）→ 点刷新（SubmitService.Refresh 对 library_kind 无限制）→ 模型
// 判 reading。analyzer 判 reading 时走 reading_profile 分支并直接返回，
// SiteName / EntryName 全为空；归属由锁定决定仍是 site，于是空值透传给
// ValidateAggregateSiteParams 并被拒绝，整条解析失败、链接落 failed。
//
// 「用户锁 A、模型认为 B」正是这把锁存在的唯一理由，不能让它反过来把解析打死。
func TestLockedSiteSurvivesReadingAnalysis(t *testing.T) {
	t.Parallel()

	site := model.LibraryKindSite
	link := &model.Link{
		ID: uuid.New(), URL: "https://tool.example.com/",
		LibraryKind: &site, LibraryKindLocked: true,
	}

	completer := &recordingSiteCompleter{}
	pipeline := &ParsePipeline{siteCompleter: completer}

	// 模型判 reading：只给文章字段，站点画像全空。
	result := analyzer.AnalysisResult{
		LibraryKind:              model.LibraryKindReading,
		Title:                    "深入理解某工具",
		Summary:                  "一篇讲该工具的长文",
		ClassificationConfidence: 0.91,
		ClassificationReason:     "ai_reading",
	}

	err := pipeline.finalizeAnalysis(context.Background(), parseInputForTest(link), uuid.New(), 1, urlmeta.URLMetadata{},
		fetcher.Content{URL: link.URL}, result, nil, "http",
		requestedKindForLink(parseInputForTest(link)), nil,
		func(stage string, e error) { t.Fatalf("锁定链接的解析失败于 %s: %v", stage, e) })
	if err != nil {
		t.Fatalf("finalizeAnalysis 失败: %v——用户锁定不该让解析打死", err)
	}

	if completer.calls != 1 {
		t.Fatalf("CompleteSiteParse 调用 %d 次, want 1", completer.calls)
	}
	// 站点画像必须被兜底填上，否则 ValidateAggregateSiteParams 会拒绝。
	got := completer.params.Site
	if strings.TrimSpace(got.Name) == "" || strings.TrimSpace(got.EntryName) == "" {
		t.Fatalf("站点画像未兜底: name=%q entry=%q", got.Name, got.EntryName)
	}
	if got.Name != "深入理解某工具" {
		t.Fatalf("site name = %q, want 取自文章标题", got.Name)
	}
	// 归属仍是用户锁定的 site，分歧记在 predicted 上。
	if completer.params.Classification.Kind != model.LibraryKindSite {
		t.Fatalf("kind = %q, want site", completer.params.Classification.Kind)
	}
	if completer.params.Classification.PredictedKind == nil ||
		*completer.params.Classification.PredictedKind != model.LibraryKindReading {
		t.Fatal("predicted_library_kind 应记录模型的 reading 判断，供复核面看见分歧")
	}
}

// TestCollectionFinalizerInvalidatesAggregatesOnBothBranches 锁定解析完成后
// **两条分支**都失效聚合缓存。
//
// 站点分支此前在 CompleteSiteParse 之后直接 `return nil`，从未触碰失效器——
// 而它同样把链接写成 status='done' 并写入 site_tags，域名聚合
// （`WHERE status='done' GROUP BY domain`，无 library_kind 过滤）、全局标签
// 聚合、以及 scoped 标签聚合三份全部被改变。用户收藏一个新站点后，域名侧栏
// 要等整个 TTL 窗口（默认 5 分钟）才看得见它。
func TestCollectionFinalizerInvalidatesAggregatesOnBothBranches(t *testing.T) {
	t.Parallel()

	siteResult := analyzer.AnalysisResult{
		LibraryKind: model.LibraryKindSite, ClassificationConfidence: .91,
		ClassificationReason: "ai_site_resolution", ClassificationExplanation: "交互工具为主",
		SiteName: "Example Tool", SiteIntro: "用于演示的网站工具。",
		EntryName: "工具主页", EntryPurpose: "在线使用工具", Tags: []string{"工具"},
	}
	readingResult := analyzer.AnalysisResult{
		LibraryKind: model.LibraryKindReading, ClassificationConfidence: .73,
		ClassificationReason: "ai_reading", ClassificationExplanation: "article-like page",
		Title: "Reader Modules", Summary: "Explains reader module boundaries.", Tags: []string{"Go"},
	}

	cases := []struct {
		name   string
		result analyzer.AnalysisResult
		build  func(*pipelineCacheInvalidator) collectionFinalizer
	}{
		{
			name: "site 分支", result: siteResult,
			build: func(inv *pipelineCacheInvalidator) collectionFinalizer {
				return collectionFinalizer{siteCompleter: &recordingSiteCompleter{}, tagCacheInvalidator: inv}
			},
		},
		{
			name: "reading 分支", result: readingResult,
			build: func(inv *pipelineCacheInvalidator) collectionFinalizer {
				return collectionFinalizer{readingCompleter: &recordingReadingCompleter{}, tagCacheInvalidator: inv}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			invalidator := &pipelineCacheInvalidator{}
			finalizer := tc.build(invalidator)

			err := finalizer.Finalize(context.Background(), collectionFinalizationRequest{
				LinkID: uuid.New(), JobID: uuid.New(),
				URL:           "https://www.example.com/tool",
				URLMeta:       urlmeta.URLMetadata{Domain: "example.com", ContentType: model.ContentTypeHomepage},
				Content:       fetcher.Content{URL: "https://www.example.com/tool", FetcherType: "http"},
				Result:        tc.result,
				FetcherType:   "http",
				RequestedKind: model.RequestedLibraryKindAuto,
				RecordFail:    func(stage string, err error) { t.Fatalf("%s failed: %v", stage, err) },
			})
			if err != nil {
				t.Fatalf("Finalize() error = %v", err)
			}
			if invalidator.calls != 1 {
				t.Fatalf("聚合缓存失效次数 = %d, want 1（这条分支解析完成后没有失效任何聚合）", invalidator.calls)
			}
		})
	}
}
