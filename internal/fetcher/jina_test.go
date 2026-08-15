package fetcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestJinaFetcherRejectsEmptyContent(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("   "))
	}))
	defer server.Close()

	fetcher := NewJinaFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	fetcher.BaseURL = server.URL

	_, err := fetcher.Fetch(context.Background(), "https://example.com/post")
	if err == nil {
		t.Fatal("Fetch() error = nil, want empty content error")
	}
	if !strings.Contains(err.Error(), "empty content") {
		t.Fatalf("Fetch() error = %v, want empty content error", err)
	}
}

func TestJinaFetcherDoesNotDiscloseCredentialedOrSignedURL(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()

	fetcher := NewJinaFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	fetcher.BaseURL = server.URL
	sensitive := "https://alice:password@example.com/post?signature=query-secret#fragment-secret"
	_, err := fetcher.Fetch(context.Background(), sensitive)
	if err == nil {
		t.Fatal("Fetch() error = nil, want privacy-policy rejection")
	}
	if calls.Load() != 0 {
		t.Fatalf("Jina HTTP calls=%d, want 0", calls.Load())
	}
	for _, sentinel := range []string{"alice", "password", "signature", "query-secret", "fragment-secret"} {
		if strings.Contains(err.Error(), sentinel) {
			t.Fatalf("privacy error leaked %q: %v", sentinel, err)
		}
	}
}

// TestJinaFetcherRejectsSoftFailureErrorPage: jina returns HTTP 200 with a
// "Warning: Target URL returned error NNN" body and empty Markdown Content
// when the target is unreachable (anti-bot / 4xx-5xx / Cloudflare 521). This
// must be treated as a fetch failure, not passed downstream as content — else
// the analyzer hallucinates a summary for a page that does not exist.
func TestJinaFetcherRejectsSoftFailureErrorPage(t *testing.T) {
	t.Parallel()

	body := "Title: \n\nURL Source: https://blog.csdn.net/x/article/details/123\n\n" +
		"Warning: Target URL returned error 521\nWarning: This page maybe not yet fully loaded.\n\nMarkdown Content:\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	fetcher := NewJinaFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	fetcher.BaseURL = server.URL

	_, err := fetcher.Fetch(context.Background(), "https://blog.csdn.net/x/article/details/123")
	if err == nil {
		t.Fatal("Fetch() error = nil, want error-page failure")
	}
	if !strings.Contains(err.Error(), "error page") {
		t.Fatalf("Fetch() error = %v, want error-page failure", err)
	}
}

// TestJinaSoftFailureKeepsRealContentDespiteWarning: a warning line alongside
// real Markdown Content must NOT be rejected (partial-load pages still有正文).
func TestJinaSoftFailureKeepsRealContentDespiteWarning(t *testing.T) {
	t.Parallel()
	text := "Title: 真标题\n\nWarning: Target URL returned error 502\n\nMarkdown Content:\n这里是真实抓到的正文内容，足够长可用。"
	if jinaSoftFailure(text) {
		t.Error("jinaSoftFailure = true, want false when Markdown Content has real body")
	}
}

func TestJinaFetcherRejectsOversizedResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(strings.Repeat("A", 64)))
	}))
	defer server.Close()

	fetcher := NewJinaFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	fetcher.BaseURL = server.URL
	fetcher.MaxBytes = 16

	_, err := fetcher.Fetch(context.Background(), "https://example.com/post")
	if err == nil {
		t.Fatal("Fetch() error = nil, want oversized response error")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("Fetch() error = %v, want oversized response error", err)
	}
}

func TestJinaFetcherRetriesTransientHTTPFailures(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Title: Recovered\nRecovered body content"))
	}))
	defer server.Close()

	fetcher := NewJinaFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	fetcher.BaseURL = server.URL

	got, err := fetcher.Fetch(context.Background(), "https://example.com/post")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("HTTP calls = %d, want 2", calls.Load())
	}
	if got.Title != "Recovered" {
		t.Fatalf("Title = %q, want Recovered", got.Title)
	}
}

func TestJinaFetcherRejectsBinaryContentTypes(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("not markdown"))
	}))
	defer server.Close()

	fetcher := NewJinaFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	fetcher.BaseURL = server.URL

	_, err := fetcher.Fetch(context.Background(), "https://example.com/post")
	if err == nil {
		t.Fatal("Fetch() error = nil, want binary content-type rejection")
	}
	if !strings.Contains(err.Error(), "content-type") {
		t.Fatalf("Fetch() error = %v, want content-type error", err)
	}
}

func TestJinaFetcherAllowsTextMarkdownResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte("Title: Markdown\nMarkdown body"))
	}))
	defer server.Close()

	fetcher := NewJinaFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	fetcher.BaseURL = server.URL

	got, err := fetcher.Fetch(context.Background(), "https://example.com/post")
	if err != nil {
		t.Fatalf("Fetch() error = %v, want success for markdown response", err)
	}
	if got.Title != "Markdown" {
		t.Fatalf("Title = %q, want Markdown", got.Title)
	}
}

func TestJinaFetcherRequestsCommentRemoval(t *testing.T) {
	t.Parallel()

	var removeSelector string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		removeSelector = r.Header.Get("X-Remove-Selector")
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte("Title: Clean article\nArticle body"))
	}))
	defer server.Close()

	fetcher := NewJinaFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	fetcher.BaseURL = server.URL

	if _, err := fetcher.Fetch(context.Background(), "https://example.com/post"); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	for _, selector := range []string{"#comments", ".comment-list", `[itemprop="comment"]`} {
		if !strings.Contains(removeSelector, selector) {
			t.Errorf("X-Remove-Selector = %q, want %q", removeSelector, selector)
		}
	}
}
