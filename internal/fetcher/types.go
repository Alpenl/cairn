// Package fetcher 是 WebTag 的 URL 抓取层，统一对外提供"给 URL 拿正文"的能力。
//
// 主要组成：
//   - 通用 HTTP 客户端（HTTPClient）：带重试、Retry-After 解析、跨主机重定向时丢弃 Authorization；
//   - SSRF 防护（safedial）：请求前预检 + DialContext 重新校验，封堵 DNS rebinding 的 TOCTOU 窗口；
//   - 站点专用抓取器：arxiv / github / wechat / pdf / ytdlp / jina / lightfetch / 通用 basic / host_bound_html 模板；
//   - 路由派发（Router / Manager）：按 URL 选择合适的 Fetcher，并在正文不足时依次回退到 jina、search 摘要；
//   - 出站节流（OutboundLimiter）：对易触发反爬的站点按 host 做请求速率限制；
//   - 观测层（metrics_transport）：包装 RoundTripper 上报抓取耗时指标。
package fetcher

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"webtag/internal/textutil"
)

// Content 是抓取器统一返回的内容结构，承载正文、标题、来源元信息及抓取来源标签。
type Content struct {
	URL         string
	Title       string
	Body        string
	SourceKind  string
	HTML        string
	ImageURLs   []string
	Metadata    map[string]any
	FetcherType string
}

// Fetcher 表示一种抓取策略：先用 CanHandle 判断能否处理给定 URL，再调用 Fetch 实际拉取内容。
type Fetcher interface {
	CanHandle(url string) bool
	Fetch(ctx context.Context, url string) (Content, error)
}

// Searcher 表示一种"以查询字符串换摘要"的搜索能力，供 Manager 在抓取失败时降级使用。
type Searcher interface {
	Search(ctx context.Context, query string) (string, error)
}

// FetchError 是抓取过程中出现错误时返回的结构化错误，保留原始 URL、失败原因与底层错误便于分类。
type FetchError struct {
	URL    string
	Reason string
	Err    error
}

// Error 实现 error 接口，按是否带 Reason / Err 拼装人类可读的错误消息。
func (e *FetchError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Reason == "" && e.Err != nil {
		return fmt.Sprintf("fetch %s: %v", e.URL, e.Err)
	}
	if e.Reason == "" {
		return fmt.Sprintf("fetch %s failed", e.URL)
	}
	return fmt.Sprintf("fetch %s: %s", e.URL, e.Reason)
}

// Unwrap 暴露被包装的底层错误，供 errors.Is / errors.As 链式匹配。
func (e *FetchError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func hasBody(content Content) bool {
	return strings.TrimSpace(content.Body) != ""
}

func isSufficient(content Content, minBodyChars int) bool {
	body := strings.TrimSpace(content.Body)
	if body == "" {
		return false
	}
	if minBodyChars <= 0 {
		return true
	}
	return utf8.RuneCountInString(body) >= minBodyChars
}

func cloneMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}

	cloned := make(map[string]any, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func normalizeSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

// runeCount / truncateRunes / limitTextByRunes are thin in-package
// wrappers around internal/textutil. They exist so the existing fetcher
// callers keep working without sprinkling textutil imports across every
// fetcher file; the canonical implementations live in internal/textutil.
func runeCount(value string) int {
	return textutil.Count(value)
}

func truncateRunes(value string, limit int) string {
	return textutil.Truncate(value, limit)
}

func limitTextByRunes(value string, limit int) string {
	return textutil.LimitTextByRunes(value, limit)
}
