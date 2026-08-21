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
	calls  int
}

// CompleteSiteParse shares production validation so the fake cannot accept an
// invalid aggregate that PostgreSQL would reject.
func (r *recordingSiteCompleter) CompleteSiteParse(_ context.Context, params repository.CompleteSiteParseParams) (repository.SiteAggregateResult, error) {
	if err := repotest.ValidateSiteParse(params); err != nil {
		return repository.SiteAggregateResult{}, err
	}
	r.params, r.calls = params, r.calls+1
	return repository.SiteAggregateResult{SiteID: uuid.New(), EntryID: uuid.New()}, nil
}

func TestCollectionFinalizerCompletesSiteThroughAtomicCompleter(t *testing.T) {
	completer := &recordingSiteCompleter{}
	finalizer := collectionFinalizer{siteCompleter: completer}
	linkID := uuid.New()
	attempt := model.ParseAttempt{LinkID: linkID, Generation: 1, ExpectedMetadataRevision: 1}
	result := analyzer.AnalysisResult{LibraryKind: model.LibraryKindSite, SiteName: "Example Tool", SiteIntro: "用于演示的网站工具。", EntryName: "工具主页", EntryPurpose: "在线使用工具", Tags: []string{"工具"}}
	err := finalizer.Finalize(context.Background(), collectionFinalizationRequest{LinkID: linkID, Attempt: attempt, URL: "https://www.example.com/tool?utm_source=x", URLMeta: urlmeta.URLMetadata{Domain: "example.com", ContentType: model.ContentTypeHomepage}, Content: fetcher.Content{URL: "https://www.example.com/tool?utm_source=x", FetcherType: "http"}, Result: result, FetcherType: "http"})
	if err != nil {
		t.Fatal(err)
	}
	if completer.calls != 1 || completer.params.Analysis.ExpectedParseGeneration != 1 {
		t.Fatalf("site completer calls=%d generation=%d", completer.calls, completer.params.Analysis.ExpectedParseGeneration)
	}
	if got := completer.params.Site; got.IdentityKey != "v1:host:example.com" || got.NormalizedURL != "https://example.com/tool" {
		t.Fatalf("site params = %#v", got)
	}
	if got := completer.params.Classification; got.Kind != model.LibraryKindSite || got.Locked {
		t.Fatalf("classification = %#v", got)
	}
}

func TestCollectionFinalizerKeepsExplicitSiteLocked(t *testing.T) {
	completer := &recordingSiteCompleter{}
	finalizer := collectionFinalizer{siteCompleter: completer}
	result := analyzer.AnalysisResult{LibraryKind: model.LibraryKindSite, SiteName: "Example", SiteIntro: "简介", EntryName: "首页", EntryPurpose: "用途"}
	linkID := uuid.New()
	site := model.LibraryKindSite
	err := finalizer.Finalize(context.Background(), collectionFinalizationRequest{LinkID: linkID, Attempt: model.ParseAttempt{LinkID: linkID, Generation: 1, ExpectedMetadataRevision: 1}, URL: "https://example.com", Content: fetcher.Content{URL: "https://example.com"}, Result: result, FetcherType: "http", CurrentKind: &site, CurrentLocked: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := completer.params.Classification; got.Kind != model.LibraryKindSite || !got.Locked {
		t.Fatalf("classification = %#v", got)
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
		LibraryKind: model.LibraryKindReading,
		Title:       "深入理解某工具",
		Summary:     "一篇讲该工具的长文",
	}

	err := pipeline.finalizeAnalysis(context.Background(), parseInputForTest(link), model.ParseAttempt{LinkID: link.ID, Generation: 1, ExpectedMetadataRevision: 1}, urlmeta.URLMetadata{},
		fetcher.Content{URL: link.URL}, result, "http")
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
	if completer.params.Classification.Kind != model.LibraryKindSite || !completer.params.Classification.Locked {
		t.Fatalf("classification = %#v, want locked site", completer.params.Classification)
	}
}
