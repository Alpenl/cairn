package fetcher

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	nurl "net/url"
	"regexp"
	"strings"
	"time"

	"webtag/internal/errsafe"
)

const (
	defaultArxivTimeout  = 15 * time.Second
	defaultArxivAPIBase  = "https://export.arxiv.org/api/query"
	defaultArxivMaxBytes = 1 << 20
)

var (
	arxivHostPattern = regexp.MustCompile(`(?i)^https?://(?:www\.|export\.)?arxiv\.org/`)
	arxivPathPattern = regexp.MustCompile(`(?i)^/(abs|pdf|html|ps)/(.+?)(?:\.pdf)?/?$`)
)

// ArxivFetcher 通过 arXiv Atom API 抓取论文元信息（标题、摘要、作者、分类等），不直接爬网页。
type ArxivFetcher struct {
	client     *HTTPClient
	Timeout    time.Duration
	APIBaseURL string
	MaxBytes   int64
	UserAgent  string
}

// NewArxivFetcher 用默认 API 地址和超时构造 ArxivFetcher。
func NewArxivFetcher(client *HTTPClient) *ArxivFetcher {
	return &ArxivFetcher{
		client:     ensureHTTPClient(client),
		Timeout:    defaultArxivTimeout,
		APIBaseURL: defaultArxivAPIBase,
		MaxBytes:   defaultArxivMaxBytes,
		UserAgent:  defaultUserAgent,
	}
}

// CanHandle 当 URL 是可识别的 arXiv 论文链接（abs/pdf/html/ps 路径）时返回 true。
func (f *ArxivFetcher) CanHandle(url string) bool {
	_, ok := extractArxivID(url)
	return ok
}

// Fetch 调 arXiv Atom API 取第一条 entry，把标题 / summary / 元数据封装成 Content 返回。
//
//nolint:gocyclo // reason: 单一 HTTP fetch + Atom 解析 + 多字段提取的线性流程，拆成 helper 反而要在多函数间传 entry/feed 状态，不利于阅读。
func (f *ArxivFetcher) Fetch(ctx context.Context, url string) (Content, error) {
	id, ok := extractArxivID(url)
	if !ok {
		return Content{}, &FetchError{URL: url, Reason: "URL is not a recognizable arXiv paper"}
	}

	target := fmt.Sprintf("%s?id_list=%s", strings.TrimRight(f.APIBaseURL, "?&"), nurl.QueryEscape(id))
	req, cancel, err := f.client.NewRequest(ctx, f.Timeout, http.MethodGet, target, nil)
	if err != nil {
		return Content{}, &FetchError{URL: url, Reason: "build arXiv request failed", Err: err}
	}
	defer cancel()

	req.Header.Set("User-Agent", f.UserAgent)
	req.Header.Set("Accept", "application/atom+xml, application/xml;q=0.9")

	resp, err := f.client.DoWithRetry(req)
	if err != nil {
		return Content{}, &FetchError{URL: url, Reason: "arXiv API request failed", Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return Content{}, &FetchError{URL: url, Reason: "arXiv paper not found"}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Content{}, &FetchError{URL: url, Reason: fmt.Sprintf("arXiv API HTTP %d", resp.StatusCode), Err: errsafe.ErrUpstreamHTTP}
	}

	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if contentType != "" && !strings.Contains(contentType, "xml") {
		return Content{}, &FetchError{URL: url, Reason: "arXiv response content-type is not XML", Err: errsafe.ErrContentType}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, f.MaxBytes+1))
	if err != nil {
		return Content{}, &FetchError{URL: url, Reason: "read arXiv response failed", Err: err}
	}
	if f.MaxBytes > 0 && int64(len(body)) > f.MaxBytes {
		return Content{}, &FetchError{URL: url, Reason: "arXiv response too large", Err: errsafe.ErrResponseTooLarge}
	}

	feed, err := parseArxivFeed(body)
	if err != nil {
		return Content{}, &FetchError{URL: url, Reason: "decode arXiv response failed", Err: err}
	}
	if len(feed.Entries) == 0 {
		return Content{}, &FetchError{URL: url, Reason: "arXiv response contains no entries"}
	}

	entry := feed.Entries[0]
	title := normalizeSpace(entry.Title)
	if title == "" {
		return Content{}, &FetchError{URL: url, Reason: "arXiv entry missing title"}
	}

	summary := strings.TrimSpace(entry.Summary)

	authors := make([]string, 0, len(entry.Authors))
	for _, a := range entry.Authors {
		name := normalizeSpace(a.Name)
		if name != "" {
			authors = append(authors, name)
		}
	}

	pdfURL, htmlURL := extractArxivLinks(entry.Links)

	metadata := map[string]any{
		"arxiv_id": id,
	}
	if len(authors) > 0 {
		metadata["authors"] = authors
	}
	if cat := normalizeSpace(entry.PrimaryCategory.Term); cat != "" {
		metadata["primary_category"] = cat
	}
	if entry.Published != "" {
		metadata["published"] = entry.Published
	}
	if entry.Updated != "" {
		metadata["updated"] = entry.Updated
	}
	if doi := normalizeSpace(entry.DOI); doi != "" {
		metadata["doi"] = doi
	}
	if ref := normalizeSpace(entry.JournalRef); ref != "" {
		metadata["journal_ref"] = ref
	}
	if pdfURL != "" {
		metadata["pdf_url"] = pdfURL
	}
	if htmlURL != "" {
		metadata["html_url"] = htmlURL
	}

	return Content{
		URL:         url,
		Title:       title,
		Body:        summary,
		Metadata:    metadata,
		FetcherType: "arxiv",
	}, nil
}

func extractArxivID(url string) (string, bool) {
	if !arxivHostPattern.MatchString(url) {
		return "", false
	}
	parsed, err := nurl.Parse(url)
	if err != nil {
		return "", false
	}
	matches := arxivPathPattern.FindStringSubmatch(parsed.Path)
	if len(matches) != 3 {
		return "", false
	}
	id := strings.Trim(matches[2], "/")
	if id == "" {
		return "", false
	}
	return id, true
}

func extractArxivLinks(links []arxivLink) (pdf string, htmlPage string) {
	for _, link := range links {
		href := strings.TrimSpace(link.Href)
		if href == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(link.Title), "pdf") {
			pdf = href
			continue
		}
		if strings.EqualFold(link.Type, "application/pdf") {
			pdf = href
			continue
		}
		if strings.EqualFold(link.Rel, "alternate") || link.Rel == "" {
			if htmlPage == "" {
				htmlPage = href
			}
		}
	}
	return pdf, htmlPage
}

type arxivFeed struct {
	XMLName xml.Name     `xml:"feed"`
	Entries []arxivEntry `xml:"entry"`
}

type arxivEntry struct {
	Title           string               `xml:"title"`
	Summary         string               `xml:"summary"`
	Published       string               `xml:"published"`
	Updated         string               `xml:"updated"`
	Authors         []arxivAuthor        `xml:"author"`
	Links           []arxivLink          `xml:"link"`
	PrimaryCategory arxivPrimaryCategory `xml:"primary_category"`
	DOI             string               `xml:"doi"`
	JournalRef      string               `xml:"journal_ref"`
}

type arxivAuthor struct {
	Name string `xml:"name"`
}

type arxivLink struct {
	Href  string `xml:"href,attr"`
	Rel   string `xml:"rel,attr"`
	Type  string `xml:"type,attr"`
	Title string `xml:"title,attr"`
}

type arxivPrimaryCategory struct {
	Term string `xml:"term,attr"`
}

func parseArxivFeed(body []byte) (arxivFeed, error) {
	var feed arxivFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return arxivFeed{}, err
	}
	return feed, nil
}
