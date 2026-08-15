package analyzer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAIAnalyzerCompleteUsesPrivatePlainTextPrompt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		if payload.Model != "reader-model" || payload.Stream || len(payload.Messages) != 2 || payload.Messages[0].Role != "system" || payload.Messages[1].Role != "user" {
			http.Error(writer, "unexpected completion shape", http.StatusBadRequest)
			return
		}
		if strings.Contains(payload.Messages[0].Content, "untrusted question") {
			http.Error(writer, "user prompt leaked into system policy", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"content":"  concise answer  "}}]}`))
	}))
	defer server.Close()

	client := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              server.URL,
		Model:                "reader-model",
		APIKey:               "secret",
		HTTPClient:           server.Client(),
		EmptyResponseRetries: 1,
	})

	answer, modelName, err := client.Complete(t.Context(), "untrusted question", "selection")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if answer != "concise answer" || modelName != "reader-model" {
		t.Fatalf("Complete() = (%q, %q), want (concise answer, reader-model)", answer, modelName)
	}

}

func TestOpenAIAnalyzerCompletePreservesCancellationAndTimeout(t *testing.T) {
	t.Run("caller cancellation", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			<-request.Context().Done()
		}))
		defer server.Close()

		client := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
			BaseURL:              server.URL,
			Model:                "reader-model",
			HTTPClient:           server.Client(),
			EmptyResponseRetries: 1,
			RequestTimeout:       time.Second,
		})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, _, err := client.Complete(ctx, "question", "general")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Complete() error = %v, want context.Canceled", err)
		}
	})

	t.Run("provider timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			time.Sleep(100 * time.Millisecond)
		}))
		defer server.Close()

		client := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
			BaseURL:              server.URL,
			Model:                "reader-model",
			HTTPClient:           server.Client(),
			EmptyResponseRetries: 1,
			RequestTimeout:       10 * time.Millisecond,
		})

		_, _, err := client.Complete(t.Context(), "question", "general")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Complete() error = %v, want context.DeadlineExceeded", err)
		}
	})
}

func TestOpenAIAnalyzerCompleteRejectsUnknownScopeBeforeHTTP(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	defer server.Close()

	client := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              server.URL,
		Model:                "reader-model",
		HTTPClient:           server.Client(),
		EmptyResponseRetries: 1,
	})
	if _, _, err := client.Complete(t.Context(), "question", "unknown"); err == nil {
		t.Fatal("Complete() error = nil, want unsupported scope error")
	}
	if calls != 0 {
		t.Fatalf("HTTP calls = %d, want 0 for unsupported scope", calls)
	}
}

func TestOpenAIAnalyzerAvailabilityRequiresProviderURLAndModel(t *testing.T) {
	if NewOpenAIAnalyzer(OpenAIAnalyzerOptions{BaseURL: "https://example.com"}).Available() {
		t.Fatal("analyzer with no model should be unavailable")
	}
	if NewOpenAIAnalyzer(OpenAIAnalyzerOptions{Model: "reader-model"}).Available() {
		t.Fatal("analyzer with no provider URL should be unavailable")
	}
	if !NewOpenAIAnalyzer(OpenAIAnalyzerOptions{BaseURL: "https://example.com", Model: "reader-model"}).Available() {
		t.Fatal("configured analyzer should be available")
	}
}
