package analyzer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"webtag/internal/fetcher"
)

func TestOpenAIAnalyzerWaitsBeforeRetryingTransientFailures(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	callTimes := make([]time.Time, 0, 2)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callTimes = append(callTimes, time.Now())
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

	_, err := analyzer.Analyze(context.Background(), AnalyzeRequest{
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
	if callTimes[1].Sub(callTimes[0]) < 10*time.Millisecond {
		t.Fatalf("retry delay = %v, want at least 10ms", callTimes[1].Sub(callTimes[0]))
	}
}

func TestOpenAIAnalyzerHonorsRetryAfterHeader(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	callTimes := make([]time.Time, 0, 2)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callTimes = append(callTimes, time.Now())
		count := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"slow down"}`))
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": "{\"summary\":\"限流恢复\",\"tags\":[\"Go\",\"Retry\"]}",
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

	_, err := analyzer.Analyze(context.Background(), AnalyzeRequest{
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
	// Retry-After: 1s with ±20% jitter → [800ms, 1200ms). Allow a small
	// scheduling slack on the lower bound; we just need to confirm the wait
	// was Retry-After-driven (~1s) rather than the unjittered exponential
	// backoff base which would be much shorter.
	if delay := callTimes[1].Sub(callTimes[0]); delay < 750*time.Millisecond {
		t.Fatalf("retry delay = %v, want Retry-After-driven delay (>=750ms)", delay)
	}
}

func TestOpenAIAnalyzerStopsWhenContextIsCancelledDuringBackoff(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"temporary upstream failure"}`))
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

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := analyzer.Analyze(ctx, AnalyzeRequest{
		Content: fetcher.Content{
			URL:  "https://example.com/post",
			Body: "content",
		},
	})
	if err == nil {
		t.Fatal("Analyze() error = nil, want context cancellation")
	}
	if time.Since(start) >= 150*time.Millisecond {
		t.Fatalf("elapsed = %v, want cancellation to interrupt analyzer backoff", time.Since(start))
	}
}

func TestOpenAIAnalyzerCustomRetryDelayChangesBackoff(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	callTimes := make([]time.Time, 0, 2)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callTimes = append(callTimes, time.Now())
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
		RetryDelay:           80 * time.Millisecond,
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
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("HTTP calls = %d, want 2", calls.Load())
	}
	// Custom RetryDelay=80ms with ±20% jitter lands in [64ms, 96ms]; the
	// 60ms floor still excludes the default 25ms baseline (≤30ms post-jitter).
	if elapsed := callTimes[1].Sub(callTimes[0]); elapsed < 60*time.Millisecond {
		t.Fatalf("retry delay = %v, want custom delay to apply", elapsed)
	}
}

func TestOpenAIAnalyzerDefaultRequestTimeoutAllowsModeratelySlowResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": "{\"summary\":\"default timeout ok\",\"tags\":[\"Go\"]}",
					},
				},
			},
		})
	}))
	defer server.Close()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         server.URL,
		APIKey:          "secret-key",
		Model:           "gpt-test",
		HTTPClient:      server.Client(),
		MaxSummaryChars: 50,
		MinTags:         1,
		MaxTags:         5,
		MaxTagChars:     12,
	})

	got, err := analyzer.Analyze(context.Background(), AnalyzeRequest{
		Content: fetcher.Content{
			URL:  "https://example.com/post",
			Body: "content",
		},
	})
	if err != nil {
		t.Fatalf("Analyze() error = %v, want success under default timeout", err)
	}
	if got.Summary != "default timeout ok" {
		t.Fatalf("summary = %q, want default timeout ok", got.Summary)
	}
}

func TestOpenAIAnalyzerDefaultRetriesRecoverAfterTwoTransientFailures(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if count < 3 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"temporary upstream failure"}`))
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": "{\"summary\":\"default retries ok\",\"tags\":[\"Go\"]}",
					},
				},
			},
		})
	}))
	defer server.Close()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         server.URL,
		APIKey:          "secret-key",
		Model:           "gpt-test",
		HTTPClient:      server.Client(),
		RetryDelay:      5 * time.Millisecond,
		MaxSummaryChars: 50,
		MinTags:         1,
		MaxTags:         5,
		MaxTagChars:     12,
	})

	got, err := analyzer.Analyze(context.Background(), AnalyzeRequest{
		Content: fetcher.Content{
			URL:  "https://example.com/post",
			Body: "content",
		},
	})
	if err != nil {
		t.Fatalf("Analyze() error = %v, want success after default retries", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("HTTP calls = %d, want 3 from default retry budget", calls.Load())
	}
	if got.Summary != "default retries ok" {
		t.Fatalf("summary = %q, want default retries ok", got.Summary)
	}
}

func TestOpenAIAnalyzerTimesOutSlowRequests(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	// The upstream blocks until the test is done instead of sleeping just past
	// the timeout. The previous shape slept 1600ms against the 1500ms default and
	// asserted elapsed < 1550ms — a 50ms margin that a loaded `-race` runner eats
	// whole. It flaked in CI at 1.552s, i.e. 2ms over, which says nothing about
	// the behaviour under test.
	//
	// Now the upstream never answers at all, so the only thing that can end the
	// call is the client's own timeout, and the elapsed assertion needs no tight
	// margin to mean something.
	upstreamBlocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-upstreamBlocked:
		case <-r.Context().Done():
		}
	}))
	// LIFO: unblock the handler before closing the server, so Close never waits
	// on a handler that is waiting on us.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(upstreamBlocked) })

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              server.URL,
		APIKey:               "secret-key",
		Model:                "gpt-test",
		HTTPClient:           server.Client(),
		EmptyResponseRetries: 3,
		RetryDelay:           10 * time.Millisecond,
		MaxSummaryChars:      50,
		MinTags:              1,
		MaxTags:              5,
		MaxTagChars:          12,
	})

	// Analyze runs off the test goroutine so a regression that drops the request
	// timeout fails here with a readable message instead of hanging until the
	// whole test binary's deadline expires. Nothing else can end this call: the
	// upstream stays silent and the context passed in never expires.
	analyzed := make(chan error, 1)
	go func() {
		_, err := analyzer.Analyze(context.Background(), AnalyzeRequest{
			Content: fetcher.Content{
				URL:  "https://example.com/post",
				Body: "content",
			},
		})
		analyzed <- err
	}()

	// The ceiling is deliberately far above the 1500ms default. It is not a
	// timing assertion — it is the difference between "the analyzer gave up on
	// its own" and "nothing ever gives up".
	select {
	case err := <-analyzed:
		if err == nil {
			t.Fatal("Analyze() error = nil, want timeout")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Analyze() did not return while the upstream stayed silent; the request timeout never fired")
	}
	if calls.Load() != 1 {
		t.Fatalf("HTTP calls = %d, want 1 because timeout should not be retried", calls.Load())
	}
}

func TestOpenAIAnalyzerCustomRequestTimeoutOverridesDefault(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": "{\"summary\":\"in time\",\"tags\":[\"Go\"]}",
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
		RetryDelay:           10 * time.Millisecond,
		RequestTimeout:       150 * time.Millisecond,
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
		t.Fatalf("Analyze() error = %v, want success with custom timeout", err)
	}
	if got.Summary != "in time" {
		t.Fatalf("summary = %q, want in time", got.Summary)
	}
}
