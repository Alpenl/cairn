package analyzer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"webtag/internal/fetcher"
)

func TestOpenAIAnalyzerStillRetriesTransientHTTPFailuresWithNonJSONErrorBodies(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := calls.Add(1)
		if count == 1 {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("temporary upstream proxy failure"))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": "{\"summary\":\"恢复成功\",\"tags\":[\"Go\"]}",
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
		EmptyResponseRetries: 2,
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
		t.Fatalf("Analyze() error = %v, want retry then success", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("HTTP calls = %d, want 2", calls.Load())
	}
	if got.Summary != "恢复成功" {
		t.Fatalf("summary = %q, want 恢复成功", got.Summary)
	}
}

func TestOpenAIAnalyzerRetriesEmptyContentResponses(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{
						"message": map[string]any{"content": ""},
					},
				},
			})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": "{\"summary\":\"补偿摘要\",\"tags\":[\"Go\",\"Queue\"]}",
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
		HTTPClient:           server.Client(),
		EmptyResponseRetries: 2,
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
		t.Fatalf("Analyze() error = %v", err)
	}

	if calls.Load() != 2 {
		t.Fatalf("HTTP calls = %d, want 2", calls.Load())
	}

	if got.Summary != "补偿摘要" {
		t.Fatalf("summary = %q, want 补偿摘要", got.Summary)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "Go" || got.Tags[1] != "Queue" {
		t.Fatalf("tags = %#v, want [Go Queue]", got.Tags)
	}
}

func TestOpenAIAnalyzerAcceptsContentPartsArrayResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": []map[string]any{
							{
								"type": "output_text",
								"text": "下面是结果：",
							},
							{
								"type": "output_text",
								"text": "{\"summary\":\"分片兼容\",\"tags\":[\"Go\",\"JSON\"]}",
							},
						},
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
		HTTPClient:           server.Client(),
		EmptyResponseRetries: 1,
		MaxSummaryChars:      20,
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
		t.Fatalf("Analyze() error = %v", err)
	}
	if got.Summary != "分片兼容" {
		t.Fatalf("summary = %q, want 分片兼容", got.Summary)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "Go" || got.Tags[1] != "JSON" {
		t.Fatalf("tags = %#v, want [Go JSON]", got.Tags)
	}
}

func TestOpenAIAnalyzerAcceptsSingleContentPartObjectResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": map[string]any{
							"type": "output_text",
							"text": "{\"summary\":\"单对象兼容\",\"tags\":[\"Go\",\"Object\"]}",
						},
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
		HTTPClient:           server.Client(),
		EmptyResponseRetries: 1,
		MaxSummaryChars:      20,
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
		t.Fatalf("Analyze() error = %v, want success", err)
	}
	if got.Summary != "单对象兼容" {
		t.Fatalf("summary = %q, want 单对象兼容", got.Summary)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "Go" || got.Tags[1] != "Object" {
		t.Fatalf("tags = %#v, want [Go Object]", got.Tags)
	}
}

func TestOpenAIAnalyzerRetriesTransientHTTPFailures(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"temporary upstream failure"}`))
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": "{\"summary\":\"恢复成功\",\"tags\":[\"Go\",\"Retry\"]}",
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
		HTTPClient:           server.Client(),
		EmptyResponseRetries: 2,
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
		t.Fatalf("Analyze() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("HTTP calls = %d, want 2", calls.Load())
	}
	if got.Summary != "恢复成功" {
		t.Fatalf("summary = %q, want 恢复成功", got.Summary)
	}
}

func TestOpenAIAnalyzerRetriesMalformedFirstResponse(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{
						"message": map[string]any{
							"content": "not valid json at all",
						},
					},
				},
			})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": "{\"summary\":\"第二次成功\",\"tags\":[\"Go\",\"Parser\"]}",
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
		HTTPClient:           server.Client(),
		EmptyResponseRetries: 2,
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
		t.Fatalf("Analyze() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("HTTP calls = %d, want 2", calls.Load())
	}
	if got.Summary != "第二次成功" {
		t.Fatalf("summary = %q, want 第二次成功", got.Summary)
	}
}

func TestOpenAIAnalyzerDoesNotRetryPermanentHTTPFailures(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad api key"}`))
	}))
	defer server.Close()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              server.URL,
		APIKey:               "secret-key",
		Model:                "gpt-test",
		HTTPClient:           server.Client(),
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
		t.Fatal("Analyze() error = nil, want unauthorized error")
	}
	if calls.Load() != 1 {
		t.Fatalf("HTTP calls = %d, want 1 for permanent failure", calls.Load())
	}
}

func TestOpenAIAnalyzerRetriesMalformedTopLevelResponseEnvelope(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			_, _ = w.Write([]byte(`{"choices":[`))
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": "{\"summary\":\"包体恢复\",\"tags\":[\"Go\",\"Retry\"]}",
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
		HTTPClient:           server.Client(),
		EmptyResponseRetries: 2,
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
		t.Fatalf("Analyze() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("HTTP calls = %d, want 2", calls.Load())
	}
	if got.Summary != "包体恢复" {
		t.Fatalf("summary = %q, want 包体恢复", got.Summary)
	}
}
