package fetcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	nurl "net/url"
	"strings"
	"time"

	"webtag/internal/errsafe"
	"webtag/internal/security"
)

const (
	defaultJinaTimeout  = 20 * time.Second
	defaultJinaBaseURL  = "https://r.jina.ai/"
	defaultJinaMaxBytes = 2 << 20
)

// JinaFetcher 通过 r.jina.ai 的 Reader API 拿到任意 URL 的服务端无头渲染结果，作为 Manager 的回退手段。
type JinaFetcher struct {
	client    *HTTPClient
	Timeout   time.Duration
	BaseURL   string
	MaxBytes  int64
	UserAgent string
}

// NewJinaFetcher 构造使用默认 r.jina.ai 端点的 JinaFetcher。
func NewJinaFetcher(client *HTTPClient) *JinaFetcher {
	return &JinaFetcher{
		client:    ensureHTTPClient(client),
		Timeout:   defaultJinaTimeout,
		BaseURL:   defaultJinaBaseURL,
		MaxBytes:  defaultJinaMaxBytes,
		UserAgent: defaultUserAgent,
	}
}

// CanHandle 恒为 false：JinaFetcher 是 Manager 的回退专用通道，不应被注册进 Router。
func (f *JinaFetcher) CanHandle(string) bool {
	return false
}

// Fetch 把目标 URL 编码后拼到 r.jina.ai 路径上换取 Markdown 摘要，第一行 "Title:" 拆出标题、其余作为正文。
func (f *JinaFetcher) Fetch(ctx context.Context, url string) (Content, error) {
	projected, delegationSafe := security.ThirdPartyURLProjection(url)
	if !delegationSafe {
		return Content{}, &FetchError{
			Reason: "Jina delegation blocked by URL privacy policy",
			Err:    security.ErrSensitiveURLDisclosure,
		}
	}
	target := strings.TrimRight(f.BaseURL, "/") + "/" + nurl.PathEscape(projected)
	req, cancel, err := f.client.NewRequest(ctx, f.Timeout, http.MethodGet, target, nil)
	if err != nil {
		return Content{}, &FetchError{URL: url, Reason: "build Jina request failed", Err: err}
	}
	defer cancel()

	req.Header.Set("User-Agent", f.UserAgent)
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("X-Return-Format", "markdown")
	req.Header.Set("X-Remove-Selector", discussionRemovalSelector)

	resp, err := f.client.DoWithRetry(req)
	if err != nil {
		return Content{}, &FetchError{URL: url, Reason: "Jina request failed", Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Content{}, &FetchError{URL: url, Reason: fmt.Sprintf("Jina HTTP %d", resp.StatusCode), Err: errsafe.ErrUpstreamHTTP}
	}
	if !isAllowedJinaContentType(resp.Header.Get("Content-Type")) {
		return Content{}, &FetchError{URL: url, Reason: "Jina response content-type is not supported", Err: errsafe.ErrContentType}
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, f.MaxBytes+1))
	if err != nil {
		return Content{}, &FetchError{URL: url, Reason: "read Jina response failed", Err: err}
	}
	if int64(len(data)) > f.MaxBytes {
		return Content{}, &FetchError{URL: url, Reason: "Jina response too large", Err: errsafe.ErrResponseTooLarge}
	}

	text := strings.TrimSpace(string(data))
	if text == "" {
		return Content{}, &FetchError{URL: url, Reason: "Jina returned empty content"}
	}

	// jina 对抓取失败的目标（反爬 / 登录墙 / 4xx-5xx / Cloudflare 521 等）仍返回
	// HTTP 200，但正文只有 "Warning: Target URL returned error NNN" 且
	// "Markdown Content:" 段为空。必须把这种「软失败」识别成抓取失败返回 FetchError，
	// 否则会把错误页文本当正文喂给分析器——模型会据 URL 幻觉出根本不存在的内容
	// （实测一个 521 的 CSDN 文章被编出整篇 Go 错误处理摘要）。
	if jinaSoftFailure(text) {
		return Content{}, &FetchError{URL: url, Reason: "Jina target returned an error page (no content)", Err: errsafe.ErrUpstreamHTTP}
	}

	title, body := parseJinaMarkdown(text)
	if body == "" {
		body = text
	}

	return Content{
		URL:   url,
		Title: title,
		Body:  body,
		Metadata: map[string]any{
			"source": "jina",
		},
		FetcherType: "jina",
	}, nil
}

// jinaSoftFailure 判断 jina 的 200 响应是否其实是抓取失败的错误页：带
// "Warning: Target URL returned error" 警告，且 "Markdown Content:" 段无实质正文。
// 这种页面没有真实内容，必须当抓取失败处理，不能进入分析（否则模型幻觉）。
func jinaSoftFailure(text string) bool {
	if !strings.Contains(text, "Warning: Target URL returned error") {
		return false
	}
	// 取 "Markdown Content:" 之后的真实正文；为空/空白即错误页。
	if idx := strings.Index(text, "Markdown Content:"); idx >= 0 {
		return strings.TrimSpace(text[idx+len("Markdown Content:"):]) == ""
	}
	// 带错误警告却没有 Markdown Content 段，同样视为失败。
	return true
}

func parseJinaMarkdown(text string) (string, string) {
	lines := strings.Split(text, "\n")
	for idx, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Title:") {
			continue
		}

		title := strings.TrimSpace(strings.TrimPrefix(line, "Title:"))
		body := strings.TrimSpace(strings.Join(lines[idx+1:], "\n"))
		return title, body
	}

	return "", strings.TrimSpace(text)
}

func isAllowedJinaContentType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return true
	}
	return strings.Contains(value, "text/") ||
		strings.Contains(value, "markdown") ||
		strings.Contains(value, "json") ||
		strings.Contains(value, "xml")
}
