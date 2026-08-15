package analyzer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIAnalyzerSummarizeInboxReturnsOnlyStructuredFields(t *testing.T) {
	const body = "private inbox body"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		if len(payload.Messages) != 2 || payload.Messages[1].Role != "user" || !strings.Contains(payload.Messages[1].Content, body) {
			http.Error(writer, "body was not sent as user context", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"content":"{\"title\":\"ignored\",\"summary\":\"structured summary\",\"tags\":[\"Reader\"]}"}}]}`))
	}))
	defer server.Close()

	client := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              server.URL,
		Model:                "reader-model",
		APIKey:               "test",
		HTTPClient:           server.Client(),
		EmptyResponseRetries: 1,
		MinTags:              1,
		MaxTags:              3,
	})

	summary, tags, err := client.SummarizeInbox(t.Context(), body, []string{"user-tag"})
	if err != nil {
		t.Fatalf("SummarizeInbox() error = %v", err)
	}
	if summary != "structured summary" || len(tags) != 1 || tags[0] != "Reader" {
		t.Fatalf("SummarizeInbox() = summary %q tags %#v", summary, tags)
	}
}
