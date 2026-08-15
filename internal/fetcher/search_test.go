package fetcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDuckDuckGoSearcherReturnsEmptyWhenResultsHaveNoUsableText(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><div class="result"><a class="result__a"></a><div class="result__snippet">   </div></div></body></html>`))
	}))
	defer server.Close()

	searcher := NewDuckDuckGoSearcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	searcher.BaseURL = server.URL

	got, err := searcher.Search(context.Background(), "example query")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got != "" {
		t.Fatalf("Search() = %q, want empty string", got)
	}
}

func TestTruncateRunesKeepsWholeChineseCharacters(t *testing.T) {
	t.Parallel()

	got := truncateRunes("你好世界再见", 4)
	if got != "你好世界" {
		t.Fatalf("truncateRunes() = %q, want 你好世界", got)
	}
}

func TestDuckDuckGoSearcherRetriesTransientHTTPFailures(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><div class="result"><a class="result__a">Recovered</a><div class="result__snippet">Recovered snippet</div></div></body></html>`))
	}))
	defer server.Close()

	searcher := NewDuckDuckGoSearcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	searcher.BaseURL = server.URL

	got, err := searcher.Search(context.Background(), "example query")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("HTTP calls = %d, want 2", calls.Load())
	}
	if got == "" {
		t.Fatal("Search() = empty, want recovered result")
	}
}

func TestDuckDuckGoSearcherRejectsOversizedResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(strings.Repeat("A", 128)))
	}))
	defer server.Close()

	searcher := NewDuckDuckGoSearcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	searcher.BaseURL = server.URL
	searcher.MaxBytes = 16

	_, err := searcher.Search(context.Background(), "example query")
	if err == nil {
		t.Fatal("Search() error = nil, want oversized response error")
	}
	if !strings.Contains(err.Error(), "response too large") {
		t.Fatalf("Search() error = %v, want oversized response error", err)
	}
}

func TestDuckDuckGoSearcherRejectsUnexpectedContentType(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"unexpected":"payload"}`))
	}))
	defer server.Close()

	searcher := NewDuckDuckGoSearcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	searcher.BaseURL = server.URL

	_, err := searcher.Search(context.Background(), "example query")
	if err == nil {
		t.Fatal("Search() error = nil, want unexpected content-type error")
	}
	if !strings.Contains(err.Error(), "content-type") {
		t.Fatalf("Search() error = %v, want content-type error", err)
	}
}

func TestBuildQueryFallsBackToURLKeywordsWhenTitleIsGeneric(t *testing.T) {
	t.Parallel()

	got := BuildQuery("https://example.com/posts/go-parser-quality", "Home")
	want := "example.com posts go parser quality"
	if got != want {
		t.Fatalf("BuildQuery() = %q, want %q", got, want)
	}
}

func TestBuildQueryPrefersInformativeTitle(t *testing.T) {
	t.Parallel()

	got := BuildQuery("https://example.com/posts/go-parser-quality", "Go Parser Quality")
	if got != "Go Parser Quality" {
		t.Fatalf("BuildQuery() = %q, want informative title", got)
	}
}

func TestBuildQueryKeepsShortInformativeTitle(t *testing.T) {
	t.Parallel()

	got := BuildQuery("https://example.com/lang/rust", "Rust")
	if got != "Rust" {
		t.Fatalf("BuildQuery() = %q, want short informative title preserved", got)
	}
}
