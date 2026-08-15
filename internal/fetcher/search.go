package fetcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	nurl "net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"webtag/internal/errsafe"
	"webtag/internal/textutil"
)

var schemePrefixPattern = regexp.MustCompile(`^https?://`)

const (
	defaultSearchTimeout  = 10 * time.Second
	defaultSearchBaseURL  = "https://html.duckduckgo.com/html/"
	defaultSearchResults  = 5
	defaultSearchMaxBytes = 1 << 20
)

// DuckDuckGoSearcher 通过 html.duckduckgo.com 提交查询并解析前 MaxResults 条结果，作为抓取失败时的摘要降级来源。
type DuckDuckGoSearcher struct {
	client     *HTTPClient
	Timeout    time.Duration
	BaseURL    string
	MaxResults int
	MaxBytes   int64
}

// NewDuckDuckGoSearcher 构造默认 10s 超时 / 5 条结果上限的 DuckDuckGo 搜索器。
func NewDuckDuckGoSearcher(client *HTTPClient) *DuckDuckGoSearcher {
	return &DuckDuckGoSearcher{
		client:     ensureHTTPClient(client),
		Timeout:    defaultSearchTimeout,
		BaseURL:    defaultSearchBaseURL,
		MaxResults: defaultSearchResults,
		MaxBytes:   defaultSearchMaxBytes,
	}
}

// Search 用查询字符串发起一次 DDG 搜索，把前 MaxResults 条 (标题 — 摘要) 拼成多行字符串返回；空查询返回 ""。
//
//nolint:gocyclo // reason: HTTP 调用 + HTML 解析 + 多分支结果整形的线性流程，拆成 helper 反而要在多函数间共享 goquery selection 状态。
func (s *DuckDuckGoSearcher) Search(ctx context.Context, query string) (string, error) {
	query = normalizeSpace(query)
	if query == "" {
		return "", nil
	}

	values := nurl.Values{}
	values.Set("q", query)
	target := strings.TrimRight(s.BaseURL, "?") + "?" + values.Encode()

	req, cancel, err := s.client.NewRequest(ctx, s.Timeout, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	defer cancel()

	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "text/html")

	resp, err := s.client.DoWithRetry(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("search HTTP %d: %w", resp.StatusCode, errsafe.ErrUpstreamHTTP)
	}
	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if contentType != "" && !strings.Contains(contentType, "text/html") {
		return "", fmt.Errorf("search response content-type is not HTML: %w", errsafe.ErrContentType)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, s.MaxBytes+1))
	if err != nil {
		return "", err
	}
	if s.MaxBytes > 0 && int64(len(body)) > s.MaxBytes {
		return "", fmt.Errorf("search response too large: %w", errsafe.ErrResponseTooLarge)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}

	lines := make([]string, 0, s.MaxResults)
	doc.Find(".result").EachWithBreak(func(idx int, selection *goquery.Selection) bool {
		if idx >= s.MaxResults {
			return false
		}

		title := normalizeSpace(selection.Find(".result__a").First().Text())
		snippet := normalizeSpace(selection.Find(".result__snippet").First().Text())
		if title == "" && snippet == "" {
			return true
		}

		switch {
		case title != "" && snippet != "":
			lines = append(lines, fmt.Sprintf("%d. %s — %s", idx+1, title, snippet))
		case title != "":
			lines = append(lines, fmt.Sprintf("%d. %s", idx+1, title))
		default:
			lines = append(lines, fmt.Sprintf("%d. %s", idx+1, snippet))
		}
		return true
	})

	return strings.Join(lines, "\n"), nil
}

// BuildQuery 从原始 URL 和已知标题里拼出搜索关键词：优先用非通用标题，否则降级到 "域名 + 路径关键字"。
func BuildQuery(rawURL string, title string) string {
	title = normalizeSpace(title)
	if title != "" && !textutil.IsGenericTitle(title) {
		return title
	}

	trimmed := strings.TrimRight(schemePrefixPattern.ReplaceAllString(strings.TrimSpace(rawURL), ""), "/")
	if trimmed == "" {
		return ""
	}

	parts := strings.SplitN(trimmed, "/", 2)
	domain := strings.TrimPrefix(parts[0], "www.")
	if len(parts) == 1 {
		return domain
	}

	keywords := normalizeSpace(strings.NewReplacer("/", " ", "-", " ", "_", " ").Replace(parts[1]))
	if keywords == "" {
		return domain
	}
	return domain + " " + keywords
}

func ensureHTTPClient(client *HTTPClient) *HTTPClient {
	if client != nil {
		return client
	}
	return NewHTTPClient(nil)
}
