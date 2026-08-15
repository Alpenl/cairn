package fetcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	nurl "net/url"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/PuerkitoBio/goquery"
	"github.com/go-shiori/dom"
	"golang.org/x/net/html/charset"

	"webtag/internal/contentdoc"
	"webtag/internal/errsafe"
	"webtag/internal/textutil"
)

const (
	defaultBasicTimeout  = 12 * time.Second
	defaultBasicMaxBytes = 2 << 20
	defaultUserAgent     = "WebTag-Go/0.1"
	// discussionRemovalSelector is intentionally limited to explicit comment
	// and reply semantics. Substring selectors such as [class*="comment"]
	// would also delete legitimate article sections named "commentary".
	discussionRemovalSelector = `#comments,#comment-list,#commentlist,#comment-thread,#comment-section,#comments-area,#disqus_thread,#giscus,#utterances,.comment,.comments,.comment-list,.commentlist,.comment-thread,.comment-section,.comments-area,.reply,.reply-list,.replies,.replies-list,[itemprop="comment"],[itemprop="comments"],[itemtype$="/Comment"]`
)

// BasicFetcher 是通用 HTML 抓取器，对任意 URL 都 CanHandle，作为 Router 链路的兜底实现。
type BasicFetcher struct {
	client    *HTTPClient
	Timeout   time.Duration
	MaxBytes  int64
	UserAgent string
}

// NewBasicFetcher 用默认超时 / 体积上限 / UA 构造 BasicFetcher；传 nil client 时自动落到内置 SSRF-safe 客户端。
func NewBasicFetcher(client *HTTPClient) *BasicFetcher {
	return &BasicFetcher{
		client:    ensureHTTPClient(client),
		Timeout:   defaultBasicTimeout,
		MaxBytes:  defaultBasicMaxBytes,
		UserAgent: defaultUserAgent,
	}
}

// CanHandle 恒为 true：BasicFetcher 是兜底，必须保证 Router 排在它前面的专用抓取器先尝试。
func (f *BasicFetcher) CanHandle(string) bool {
	return true
}

// Fetch 拉取 URL 对应的 HTML，跑 readability 抽出正文 / 标题 / 元信息，未取到正文时返回 FetchError。
func (f *BasicFetcher) Fetch(ctx context.Context, url string) (Content, error) {
	req, cancel, err := f.client.NewRequest(ctx, f.Timeout, http.MethodGet, url, nil)
	if err != nil {
		return Content{}, &FetchError{URL: url, Reason: "build request failed", Err: err}
	}
	defer cancel()

	req.Header.Set("User-Agent", f.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := f.client.DoWithRetry(req)
	if err != nil {
		return Content{}, &FetchError{URL: url, Reason: "request failed", Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Content{}, &FetchError{URL: url, Reason: fmt.Sprintf("HTTP %d", resp.StatusCode), Err: errsafe.ErrUpstreamHTTP}
	}
	contentType := resp.Header.Get("Content-Type")
	if !isAllowedBasicContentType(contentType) {
		return Content{}, &FetchError{URL: url, Reason: "response content-type is not supported", Err: errsafe.ErrContentType}
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, f.MaxBytes+1))
	if err != nil {
		return Content{}, &FetchError{URL: url, Reason: "read body failed", Err: err}
	}
	if int64(len(data)) > f.MaxBytes {
		return Content{}, &FetchError{URL: url, Reason: "response too large", Err: errsafe.ErrResponseTooLarge}
	}

	body, title, documentHTML, metadata := extractReadableContent(url, data, contentType)
	if body == "" {
		return Content{}, &FetchError{URL: url, Reason: "readability extraction returned empty body"}
	}

	return Content{
		URL:         url,
		Title:       title,
		Body:        body,
		HTML:        documentHTML,
		Metadata:    metadata,
		FetcherType: "basic",
	}, nil
}

// decodeToUTF8 把抓回的 HTML 字节归一化成 UTF-8。HTTP Content-Type 必须参与
// 探测：HTML charset prescan 只看前 1024 字节，且不少站点（包括 V2EX）使用不被
// WHATWG prescan 识别的 meta name="Content-Type"。忽略响应头时，这类 UTF-8 页面
// 会回退到 Windows-1252 并产生 ä»Šå¹´ 形式的 mojibake。
func decodeToUTF8(html []byte, contentType string) []byte {
	r, err := charset.NewReader(bytes.NewReader(html), contentType)
	if err != nil {
		return html
	}
	decoded, err := io.ReadAll(r)
	if err != nil || len(decoded) == 0 {
		return html
	}
	return decoded
}

func extractReadableContent(rawURL string, html []byte, contentType string) (string, string, string, map[string]any) { //nolint:gocyclo // 正文抽取的多级回退：readability → 结构化 → 纯文本
	parsedURL, err := nurl.Parse(rawURL)
	if err != nil {
		parsedURL = &nurl.URL{}
	}

	// 先按真实编码转成 UTF-8，再交给 dom.Parse / readability（两者都假定 UTF-8）。
	html = decodeToUTF8(html, contentType)

	// Parse once and share the decoded tree with the site-specific cleanup,
	// readability, and the structural fallback. readability.FromDocument
	// clones the cleaned tree before running its own destructive transforms.
	parsedDoc, parseErr := dom.Parse(bytes.NewReader(html))
	if parseErr != nil {
		// dom.Parse is virtually infallible for any byte slice (it tolerates
		// malformed HTML), but if it ever does fail we surface an empty body
		// so the pipeline records a fetch_quality:weak run rather than a
		// crash. Returning empty here mirrors the previous behaviour when
		// readability.FromReader failed and goquery.NewDocumentFromReader
		// failed in tandem.
		return "", "", "", nil
	}

	doc := goquery.NewDocumentFromNode(parsedDoc)
	pruneDiscussionContent(doc, parsedURL)
	preferredHTML := siteSpecificReadableHTML(doc, parsedURL)

	article, articleErr := readability.FromDocument(parsedDoc, parsedURL)
	var readableHTML, readableText, byline, excerpt, siteName, language, readableTitle string
	if articleErr == nil {
		var htmlBuffer bytes.Buffer
		if renderErr := article.RenderHTML(&htmlBuffer); renderErr == nil {
			readableHTML = strings.TrimSpace(htmlBuffer.String())
		}
		var textBuffer bytes.Buffer
		if renderErr := article.RenderText(&textBuffer); renderErr == nil {
			readableText = textBuffer.String()
		}
		byline = article.Byline()
		excerpt = article.Excerpt()
		siteName = article.SiteName()
		language = article.Language()
		readableTitle = article.Title()
	}

	documentHTML := firstNonEmpty(preferredHTML, readableHTML)
	body := ""
	if documentHTML != "" {
		if document, convertErr := contentdoc.FromHTML(documentHTML, rawURL); convertErr == nil {
			body = document.Text
		}
	}
	if body == "" {
		body = contentdoc.Plain(readableText).Text
	}
	title := normalizeSpace(readableTitle)

	if doc != nil {
		title = chooseBestDocumentTitle(title, doc)
		if body == "" {
			body = contentdoc.Plain(doc.Find("body").Text()).Text
		}
		if documentHTML == "" {
			if fallbackHTML, htmlErr := doc.Find("body").Html(); htmlErr == nil {
				documentHTML = strings.TrimSpace(fallbackHTML)
			}
		}
	}

	var metadata map[string]any
	if byline != "" || excerpt != "" || siteName != "" || language != "" {
		metadata = map[string]any{}
		if byline != "" {
			metadata["author"] = byline
		}
		if excerpt != "" {
			metadata["excerpt"] = excerpt
		}
		if siteName != "" {
			metadata["site_name"] = siteName
		}
		if language != "" {
			metadata["language"] = language
		}
	}
	if title != "" {
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["best_title"] = title
	}

	return body, title, documentHTML, metadata
}

func pruneDiscussionContent(doc *goquery.Document, parsedURL *nurl.URL) {
	if doc == nil {
		return
	}
	doc.Find(discussionRemovalSelector).Remove()

	if !isV2EXTopicURL(parsedURL) {
		return
	}
	// V2EX reply cells use r_<id>. Removing their enclosing box also drops
	// reply counts and pagination; the direct .topic_content extraction below
	// remains the authoritative body when the site's layout is recognized.
	replies := doc.Find(`[id^="r_"]`)
	replies.Closest(".box").Remove()
	replies.Remove()
}

func siteSpecificReadableHTML(doc *goquery.Document, parsedURL *nurl.URL) string {
	if doc == nil || !isV2EXTopicURL(parsedURL) {
		return ""
	}
	topic := doc.Find(".topic_content").First()
	if len(topic.Nodes) == 0 {
		return ""
	}
	html, err := goquery.OuterHtml(topic)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(html)
}

func isV2EXTopicURL(parsedURL *nurl.URL) bool {
	if parsedURL == nil || !strings.HasPrefix(parsedURL.EscapedPath(), "/t/") {
		return false
	}
	host := strings.ToLower(parsedURL.Hostname())
	return host == "v2ex.com" || strings.HasSuffix(host, ".v2ex.com")
}

func chooseBestDocumentTitle(current string, doc *goquery.Document) string {
	candidates := []string{current}
	if doc != nil {
		candidates = append(candidates,
			normalizeSpace(doc.Find(`meta[property="og:title"]`).First().AttrOr("content", "")),
			normalizeSpace(doc.Find(`meta[name="twitter:title"]`).First().AttrOr("content", "")),
			normalizeSpace(doc.Find("h1").First().Text()),
			normalizeSpace(doc.Find("title").First().Text()),
		)
	}

	fallback := ""
	for _, candidate := range candidates {
		candidate = normalizeSpace(candidate)
		if candidate == "" {
			continue
		}
		if fallback == "" {
			fallback = candidate
		}
		if !textutil.IsGenericTitle(candidate) {
			return candidate
		}
	}

	return fallback
}

// isAllowedBasicContentType decides whether a response's Content-Type is
// safe for the readability extractor (text-y formats only).
//
// Empty Content-Type is treated as allowed deliberately. A non-trivial
// fraction of small / personal sites omit the header entirely; rejecting
// them would push every such site through to the jina upgrade path
// unnecessarily. The downside is that a server returning binary bytes
// without a header will reach readability — but readability degrades
// gracefully on non-HTML input (it just produces empty title / body
// which the pipeline already classifies as fetch_quality:weak), so the
// worst case is a single low-confidence link, not a panic or a
// security issue.
//
// If we ever want to flip this default, first instrument the call rate
// with metrics.ParseRunsTotal{fetcher_type="basic_no_content_type"} so
// we can compare hit-rate against the cost of the upgrade path.
func isAllowedBasicContentType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return true
	}
	return strings.Contains(value, "text/") ||
		strings.Contains(value, "html") ||
		strings.Contains(value, "xml") ||
		strings.Contains(value, "json") ||
		strings.Contains(value, "javascript")
}
