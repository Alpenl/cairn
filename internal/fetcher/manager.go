package fetcher

import (
	"context"

	"webtag/internal/errsafe"
	"webtag/internal/security"
	"webtag/internal/textutil"
)

// defaultMinBodyChars is the production threshold below which a
// successfully fetched body is still treated as too thin and triggers
// the configured fallback chain. Held as a package constant rather
// than a constructor option because no production wiring has ever
// needed to override it — exposing it as a public knob would invite
// confusion. Tests that need a lower threshold call (*Manager).setMinBodyChars.
const defaultMinBodyChars = 200

// Manager 是抓取层的对外入口，串起路由派发和 jina 回退。
type Manager struct {
	router       *Router
	jina         Fetcher
	minBodyChars int
}

// NewManager 用给定的 router 和 jina 组合 Manager。
func NewManager(router *Router, jina Fetcher) *Manager {
	return &Manager{
		router:       router,
		jina:         jina,
		minBodyChars: defaultMinBodyChars,
	}
}

// setMinBodyChars overrides the body-size threshold used by Fetch's
// quality gate. Package-private on purpose: fixture-driven tests
// exercise the threshold logic with much smaller bodies (e.g. 40
// chars) than production would ever fetch, so they need a path to
// lower the cutoff without bloating the public constructor surface.
func (m *Manager) setMinBodyChars(n int) {
	if n <= 0 {
		return
	}
	m.minBodyChars = n
}

// NewDefaultManager 按生产默认顺序装配所有内置 Fetcher。
// 注意 wechat 等 host-bound 抓取器必须排在万能匹配的 basic 之前。
func NewDefaultManager(client *HTTPClient, githubToken string) *Manager {
	shared := ensureHTTPClient(client)
	basic := NewBasicFetcher(shared)
	fetchers := []Fetcher{NewArxivFetcher(shared)}
	fetchers = append(fetchers,
		NewGitHubFetcher(shared, githubToken),
		NewPDFFetcher(shared),
		NewWeChatFetcher(shared), // host-bound, must come before basic which CanHandle: true
		basic,
	)
	return NewManager(
		NewRouter(fetchers...),
		NewJinaFetcher(shared),
	)
}

// Fetch 是抓取主入口：先走 Router 选中的专用 Fetcher，正文不足时回退到 jina。
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
