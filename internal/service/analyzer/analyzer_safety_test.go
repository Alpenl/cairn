package analyzer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"webtag/internal/fetcher"
)

func TestOpenAIAnalyzerRejectsUnsafeBaseURLByDefault(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not receive analyzer request for blocked unsafe base URL")
	}))
	defer server.Close()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              server.URL,
		APIKey:               "secret-key",
		Model:                "gpt-test",
		HTTPClient:           fetcher.NewHTTPClient(server.Client()).Raw(),
		EmptyResponseRetries: 1,
		MaxSummaryChars:      50,
		MinTags:              1,
		MaxTags:              5,
		MaxTagChars:          12,
	})

	_, err := analyzer.Analyze(context.Background(), AnalyzeRequest{
		Content: fetcher.Content{
			URL:  "https://example.com/post",
			Body: "content",
		},
	})
	if err == nil {
		t.Fatal("Analyze() error = nil, want unsafe target rejection")
	}
	if !strings.Contains(err.Error(), "unsafe target host") {
		t.Fatalf("Analyze() error = %v, want unsafe target host error", err)
	}
}

func TestOpenAIAnalyzerDefaultHTTPClientRejectsUnsafeBaseURL(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Fatal("server should not receive analyzer request from default hardened client")
	}))
	defer server.Close()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              server.URL,
		APIKey:               "secret-key",
		Model:                "gpt-test",
		EmptyResponseRetries: 3,
		MaxSummaryChars:      50,
		MinTags:              1,
		MaxTags:              5,
		MaxTagChars:          12,
	})

	_, err := analyzer.Analyze(context.Background(), AnalyzeRequest{
		Content: fetcher.Content{
			URL:  "https://example.com/post",
			Body: "content",
		},
	})
	if err == nil {
		t.Fatal("Analyze() error = nil, want unsafe target rejection from default client")
	}
	if !strings.Contains(err.Error(), "unsafe target host") {
		t.Fatalf("Analyze() error = %v, want unsafe target host error", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("HTTP calls = %d, want 0 for blocked unsafe default client", calls.Load())
	}
}

func TestOpenAIAnalyzerAllowsUnsafeBaseURLWhenExplicitlyEnabled(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": "{\"summary\":\"private gateway ok\",\"tags\":[\"Go\"]}",
					},
				},
			},
		})
	}))
	defer server.Close()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              server.URL,
		APIKey:               "secret-key",
		Model:                "gpt-test",
		HTTPClient:           fetcher.NewHTTPClientWithOptions(fetcher.HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}).Raw(),
		EmptyResponseRetries: 1,
		MaxSummaryChars:      50,
		MinTags:              1,
		MaxTags:              5,
		MaxTagChars:          12,
	})

	got, err := analyzer.Analyze(context.Background(), AnalyzeRequest{
		Content: fetcher.Content{
			URL:  "https://example.com/post",
			Body: "content",
		},
	})
	if err != nil {
		t.Fatalf("Analyze() error = %v, want success with unsafe targets explicitly enabled", err)
	}
	if got.Summary != "private gateway ok" {
		t.Fatalf("summary = %q, want private gateway ok", got.Summary)
	}
}

func TestOpenAIAnalyzerRejectsOversizedResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Repeat("A", 256)))
	}))
	defer server.Close()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              server.URL,
		APIKey:               "secret-key",
		Model:                "gpt-test",
		HTTPClient:           fetcher.NewHTTPClientWithOptions(fetcher.HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}).Raw(),
		EmptyResponseRetries: 1,
		MaxSummaryChars:      50,
		MinTags:              1,
		MaxTags:              5,
		MaxTagChars:          12,
		MaxResponseBytes:     32,
	})

	_, err := analyzer.Analyze(context.Background(), AnalyzeRequest{
		Content: fetcher.Content{
			URL:  "https://example.com/post",
			Body: "content",
		},
	})
	if err == nil {
		t.Fatal("Analyze() error = nil, want oversized analyzer response rejection")
	}
	if !strings.Contains(err.Error(), "response too large") {
		t.Fatalf("Analyze() error = %v, want oversized response error", err)
	}
}

func TestOpenAIAnalyzerRejectsUnexpectedContentType(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"ok\",\"tags\":[\"Go\"]}"}}]}`))
	}))
	defer server.Close()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              server.URL,
		APIKey:               "secret-key",
		Model:                "gpt-test",
		HTTPClient:           fetcher.NewHTTPClientWithOptions(fetcher.HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}).Raw(),
		EmptyResponseRetries: 1,
		MaxSummaryChars:      50,
		MinTags:              1,
		MaxTags:              5,
		MaxTagChars:          12,
	})

	_, err := analyzer.Analyze(context.Background(), AnalyzeRequest{
		Content: fetcher.Content{
			URL:  "https://example.com/post",
			Body: "content",
		},
	})
	if err == nil {
		t.Fatal("Analyze() error = nil, want analyzer content-type rejection")
	}
	if !strings.Contains(err.Error(), "content-type") {
		t.Fatalf("Analyze() error = %v, want content-type error", err)
	}
}
