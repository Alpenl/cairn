package fetcher

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"webtag/internal/errsafe"
	"webtag/internal/observability"
	"webtag/internal/security"
	"webtag/internal/textutil"
)

// defaultMinBodyChars is the production threshold below which a
// successfully fetched body is still treated as too thin and triggers
// the configured fallback chain. Held as a package constant rather
// than a ManagerOptions field because no production wiring has ever
// needed to override it — exposing it as a public knob would invite
// confusion. Tests that need a lower threshold call (*Manager).setMinBodyChars.
const defaultMinBodyChars = 200
const minSearchSignalRunes = 12

// ManagerOptions 汇总 Manager 构造时可配的外部依赖（yt-dlp 路径、超时、GitHub Token）。
type ManagerOptions struct {
	YtdlpBinaryPath string
	YtdlpTimeout    time.Duration
	GitHubToken     string
	Metrics         *observability.Metrics
}

// Manager 是抓取层的对外入口，串起路由派发、jina 回退、可选 search 摘要降级和轻量抓取通道。
type Manager struct {
	router       *Router
	jina         Fetcher
	search       Searcher
	light        *LightFetcher
	options      ManagerOptions
	minBodyChars int
}

// NewManager 用给定的 router / jina / search 组合 Manager；minBodyChars 默认值见 defaultMinBodyChars。
func NewManager(router *Router, jina Fetcher, search Searcher, options ManagerOptions) *Manager {
	return &Manager{
		router:       router,
		jina:         jina,
		search:       search,
		options:      options,
		minBodyChars: defaultMinBodyChars,
	}
}

// SetLightFetcher installs the light path used by FetchLight. Separate
// setter (rather than a NewManager arg) so existing call sites that
// construct Manager directly stay unaffected — only the deep-fetch
// pipeline is mandatory.
func (m *Manager) SetLightFetcher(f *LightFetcher) {
	m.light = f
}

// setMinBodyChars overrides the body-size threshold used by Fetch's
// quality gate. Package-private on purpose: fixture-driven tests
// exercise the threshold logic with much smaller bodies (e.g. 40
// chars) than production would ever fetch, so they need a path to
// lower the cutoff without bloating the public ManagerOptions surface.
func (m *Manager) setMinBodyChars(n int) {
	if n <= 0 {
		return
	}
	m.minBodyChars = n
}

// NewDefaultManager 按生产默认顺序装配所有内置 Fetcher（arxiv → ytdlp → github → pdf → wechat → basic）并启用 light 通道。
// 默认链路不启用搜索摘要降级；搜索结果不是页面原文，需要该能力的调用方可通过 NewManager 显式注入 Searcher。
// 注意 wechat 等 host-bound 抓取器必须排在万能匹配的 basic 之前。
func NewDefaultManager(client *HTTPClient, options ManagerOptions) *Manager {
	shared := ensureHTTPClient(client)
	basic := NewBasicFetcher(shared)
	fetchers := []Fetcher{NewArxivFetcher(shared)}
	if strings.TrimSpace(options.YtdlpBinaryPath) != "" {
		fetchers = append(fetchers, NewYtdlpFetcher(options.YtdlpBinaryPath, options.YtdlpTimeout))
	}
	fetchers = append(fetchers,
		NewGitHubFetcher(shared, options.GitHubToken),
		newPDFFetcherWithMetrics(shared, options.Metrics),
		NewWeChatFetcher(shared), // host-bound, must come before basic which CanHandle: true
		basic,
	)
	mgr := NewManager(
		NewRouter(fetchers...),
		NewJinaFetcher(shared),
		nil,
		options,
	)
	mgr.SetLightFetcher(NewLightFetcher(shared))
	return mgr
}

// Fetch 是 deep-fetch 主入口：先走 Router 选中的专用 Fetcher，正文不足时回退到 jina；仅显式配置 Searcher 时才使用搜索摘要。
// 全部失败才返回错误；只是质量偏弱时仍会返回结果，但通过 FetcherType / Metadata["quality_signal"] 暴露信号。
func (m *Manager) Fetch(ctx context.Context, url string) (Content, error) {
	// blocked tracks whether any strategy came back with a verification
	// interstitial. It changes the *error* we report when everything
	// fails: "blocked" is actionable (re-fetch through a real browser),
	// while the generic "all strategies failed" is not.
	candidate, hasCandidate, blocked, originalErr := m.fetchRouterUnblocked(ctx, url)
	if hasCandidate && isSufficient(candidate, m.minBodyChars) && !shouldEscalateToJina(candidate) {
		return candidate, nil
	}

	jinaCandidate, hasJina, jinaBlocked := m.fetchJinaUnblocked(ctx, url, candidate, hasCandidate)
	blocked = blocked || jinaBlocked
	if hasJina {
		if isSufficient(jinaCandidate, m.minBodyChars) {
			return jinaCandidate, nil
		}
		candidate = jinaCandidate
		hasCandidate = true
	}

	if searchResult, ok := m.fetchSearch(ctx, url, candidate, hasCandidate); ok {
		return searchResult, nil
	}

	if hasCandidate && hasBody(candidate) {
		if shouldEscalateToJina(candidate) {
			candidate.Metadata = cloneMetadata(candidate.Metadata)
			if candidate.Metadata == nil {
				candidate.Metadata = map[string]any{}
			}
			candidate.Metadata["quality_signal"] = "weak"
			return candidate, nil
		}
		candidate.FetcherType += "+thin"
		return candidate, nil
	}

	if blocked {
		return Content{}, &FetchError{
			URL:    url,
			Reason: "origin served an anti-bot verification page instead of the document",
			Err:    errsafe.ErrBlockedByOrigin,
		}
	}

	if originalErr != nil {
		return Content{}, originalErr
	}

	return Content{}, &FetchError{URL: url, Reason: "all fetch strategies failed"}
}

// fetchRouterUnblocked runs the router pass and discards a result that
// turns out to be a verification interstitial, reporting separately that
// one was seen.
//
// Dropping it matters: an interstitial carries a title and a paragraph,
// so it would otherwise satisfy isSufficient and be handed to analysis
// as though it were the article.
func (m *Manager) fetchRouterUnblocked(ctx context.Context, url string) (Content, bool, bool, error) {
	candidate, hasCandidate, originalErr := m.fetchRouter(ctx, url)
	if hasCandidate && blockedInterstitial(candidate) {
		return Content{}, false, true, originalErr
	}
	return candidate, hasCandidate, false, originalErr
}

// fetchJinaUnblocked is the same filter over the jina escalation. This is
// the arm that actually fires in practice: jina renders server-side from
// its own datacenter IPs, which origins like mp.weixin.qq.com block
// wholesale, so a blocked target reliably comes back as an interstitial
// here even when the router pass produced nothing at all.
func (m *Manager) fetchJinaUnblocked(ctx context.Context, url string, candidate Content, hasCandidate bool) (Content, bool, bool) {
	jinaCandidate, ok := m.fetchJina(ctx, url, candidate, hasCandidate)
	if !ok {
		return Content{}, false, false
	}
	if blockedInterstitial(jinaCandidate) {
		return Content{}, false, true
	}
	return jinaCandidate, true, false
}

// minLightSignalChars is the floor below which a "light" fetch is
// considered too thin to skip the jina escalation. Lower than the
// full-mode minBodyChars threshold because the LightFetcher truncates
// body intentionally — title plus 100 chars of body is plenty of
// signal for the analyzer to produce decent tags.
const minLightSignalChars = 100

// FetchLight is the "I only need enough signal to tag this URL" path.
// It is intentionally separate from Fetch: the existing Fetch pipeline
// is contractually "get me everything, using configured fallbacks"
// and the rest of the parse pipeline depends on it.
//
// FetchLight uses ONLY the LightFetcher (Range-limited HTTP + truncated
// readability) and escalates to jina only when the light pass returns
// neither a title nor any body — jina is expensive (server-side
// headless render) so we avoid it when light has produced something
// the analyzer can work with.
//
// jina's body is truncated to LightFetcher.MaxChars before return so
// the analyzer prompt stays predictably small regardless of which leg
// produced the content.
//
// Returns an error only when both light and jina produced nothing
// usable. Callers (analyzer pipeline) can treat the error as "skip
// auto-tagging for this URL" rather than as a hard failure.
func (m *Manager) FetchLight(ctx context.Context, url string) (Content, error) {
	if m.light == nil {
		return Content{}, &FetchError{URL: url, Reason: "light fetcher not configured"}
	}

	candidate, lightErr := m.light.Fetch(ctx, url)
	if lightErr == nil && isSufficient(candidate, minLightSignalChars) {
		return candidate, nil
	}

	// Light produced too little. Try jina (server-side render). Truncate
	// jina's body to the same MaxChars so downstream sees a uniform
	// payload shape regardless of which leg succeeded.
	hasLight := lightErr == nil
	if jinaCandidate, ok := m.fetchJina(ctx, url, candidate, hasLight); ok {
		if hasBody(jinaCandidate) {
			jinaCandidate.Body = truncateRunes(jinaCandidate.Body, m.light.MaxChars)
			jinaCandidate.FetcherType += "+light"
			if isSufficient(jinaCandidate, minLightSignalChars) {
				return jinaCandidate, nil
			}
			candidate = jinaCandidate
			hasLight = true
		}
	}

	// If we have at least a title (from light's metadata-only path or
	// jina's truncated body), return it — title alone is enough signal
	// for the analyzer to produce coarse tags. Better than failing.
	if hasLight && (candidate.Title != "" || hasBody(candidate)) {
		candidate.FetcherType += "+thin"
		return candidate, nil
	}

	if lightErr != nil {
		return Content{}, lightErr
	}
	return Content{}, &FetchError{URL: url, Reason: "light fetch produced no usable signal"}
}

func shouldEscalateToJina(content Content) bool {
	if !hasBody(content) {
		return true
	}
	return textutil.IsGenericTitle(content.Title)
}

func (m *Manager) fetchRouter(ctx context.Context, url string) (Content, bool, error) {
	if m.router == nil {
		return Content{}, false, &FetchError{URL: url, Reason: "no fetcher router configured"}
	}

	selected := m.router.Select(url)
	if selected == nil {
		return Content{}, false, &FetchError{URL: url, Reason: "no fetcher matched URL"}
	}

	content, err := selected.Fetch(ctx, url)
	if err != nil {
		return Content{}, false, err
	}
	return content, hasBody(content), nil
}

func (m *Manager) fetchJina(ctx context.Context, url string, fallback Content, hasFallback bool) (Content, bool) {
	if m.jina == nil {
		return Content{}, false
	}
	if _, delegationSafe := security.ThirdPartyURLProjection(url); !delegationSafe {
		return Content{}, false
	}

	content, err := m.jina.Fetch(ctx, url)
	if err != nil || !hasBody(content) {
		return Content{}, false
	}

	if isSufficient(content, m.minBodyChars) {
		return content, true
	}

	if !hasFallback ||
		runeCount(normalizeSpace(content.Body)) > runeCount(normalizeSpace(fallback.Body)) ||
		(fallback.Title == "" && content.Title != "") {
		return content, true
	}

	return Content{}, false
}

func (m *Manager) fetchSearch(ctx context.Context, url string, candidate Content, hasCandidate bool) (Content, bool) {
	if m.search == nil {
		return Content{}, false
	}

	projected, _ := security.ThirdPartyURLProjection(url)
	query := BuildQuery(projected, candidate.Title)
	summary, err := m.search.Search(ctx, query)
	if err != nil || !isUsefulSearchSummary(summary) {
		return Content{}, false
	}

	fetcherType := "search"
	body := normalizeSpace(summary)
	title := ""
	metadata := map[string]any(nil)

	if hasCandidate {
		title = candidate.Title
		metadata = cloneMetadata(candidate.Metadata)
		body = candidate.Body
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["search_summary"] = strings.TrimSpace(summary)
		metadata["search_context"] = "duckduckgo"
		if title != "" {
			metadata["search_title"] = title
		}
		if candidate.FetcherType != "" {
			fetcherType = candidate.FetcherType + "+search"
		}
	}

	return Content{
		URL:         url,
		Title:       title,
		Body:        body,
		Metadata:    metadata,
		FetcherType: fetcherType,
	}, true
}

func isUsefulSearchSummary(summary string) bool {
	normalized := normalizeSpace(summary)
	if normalized == "" {
		return false
	}
	if utf8.RuneCountInString(normalized) >= minSearchSignalRunes {
		return true
	}

	meaningfulLines := 0
	for _, line := range strings.Split(summary, "\n") {
		if utf8.RuneCountInString(normalizeSpace(line)) >= minSearchSignalRunes {
			meaningfulLines++
			if meaningfulLines >= 2 {
				return true
			}
		}
	}

	return false
}
