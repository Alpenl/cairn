package fetcher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"webtag/internal/errsafe"

	"webtag/internal/observability"
)

func TestRouterSelectsFirstMatchingFetcherAndFallsBack(t *testing.T) {
	github := &stubFetcher{
		match: func(url string) bool { return strings.Contains(url, "github.com") },
	}
	basic := &stubFetcher{
		match: func(string) bool { return true },
	}

	router := NewRouter(github, basic)

	if got := router.Select("https://github.com/example/project"); got != github {
		t.Fatalf("Select(github URL) = %T, want github fetcher", got)
	}

	if got := router.Select("https://example.com/articles/123"); got != basic {
		t.Fatalf("Select(non-github URL) = %T, want basic fetcher", got)
	}
}

func TestManagerReturnsRouterContentWhenBodyIsSufficient(t *testing.T) {
	primary := &stubFetcher{
		match: func(string) bool { return true },
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://example.com/post",
				Title:       "Example Post",
				Body:        strings.Repeat("A", 24),
				FetcherType: "basic",
			}, nil
		},
	}
	jina := &stubFetcher{
		fetch: func(context.Context, string) (Content, error) {
			t.Fatal("jina should not run when primary content is sufficient")
			return Content{}, nil
		},
	}
	search := &stubSearcher{}

	manager := NewManager(NewRouter(primary), jina, search, ManagerOptions{})
	manager.setMinBodyChars(20)

	got, err := manager.Fetch(context.Background(), "https://example.com/post")
	if err != nil {
		t.Fatalf("Fetch() returned error: %v", err)
	}

	if got.FetcherType != "basic" {
		t.Fatalf("FetcherType = %q, want %q", got.FetcherType, "basic")
	}
	if primary.calls != 1 {
		t.Fatalf("primary calls = %d, want 1", primary.calls)
	}
	if search.calls != 0 {
		t.Fatalf("search calls = %d, want 0", search.calls)
	}
}

func TestManagerStillTriesJinaWhenPrimaryTitleIsGeneric(t *testing.T) {
	t.Parallel()

	primary := &stubFetcher{
		match: func(string) bool { return true },
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://example.com/post",
				Title:       "Home",
				Body:        strings.Repeat("A", 40),
				FetcherType: "basic",
			}, nil
		},
	}
	jina := &stubFetcher{
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://example.com/post",
				Title:       "Recovered Title",
				Body:        strings.Repeat("B", 40),
				FetcherType: "jina",
			}, nil
		},
	}
	search := &stubSearcher{}

	manager := NewManager(NewRouter(primary), jina, search, ManagerOptions{})
	manager.setMinBodyChars(20)

	got, err := manager.Fetch(context.Background(), "https://example.com/post")
	if err != nil {
		t.Fatalf("Fetch() returned error: %v", err)
	}

	if got.FetcherType != "jina" {
		t.Fatalf("FetcherType = %q, want %q", got.FetcherType, "jina")
	}
	if got.Title != "Recovered Title" {
		t.Fatalf("Title = %q, want Recovered Title", got.Title)
	}
	if primary.calls != 1 || jina.calls != 1 {
		t.Fatalf("primary/jina calls = %d/%d, want 1/1", primary.calls, jina.calls)
	}
	if search.calls != 0 {
		t.Fatalf("search calls = %d, want 0", search.calls)
	}
}

func TestManagerDoesNotMarkGenericTitleEscalationAsThinContentWhenFallbacksDoNotHelp(t *testing.T) {
	t.Parallel()

	primary := &stubFetcher{
		match: func(string) bool { return true },
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://example.com/post",
				Title:       "Home",
				Body:        strings.Repeat("A", 40),
				FetcherType: "basic",
			}, nil
		},
	}
	jina := &stubFetcher{
		fetch: func(context.Context, string) (Content, error) {
			return Content{}, &FetchError{URL: "https://example.com/post", Reason: "jina failed"}
		},
	}
	search := &stubSearcher{}

	manager := NewManager(NewRouter(primary), jina, search, ManagerOptions{})
	manager.setMinBodyChars(20)

	got, err := manager.Fetch(context.Background(), "https://example.com/post")
	if err != nil {
		t.Fatalf("Fetch() returned error: %v", err)
	}

	if got.FetcherType != "basic" {
		t.Fatalf("FetcherType = %q, want original fetcher type preserved", got.FetcherType)
	}
	if signal, ok := got.Metadata["quality_signal"].(string); !ok || signal != "weak" {
		t.Fatalf("quality_signal = %#v, want weak", got.Metadata["quality_signal"])
	}
}

func TestManagerFallsBackToJinaAfterPrimaryError(t *testing.T) {
	primaryErr := &FetchError{URL: "https://example.com/post", Reason: "primary failed"}
	primary := &stubFetcher{
		match: func(string) bool { return true },
		fetch: func(context.Context, string) (Content, error) {
			return Content{}, primaryErr
		},
	}
	jina := &stubFetcher{
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://example.com/post",
				Title:       "Recovered",
				Body:        strings.Repeat("J", 25),
				FetcherType: "jina",
			}, nil
		},
	}
	search := &stubSearcher{}

	manager := NewManager(NewRouter(primary), jina, search, ManagerOptions{})
	manager.setMinBodyChars(20)

	got, err := manager.Fetch(context.Background(), "https://example.com/post")
	if err != nil {
		t.Fatalf("Fetch() returned error: %v", err)
	}

	if got.FetcherType != "jina" {
		t.Fatalf("FetcherType = %q, want %q", got.FetcherType, "jina")
	}
	if primary.calls != 1 || jina.calls != 1 {
		t.Fatalf("primary/jina calls = %d/%d, want 1/1", primary.calls, jina.calls)
	}
	if search.calls != 0 {
		t.Fatalf("search calls = %d, want 0", search.calls)
	}
}

func TestManagerKeepsSensitiveURLLocalWhenFallbackWouldDelegate(t *testing.T) {
	t.Parallel()

	const sensitive = "https://alice:password@example.com/post?signature=query-secret#fragment-secret"
	primary := &stubFetcher{
		match: func(string) bool { return true },
		fetch: func(_ context.Context, got string) (Content, error) {
			if got != sensitive {
				t.Fatalf("local fetch URL=%q, want original URL", got)
			}
			return Content{URL: got, Title: "Local", Body: "thin", FetcherType: "basic"}, nil
		},
	}
	jina := &stubFetcher{fetch: func(context.Context, string) (Content, error) {
		t.Fatal("Jina must not receive a credentialed/signed URL")
		return Content{}, nil
	}}
	manager := NewManager(NewRouter(primary), jina, nil, ManagerOptions{})
	manager.setMinBodyChars(20)
	got, err := manager.Fetch(context.Background(), sensitive)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if got.URL != sensitive || primary.calls != 1 || jina.calls != 0 {
		t.Fatalf("result URL/calls = %q/%d/%d, want original/1/0", got.URL, primary.calls, jina.calls)
	}
}

func TestManagerFallsBackToSearchAfterThinContent(t *testing.T) {
	primary := &stubFetcher{
		match: func(string) bool { return true },
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://example.com/post",
				Title:       "Example Post",
				Body:        "too short",
				FetcherType: "basic",
			}, nil
		},
	}
	jina := &stubFetcher{
		fetch: func(context.Context, string) (Content, error) {
			return Content{}, &FetchError{URL: "https://example.com/post", Reason: "jina failed"}
		},
	}
	search := &stubSearcher{
		result: "1. Example Post — recovered from search",
	}

	manager := NewManager(NewRouter(primary), jina, search, ManagerOptions{})
	manager.setMinBodyChars(20)

	got, err := manager.Fetch(context.Background(), "https://example.com/post")
	if err != nil {
		t.Fatalf("Fetch() returned error: %v", err)
	}

	if got.FetcherType != "basic+search" {
		t.Fatalf("FetcherType = %q, want %q", got.FetcherType, "basic+search")
	}
	if got.Body != "too short" {
		t.Fatalf("Body = %q, want original body preserved", got.Body)
	}
	if searchSummary, ok := got.Metadata["search_summary"].(string); !ok || !strings.Contains(searchSummary, "recovered from search") {
		t.Fatalf("search_summary metadata = %#v, want preserved recovered search result", got.Metadata["search_summary"])
	}
	if search.query != "Example Post" {
		t.Fatalf("search query = %q, want title fallback", search.query)
	}
}

func TestManagerReturnsThinFallbackWhenSearchAddsNothing(t *testing.T) {
	primary := &stubFetcher{
		match: func(string) bool { return true },
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://example.com/post",
				Title:       "Example Post",
				Body:        "too short",
				FetcherType: "basic",
			}, nil
		},
	}
	jina := &stubFetcher{
		fetch: func(context.Context, string) (Content, error) {
			return Content{}, &FetchError{URL: "https://example.com/post", Reason: "jina failed"}
		},
	}
	search := &stubSearcher{}

	manager := NewManager(NewRouter(primary), jina, search, ManagerOptions{})
	manager.setMinBodyChars(20)

	got, err := manager.Fetch(context.Background(), "https://example.com/post")
	if err != nil {
		t.Fatalf("Fetch() returned error: %v", err)
	}

	if got.FetcherType != "basic+thin" {
		t.Fatalf("FetcherType = %q, want %q", got.FetcherType, "basic+thin")
	}
	if got.Body != "too short" {
		t.Fatalf("Body = %q, want thin original content", got.Body)
	}
}

func TestManagerIgnoresLowSignalSearchFallback(t *testing.T) {
	t.Parallel()

	primary := &stubFetcher{
		match: func(string) bool { return true },
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://example.com/post",
				Title:       "Example Post",
				Body:        "thin body",
				FetcherType: "basic",
			}, nil
		},
	}
	jina := &stubFetcher{
		fetch: func(context.Context, string) (Content, error) {
			return Content{}, &FetchError{URL: "https://example.com/post", Reason: "jina failed"}
		},
	}
	search := &stubSearcher{
		result: "Go",
	}

	manager := NewManager(NewRouter(primary), jina, search, ManagerOptions{})
	manager.setMinBodyChars(20)

	got, err := manager.Fetch(context.Background(), "https://example.com/post")
	if err != nil {
		t.Fatalf("Fetch() returned error: %v", err)
	}

	if got.FetcherType != "basic+thin" {
		t.Fatalf("FetcherType = %q, want %q", got.FetcherType, "basic+thin")
	}
	if got.Body != "thin body" {
		t.Fatalf("Body = %q, want original thin content", got.Body)
	}
}

func TestManagerUsesSearchFallbackWhenItHasMultipleMeaningfulLines(t *testing.T) {
	t.Parallel()

	primary := &stubFetcher{
		match: func(string) bool { return true },
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://example.com/post",
				Title:       "Example Post",
				Body:        "thin body",
				FetcherType: "basic",
			}, nil
		},
	}
	jina := &stubFetcher{
		fetch: func(context.Context, string) (Content, error) {
			return Content{}, &FetchError{URL: "https://example.com/post", Reason: "jina failed"}
		},
	}
	search := &stubSearcher{
		result: "1. Example Post - detailed summary\n2. Related result - extra context",
	}

	manager := NewManager(NewRouter(primary), jina, search, ManagerOptions{})
	manager.setMinBodyChars(20)

	got, err := manager.Fetch(context.Background(), "https://example.com/post")
	if err != nil {
		t.Fatalf("Fetch() returned error: %v", err)
	}

	if got.FetcherType != "basic+search" {
		t.Fatalf("FetcherType = %q, want %q", got.FetcherType, "basic+search")
	}
	if got.Body != "thin body" {
		t.Fatalf("Body = %q, want original body preserved", got.Body)
	}
	if searchSummary, ok := got.Metadata["search_summary"].(string); !ok || !strings.Contains(searchSummary, "Related result") {
		t.Fatalf("search_summary metadata = %#v, want meaningful search fallback content", got.Metadata["search_summary"])
	}
}

func TestManagerStoresSearchFallbackAsSeparateMetadataSignal(t *testing.T) {
	t.Parallel()

	primary := &stubFetcher{
		match: func(string) bool { return true },
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://example.com/post",
				Title:       "Example Post",
				Body:        "thin body",
				FetcherType: "basic",
			}, nil
		},
	}
	jina := &stubFetcher{
		fetch: func(context.Context, string) (Content, error) {
			return Content{}, &FetchError{URL: "https://example.com/post", Reason: "jina failed"}
		},
	}
	search := &stubSearcher{
		result: "1. Example Post - detailed summary\n2. Related result - extra context",
	}

	manager := NewManager(NewRouter(primary), jina, search, ManagerOptions{})
	manager.setMinBodyChars(20)

	got, err := manager.Fetch(context.Background(), "https://example.com/post")
	if err != nil {
		t.Fatalf("Fetch() returned error: %v", err)
	}

	if got.Body != "thin body" {
		t.Fatalf("Body = %q, want original body preserved for analyzer body section", got.Body)
	}

	rawSearch, ok := got.Metadata["search_summary"].(string)
	if !ok || !strings.Contains(rawSearch, "Related result") {
		t.Fatalf("search_summary metadata = %#v, want preserved search summary", got.Metadata["search_summary"])
	}

	if source, ok := got.Metadata["search_context"].(string); !ok || source != "duckduckgo" {
		t.Fatalf("search_context metadata = %#v, want duckduckgo", got.Metadata["search_context"])
	}
}

func TestManagerReturnsOriginalFetchErrorWhenNoFallbackProducesContent(t *testing.T) {
	primaryErr := &FetchError{URL: "https://example.com/post", Reason: "primary failed"}
	primary := &stubFetcher{
		match: func(string) bool { return true },
		fetch: func(context.Context, string) (Content, error) {
			return Content{}, primaryErr
		},
	}
	jina := &stubFetcher{
		fetch: func(context.Context, string) (Content, error) {
			return Content{}, &FetchError{URL: "https://example.com/post", Reason: "jina failed"}
		},
	}
	search := &stubSearcher{}

	manager := NewManager(NewRouter(primary), jina, search, ManagerOptions{})
	manager.setMinBodyChars(20)

	_, err := manager.Fetch(context.Background(), "https://example.com/post")
	if err == nil {
		t.Fatal("Fetch() returned nil error, want original fetch error")
	}
	if !errors.Is(err, primaryErr) {
		t.Fatalf("Fetch() error = %v, want original fetch error %v", err, primaryErr)
	}
}

func TestNewDefaultManagerSharesSingleHTTPClientWithoutSearchFallback(t *testing.T) {
	manager := NewDefaultManager(nil, ManagerOptions{})

	// Default options omit YtdlpBinaryPath, so YtdlpFetcher is not registered.
	arxivFetcher, ok := manager.router.fetchers[0].(*ArxivFetcher)
	if !ok {
		t.Fatalf("fetcher[0] = %T, want *ArxivFetcher", manager.router.fetchers[0])
	}
	githubFetcher, ok := manager.router.fetchers[1].(*GitHubFetcher)
	if !ok {
		t.Fatalf("fetcher[1] = %T, want *GitHubFetcher", manager.router.fetchers[1])
	}
	pdfFetcher, ok := manager.router.fetchers[2].(*PDFFetcher)
	if !ok {
		t.Fatalf("fetcher[2] = %T, want *PDFFetcher", manager.router.fetchers[2])
	}
	wechatFetcher, ok := manager.router.fetchers[3].(*HostBoundHTMLFetcher)
	if !ok {
		t.Fatalf("fetcher[3] = %T, want *HostBoundHTMLFetcher (wechat)", manager.router.fetchers[3])
	}
	basicFetcher, ok := manager.router.fetchers[4].(*BasicFetcher)
	if !ok {
		t.Fatalf("fetcher[4] = %T, want *BasicFetcher", manager.router.fetchers[4])
	}
	jinaFetcher, ok := manager.jina.(*JinaFetcher)
	if !ok {
		t.Fatalf("jina = %T, want *JinaFetcher", manager.jina)
	}
	if manager.search != nil {
		t.Fatalf("search = %T, want nil for the production default", manager.search)
	}

	if arxivFetcher.client != githubFetcher.client ||
		arxivFetcher.client != pdfFetcher.client ||
		arxivFetcher.client != wechatFetcher.client ||
		arxivFetcher.client != basicFetcher.client ||
		arxivFetcher.client != jinaFetcher.client {
		t.Fatal("expected all default fetchers to share one HTTP client")
	}
}

func TestNewDefaultManagerWiresMetricsIntoPDFFetcher(t *testing.T) {
	t.Parallel()

	metrics := observability.NewMetrics()
	manager := NewDefaultManager(nil, ManagerOptions{Metrics: metrics})
	pdfFetcher, ok := manager.router.fetchers[2].(*PDFFetcher)
	if !ok {
		t.Fatalf("fetcher[2] = %T, want *PDFFetcher", manager.router.fetchers[2])
	}
	if pdfFetcher.metrics != metrics {
		t.Fatal("default PDF fetcher did not receive ManagerOptions.Metrics")
	}
}

func TestNewDefaultManagerRegistersYtdlpWhenConfigured(t *testing.T) {
	manager := NewDefaultManager(nil, ManagerOptions{
		YtdlpBinaryPath: "yt-dlp",
		YtdlpTimeout:    5 * time.Second,
	})

	if _, ok := manager.router.fetchers[0].(*ArxivFetcher); !ok {
		t.Fatalf("fetcher[0] = %T, want *ArxivFetcher", manager.router.fetchers[0])
	}
	ytdlp, ok := manager.router.fetchers[1].(*YtdlpFetcher)
	if !ok {
		t.Fatalf("fetcher[1] = %T, want *YtdlpFetcher", manager.router.fetchers[1])
	}
	if ytdlp.BinaryPath != "yt-dlp" {
		t.Fatalf("YtdlpFetcher.BinaryPath = %q, want yt-dlp", ytdlp.BinaryPath)
	}
	if ytdlp.Timeout != 5*time.Second {
		t.Fatalf("YtdlpFetcher.Timeout = %v, want 5s", ytdlp.Timeout)
	}
	if _, ok := manager.router.fetchers[2].(*GitHubFetcher); !ok {
		t.Fatalf("fetcher[2] = %T, want *GitHubFetcher", manager.router.fetchers[2])
	}
}

func TestGitHubFetcherOnlyHandlesRepositoryRootURLs(t *testing.T) {
	fetcher := NewGitHubFetcher(NewHTTPClient(nil), "")

	if !fetcher.CanHandle("https://github.com/openai/openai-go") {
		t.Fatal("expected repository root URL to match GitHub fetcher")
	}

	for _, rawURL := range []string{
		"https://github.com/openai/openai-go/issues/1",
		"https://github.com/openai/openai-go/pull/7",
		"https://github.com/openai/openai-go/blob/main/README.md",
		"https://github.com/orgs/openai",
	} {
		if fetcher.CanHandle(rawURL) {
			t.Fatalf("expected non-repository GitHub URL to fall through: %s", rawURL)
		}
	}
}

func TestPDFFetcherOnlyHandlesPDFPathSuffix(t *testing.T) {
	fetcher := NewPDFFetcher(NewHTTPClient(nil))

	if !fetcher.CanHandle("https://example.com/files/report.pdf") {
		t.Fatal("expected .pdf path suffix to match PDF fetcher")
	}

	for _, rawURL := range []string{
		"https://example.com/viewer?file=report.pdf",
		"https://example.com/docs/about-pdf-format.html",
	} {
		if fetcher.CanHandle(rawURL) {
			t.Fatalf("expected non-PDF path to fall through: %s", rawURL)
		}
	}
}

func TestManagerPrefersBetterThinJinaContentBeforeThinFallback(t *testing.T) {
	primary := &stubFetcher{
		match: func(string) bool { return true },
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://example.com/post",
				Title:       "Example",
				Body:        "short body",
				FetcherType: "basic",
			}, nil
		},
	}
	jina := &stubFetcher{
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://example.com/post",
				Title:       "Example",
				Body:        "this body is still thin but clearly much more useful than the router fallback",
				FetcherType: "jina",
			}, nil
		},
	}
	search := &stubSearcher{}

	manager := NewManager(NewRouter(primary), jina, search, ManagerOptions{})

	got, err := manager.Fetch(context.Background(), "https://example.com/post")
	if err != nil {
		t.Fatalf("Fetch() returned error: %v", err)
	}

	if got.FetcherType != "jina+thin" {
		t.Fatalf("FetcherType = %q, want %q", got.FetcherType, "jina+thin")
	}
	if !strings.Contains(got.Body, "much more useful") {
		t.Fatalf("Body = %q, want the better thin Jina candidate", got.Body)
	}
}

func TestManagerDoesNotPreferJinaWhenItIsOnlyLongerByBytes(t *testing.T) {
	t.Parallel()

	primary := &stubFetcher{
		match: func(string) bool { return true },
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://example.com/post",
				Title:       "Example",
				Body:        "abcde",
				FetcherType: "basic",
			}, nil
		},
	}
	jina := &stubFetcher{
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://example.com/post",
				Title:       "Example",
				Body:        "你好",
				FetcherType: "jina",
			}, nil
		},
	}
	search := &stubSearcher{}

	manager := NewManager(NewRouter(primary), jina, search, ManagerOptions{})

	got, err := manager.Fetch(context.Background(), "https://example.com/post")
	if err != nil {
		t.Fatalf("Fetch() returned error: %v", err)
	}

	if got.FetcherType != "basic+thin" {
		t.Fatalf("FetcherType = %q, want %q", got.FetcherType, "basic+thin")
	}
	if got.Body != "abcde" {
		t.Fatalf("Body = %q, want router fallback body", got.Body)
	}
}

type stubFetcher struct {
	match func(string) bool
	fetch func(context.Context, string) (Content, error)
	calls int
}

func (s *stubFetcher) CanHandle(url string) bool {
	if s.match == nil {
		return false
	}
	return s.match(url)
}

func (s *stubFetcher) Fetch(ctx context.Context, url string) (Content, error) {
	s.calls++
	if s.fetch == nil {
		return Content{}, nil
	}
	return s.fetch(ctx, url)
}

type stubSearcher struct {
	result string
	err    error
	query  string
	calls  int
}

func (s *stubSearcher) Search(_ context.Context, query string) (string, error) {
	s.calls++
	s.query = query
	return s.result, s.err
}

// 这是本次修复的核心回归：mp.weixin.qq.com 的风控页是 HTTP 200 + 有标题有正文，
// 修复前它能满足 isSufficient 而被当成文章交给模型总结，链接以 status=done 入库、
// 标题「微信公众号环境异常验证页」、摘要一本正经地解释这里没有文章。抓取失败是可见
// 可重试的，被自信地总结掉的风控页则是静默的数据损失，所以它必须报错而不是降级。
func TestManagerRejectsInterstitialInsteadOfSummarisingIt(t *testing.T) {
	primary := &stubFetcher{
		match: func(string) bool { return true },
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://mp.weixin.qq.com/s/example",
				Title:       "环境异常",
				Body:        "环境异常 当前环境异常，完成验证后即可继续访问。 去验证 确定",
				FetcherType: "wechat",
			}, nil
		},
	}
	// jina 从自己的机房 IP 服务端渲染，正是被 mp.weixin.qq.com 整段封锁的那类出口，
	// 所以现实中真正命中的是这一支：它也只能拿回同一张风控页。
	jina := &stubFetcher{
		fetch: func(context.Context, string) (Content, error) {
			return Content{
				URL:         "https://mp.weixin.qq.com/s/example",
				Title:       "环境异常",
				Body:        "环境异常 完成验证后即可继续访问 去验证",
				FetcherType: "jina",
			}, nil
		},
	}

	manager := NewManager(NewRouter(primary), jina, nil, ManagerOptions{})
	manager.setMinBodyChars(20)

	got, err := manager.Fetch(context.Background(), "https://mp.weixin.qq.com/s/example")
	if err == nil {
		t.Fatalf("Fetch() should fail on an interstitial, got content %+v", got)
	}
	if !errors.Is(err, errsafe.ErrBlockedByOrigin) {
		t.Fatalf("Fetch() error = %v, want ErrBlockedByOrigin so callers can re-fetch via a browser", err)
	}
	if errsafe.ClassifyError(err) != "blocked_by_origin" {
		t.Fatalf("ClassifyError = %q, want blocked_by_origin", errsafe.ClassifyError(err))
	}
}
