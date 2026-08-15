package analyzer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"webtag/internal/fetcher"
)

func newURLDirectAnalyzer() *OpenAIAnalyzer {
	return NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		APIKey:          "k",
		Model:           "grok-test",
		MaxSummaryChars: 600,
		MinTags:         2,
		MaxTags:         5,
		MaxTagChars:     20,
	})
}

// TestURLDirectPromptInstructsFetchAndAccessible verifies the grok-direct
// system prompt tells the model to fetch the URL and carries the
// accessible/title JSON contract plus the shared tag rules.
func TestURLDirectPromptInstructsFetchAndAccessible(t *testing.T) {
	t.Parallel()
	got := buildURLDirectPrompt(nil, "article")
	for _, want := range []string{"抓取", "accessible", "title", "summary", "tags"} {
		if !strings.Contains(got, want) {
			t.Errorf("url-direct prompt missing %q\n%s", want, got)
		}
	}
	if !strings.Contains(got, contentTypeHints["article"]) {
		t.Errorf("url-direct prompt missing article content-type hint")
	}
}

// TestURLDirectUserPromptIsJustURL verifies the user message in url-direct
// mode is only the URL (+ note), with no fetched body/title block.
func TestURLDirectUserPromptIsJustURL(t *testing.T) {
	t.Parallel()
	a := newURLDirectAnalyzer()
	note := "感兴趣"
	out := a.buildURLDirectUserPrompt(AnalyzeRequest{
		Content:         fetcher.Content{URL: "https://go.dev/x"},
		UserDescription: &note,
	})
	if !strings.Contains(out, "https://go.dev/x") {
		t.Errorf("user prompt missing URL: %q", out)
	}
	if !strings.Contains(out, "感兴趣") {
		t.Errorf("user prompt missing note: %q", out)
	}
	if strings.Contains(out, "正文") {
		t.Errorf("url-direct user prompt should not include a body block: %q", out)
	}
}

func TestURLDirectBlocksSensitiveURLBeforeProviderCall(t *testing.T) {
	t.Parallel()

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
	}))
	defer server.Close()
	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL: server.URL, Model: "grok-test", HTTPClient: server.Client(), EmptyResponseRetries: 1,
	})
	sensitive := "https://alice:password@example.com/post?signature=query-secret#fragment-secret"
	_, err := a.Analyze(context.Background(), AnalyzeRequest{URLDirect: true, Content: fetcher.Content{URL: sensitive}})
	if err == nil {
		t.Fatal("Analyze() error = nil, want privacy-policy rejection")
	}
	if calls != 0 {
		t.Fatalf("provider calls=%d, want 0", calls)
	}
	for _, sentinel := range []string{"alice", "password", "signature", "query-secret", "fragment-secret"} {
		if strings.Contains(err.Error(), sentinel) {
			t.Fatalf("privacy error leaked %q: %v", sentinel, err)
		}
	}
}

// TestParseAccessibleFalseReturnsNotAccessible: a {"accessible":false}
// response is parsed into AnalysisResult{Accessible:false} with no error and
// without requiring summary/tags (so the pipeline can fall back).
func TestParseAccessibleFalseReturnsNotAccessible(t *testing.T) {
	t.Parallel()
	a := newURLDirectAnalyzer()
	got, err := a.parseAnalysisResponse(`{"accessible": false}`)
	if err != nil {
		t.Fatalf("parse error = %v, want nil", err)
	}
	if got.Accessible {
		t.Error("Accessible = true, want false")
	}
	if got.Summary != "" || len(got.Tags) != 0 {
		t.Errorf("expected empty summary/tags on inaccessible, got %+v", got)
	}
}

// TestParseAccessibleTrueCapturesTitle: an accessible url-direct response
// keeps Accessible=true and captures the model-supplied title.
func TestParseAccessibleTrueCapturesTitle(t *testing.T) {
	t.Parallel()
	a := newURLDirectAnalyzer()
	got, err := a.parseAnalysisResponse(`{"accessible":true,"title":"真实标题","summary":"- 要点","tags":["Go","错误处理"]}`)
	if err != nil {
		t.Fatalf("parse error = %v", err)
	}
	if !got.Accessible {
		t.Error("Accessible = false, want true")
	}
	if got.Title != "真实标题" {
		t.Errorf("Title = %q, want 真实标题", got.Title)
	}
	if got.Summary != "- 要点" || len(got.Tags) != 2 {
		t.Errorf("unexpected summary/tags: %+v", got)
	}
}

// TestParseNoAccessibleFieldDefaultsAccessible: the fetched-content path
// (no accessible field) must default Accessible=true so existing behaviour
// is unchanged.
func TestParseNoAccessibleFieldDefaultsAccessible(t *testing.T) {
	t.Parallel()
	a := newURLDirectAnalyzer()
	got, err := a.parseAnalysisResponse(`{"summary":"x","tags":["Go","AI"]}`)
	if err != nil {
		t.Fatalf("parse error = %v", err)
	}
	if !got.Accessible {
		t.Error("Accessible = false, want true (default when field absent)")
	}
}
