// Package fetcher – HostBoundHTMLFetcher template.
//
// HostBoundHTMLFetcher generalises "match a known host set, GET its
// HTML with site-specific UA + per-host rate limit, then run the
// shared readability extractor". Used by wechat today, intended for
// future juejin/zhihu/csdn-style fetchers — anything that is "basic.go
// but with site-specific HTTP headers and outbound throttle".
//
// Not appropriate for fetchers that diverge from the HTML/readability
// shape: arxiv (XML API), github (REST + README path), pdf (binary
// decode). Those stay in their own files.
package fetcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"webtag/internal/errsafe"
)

// HostBoundHTMLFetcher is a configurable template that satisfies the
// Fetcher interface. Construct via NewHostBoundHTMLFetcher; callers
// (one per site) set the host list + UA + optional limiter then hand
// the result to the Router.
type HostBoundHTMLFetcher struct {
	client     *HTTPClient
	hosts      []string
	userAgent  string
	timeout    time.Duration
	maxBytes   int64
	headers    map[string]string
	limiter    *OutboundLimiter
	fetcherTag string
}

// HostBoundHTMLFetcherOptions bundles the site-specific configuration.
// Zero/empty fields fall back to BasicFetcher's defaults so a minimal
// declaration only needs Hosts + FetcherTag.
type HostBoundHTMLFetcherOptions struct {
	Hosts      []string          // exact host match list (required)
	UserAgent  string            // default: defaultUserAgent
	Timeout    time.Duration     // default: defaultBasicTimeout
	MaxBytes   int64             // default: defaultBasicMaxBytes
	Headers    map[string]string // optional extra request headers
	Limiter    *OutboundLimiter  // optional per-host throttle
	FetcherTag string            // populates Content.FetcherType (required)
}

// NewHostBoundHTMLFetcher wires the template against a shared client.
func NewHostBoundHTMLFetcher(client *HTTPClient, opts HostBoundHTMLFetcherOptions) *HostBoundHTMLFetcher {
	f := &HostBoundHTMLFetcher{
		client:     ensureHTTPClient(client),
		hosts:      opts.Hosts,
		userAgent:  opts.UserAgent,
		timeout:    opts.Timeout,
		maxBytes:   opts.MaxBytes,
		headers:    opts.Headers,
		limiter:    opts.Limiter,
		fetcherTag: opts.FetcherTag,
	}
	if f.userAgent == "" {
		f.userAgent = defaultUserAgent
	}
	if f.timeout <= 0 {
		f.timeout = defaultBasicTimeout
	}
	if f.maxBytes <= 0 {
		f.maxBytes = defaultBasicMaxBytes
	}
	if f.fetcherTag == "" {
		f.fetcherTag = "host_bound_html"
	}
	return f
}

// CanHandle 当 URL 的 host 与配置的 hosts 列表精确（大小写不敏感）匹配时返回 true。
func (f *HostBoundHTMLFetcher) CanHandle(url string) bool {
	return hostMatches(url, f.hosts)
}

// Fetch 先经 OutboundLimiter 排队，再带站点专属 UA / 头发 GET 请求，对响应跑 readability 抽出正文。
func (f *HostBoundHTMLFetcher) Fetch(ctx context.Context, url string) (Content, error) {
	if f.limiter != nil {
		if err := f.limiter.Wait(ctx, hostFromURL(url)); err != nil {
			return Content{}, &FetchError{URL: url, Reason: "rate limit wait failed", Err: err}
		}
	}

	req, cancel, err := f.client.NewRequest(ctx, f.timeout, http.MethodGet, url, nil)
	if err != nil {
		return Content{}, &FetchError{URL: url, Reason: "build request failed", Err: err}
	}
	defer cancel()

	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	for k, v := range f.headers {
		req.Header.Set(k, v)
	}

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

	data, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return Content{}, &FetchError{URL: url, Reason: "read body failed", Err: err}
	}
	if int64(len(data)) > f.maxBytes {
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
		FetcherType: f.fetcherTag,
	}, nil
}
