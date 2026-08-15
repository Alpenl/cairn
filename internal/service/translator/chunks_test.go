package translator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOpenAITranslatorChunksLongTextAndPreservesOrder(t *testing.T) {
	t.Parallel()

	source := strings.Join([]string{
		"The first paragraph contains enough words to require translation.",
		"The second paragraph must remain after the first paragraph.",
		"The third paragraph proves that the complete source is processed.",
	}, "\n\n")
	inputs := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		input := payload.Messages[1].Content
		inputs = append(inputs, input)
		index := len(inputs)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": fmt.Sprintf("第%d块完整中文译文", index)},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	client := NewOpenAITranslator(Options{
		BaseURL: server.URL, Model: "grok-test", HTTPClient: server.Client(), MaxChunkChars: 48,
	})
	result, err := client.Translate(context.Background(), Request{Text: source, Format: FormatPlain})
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if len(inputs) < 3 {
		t.Fatalf("model calls = %d, want at least 3 chunks", len(inputs))
	}
	for i, input := range inputs {
		if utf8.RuneCountInString(input) > 48 {
			t.Fatalf("chunk %d length = %d, want <= 48: %q", i, utf8.RuneCountInString(input), input)
		}
		marker := fmt.Sprintf("第%d块完整中文译文", i+1)
		position := strings.Index(result.Text, marker)
		if position < 0 {
			t.Fatalf("result missing %q: %q", marker, result.Text)
		}
		if i > 0 {
			previous := fmt.Sprintf("第%d块完整中文译文", i)
			if strings.Index(result.Text, previous) > position {
				t.Fatalf("result order is wrong: %q", result.Text)
			}
		}
	}
}

func TestDefaultChunkBudgetSplitsNearMaximumFullArticle(t *testing.T) {
	t.Parallel()

	paragraph := "This paragraph represents one part of a long saved article that must be translated completely.\n\n"
	source := strings.Repeat(paragraph, 1_100)
	inputs := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		inputs = append(inputs, payload.Messages[1].Content)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": fmt.Sprintf("长文第%d块完整译文", len(inputs))},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	client := NewOpenAITranslator(Options{BaseURL: server.URL, Model: "grok-test", HTTPClient: server.Client()})
	result, err := client.Translate(context.Background(), Request{Text: source, Format: FormatMarkdown})
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if len(inputs) < 8 {
		t.Fatalf("model calls = %d, want at least 8 default-sized chunks", len(inputs))
	}
	for i, input := range inputs {
		if utf8.RuneCountInString(input) > defaultMaxChunkChars {
			t.Fatalf("chunk %d length = %d, want <= %d", i, utf8.RuneCountInString(input), defaultMaxChunkChars)
		}
	}
	lastMarker := fmt.Sprintf("长文第%d块完整译文", len(inputs))
	if !strings.Contains(result.Text, lastMarker) {
		t.Fatalf("result does not contain final chunk marker %q", lastMarker)
	}
}

func TestDefaultChunkBudgetHasProvableTwentyChunkUpperBound(t *testing.T) {
	t.Parallel()

	// A natural boundary just past half the chunk budget is the smallest cut
	// splitAtNaturalBoundaries can choose. Protected spans are found before
	// chunking, so placeholder expansion cannot increase this count.
	segment := strings.Repeat("a", defaultMaxChunkChars/2) + "\n"
	source := strings.Repeat(segment, 19)
	source += strings.Repeat("b", defaultMaxInputChars-utf8.RuneCountInString(source))
	parts := splitTranslationParts(source, defaultMaxChunkChars)
	if len(parts) > 20 {
		t.Fatalf("parts = %d, want at most 20", len(parts))
	}

	protectedSource := strings.Repeat("word `x` ", defaultMaxInputChars/9+1)
	protectedSource = string([]rune(protectedSource)[:defaultMaxInputChars])
	protectedParts := splitTranslationParts(protectedSource, defaultMaxChunkChars)
	if len(protectedParts) > 20 {
		t.Fatalf("protected parts = %d, want at most 20", len(protectedParts))
	}
}

func TestOpenAITranslatorRejectsTokenTruncation(t *testing.T) {
	t.Parallel()

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": "这是一段被截断的中文译文"},
				"finish_reason": "length",
			}},
		})
	}))
	defer server.Close()

	client := NewOpenAITranslator(Options{BaseURL: server.URL, Model: "grok-test", HTTPClient: server.Client()})
	_, err := client.Translate(context.Background(), Request{
		Text: "This complete paragraph must never be stored partially.", Format: FormatPlain,
	})
	if err == nil || !strings.Contains(err.Error(), "truncated output") {
		t.Fatalf("Translate() error = %v, want truncation failure", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestSimplifyNaturalLanguageUsesOpenCCAndProtectsCodeAndURL(t *testing.T) {
	t.Parallel()

	source := "醫院保存著作權與檔案，乾乾淨淨。\n\n```txt\n醫院檔案\n```\n\n`醫院` https://example.com/醫院 [檔案](./醫院/檔案.md)"
	converted, err := simplifyNaturalLanguage(source)
	if err != nil {
		t.Fatalf("simplifyNaturalLanguage() error = %v", err)
	}
	if !strings.Contains(converted, "医院保存著作权与档案，干干净净。") {
		t.Fatalf("natural language was not converted: %q", converted)
	}
	for _, protected := range []string{"```txt\n醫院檔案\n```", "`醫院`", "https://example.com/醫院", "[档案](./醫院/檔案.md)"} {
		if !strings.Contains(converted, protected) {
			t.Fatalf("protected text %q changed: %q", protected, converted)
		}
	}
}

func TestOpenAITranslatorProtectsInlineContentBeforeModelCall(t *testing.T) {
	t.Parallel()

	const source = "Run ``go test ./...`` then visit https://example.com/raw?q=醫院 and read [the docs](https://docs.example.com/a_(b)/檔案)."
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		input := payload.Messages[1].Content
		for _, protected := range []string{"go test ./...", "https://example.com/raw", "https://docs.example.com"} {
			if strings.Contains(input, protected) {
				t.Fatalf("model received protected source %q in %q", protected, input)
			}
		}
		placeholders := modelPlaceholderPattern.FindAllString(input, -1)
		if len(placeholders) != 3 {
			t.Fatalf("placeholders = %q, want 3", placeholders)
		}
		content := "占位符被模型删除"
		if calls == 2 {
			content = "运行" + placeholders[0] + "，然后访问" + placeholders[1] + "并阅读[文档" + placeholders[2] + "。"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": content}, "finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	client := NewOpenAITranslator(Options{BaseURL: server.URL, Model: "grok-test", HTTPClient: server.Client()})
	result, err := client.Translate(context.Background(), Request{Text: source, Format: FormatMarkdown})
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want corrective retry", calls)
	}
	for _, protected := range []string{"``go test ./...``", "https://example.com/raw?q=醫院", "](https://docs.example.com/a_(b)/檔案)"} {
		if !strings.Contains(result.Text, protected) {
			t.Fatalf("result lost protected content %q: %q", protected, result.Text)
		}
	}
}

func TestOpenAITranslatorNeverSplitsProtectedPlaceholder(t *testing.T) {
	t.Parallel()

	const sourceURL = "https://very-long-domain.example/documentation/reference/path?q=醫院"
	source := "See " + sourceURL + " before continuing with the article."
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		input := payload.Messages[1].Content
		if strings.Contains(input, sourceURL) {
			t.Fatalf("model received raw URL: %q", input)
		}
		placeholders := modelPlaceholderPattern.FindAllString(input, -1)
		if strings.Contains(input, modelPlaceholderPrefix) && len(placeholders) == 0 {
			t.Fatalf("chunk split a protected placeholder: %q", input)
		}
		content := "中文译文"
		if len(placeholders) == 1 {
			content += placeholders[0]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
	}))
	defer server.Close()

	client := NewOpenAITranslator(Options{
		BaseURL: server.URL, Model: "grok-test", HTTPClient: server.Client(), MaxChunkChars: 20,
	})
	result, err := client.Translate(context.Background(), Request{Text: source, Format: FormatPlain})
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if calls < 2 || !strings.Contains(result.Text, sourceURL) {
		t.Fatalf("calls=%d result=%q", calls, result.Text)
	}
}

func TestOpenAITranslatorProtectsBalancedParenthesesInBareURL(t *testing.T) {
	t.Parallel()

	const sourceURL = "https://en.wikipedia.org/wiki/Function_(mathematics)"
	const source = "Read " + sourceURL + " for the formal definition."
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		input := payload.Messages[1].Content
		if strings.Contains(input, sourceURL) || strings.Contains(input, "(mathematics)") {
			t.Fatalf("model received part of bare URL: %q", input)
		}
		placeholders := modelPlaceholderPattern.FindAllString(input, -1)
		if len(placeholders) != 1 {
			t.Fatalf("placeholders = %q, want 1", placeholders)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": "请阅读" + placeholders[0] + "了解正式定义。"},
			}},
		})
	}))
	defer server.Close()

	client := NewOpenAITranslator(Options{BaseURL: server.URL, Model: "grok-test", HTTPClient: server.Client()})
	result, err := client.Translate(context.Background(), Request{Text: source, Format: FormatPlain})
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if calls != 1 || !strings.Contains(result.Text, sourceURL) {
		t.Fatalf("calls=%d result=%q", calls, result.Text)
	}
}

func TestOpenAITranslatorProtectsApostropheInBareURL(t *testing.T) {
	t.Parallel()

	const sourceURL = "https://en.wikipedia.org/wiki/Moore's_law"
	const source = "Read " + sourceURL + " for the historical observation."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		input := payload.Messages[1].Content
		if strings.Contains(input, sourceURL) || strings.Contains(input, "'s_law") {
			t.Fatalf("model received part of apostrophe URL: %q", input)
		}
		placeholders := modelPlaceholderPattern.FindAllString(input, -1)
		if len(placeholders) != 1 {
			t.Fatalf("placeholders = %q, want 1", placeholders)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": "请阅读" + placeholders[0] + "了解这一历史观察。"},
			}},
		})
	}))
	defer server.Close()

	client := NewOpenAITranslator(Options{BaseURL: server.URL, Model: "grok-test", HTTPClient: server.Client()})
	result, err := client.Translate(context.Background(), Request{Text: source, Format: FormatPlain})
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if !strings.Contains(result.Text, sourceURL) {
		t.Fatalf("result lost apostrophe URL: %q", result.Text)
	}
}

func TestRawURLScannerExcludesOuterSingleQuotes(t *testing.T) {
	t.Parallel()

	const text = "Read 'https://example.com/wiki/Moore's_law'. Next."
	ranges := rawURLRanges(text)
	if len(ranges) != 1 {
		t.Fatalf("ranges = %+v, want one", ranges)
	}
	if got := text[ranges[0].start:ranges[0].end]; got != "https://example.com/wiki/Moore's_law" {
		t.Fatalf("protected URL = %q", got)
	}
}

func TestOpenAITranslatorReportsLaterChunkFailure(t *testing.T) {
	t.Parallel()

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		finishReason := "stop"
		if calls == 2 {
			finishReason = "length"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": "完整中文译文"}, "finish_reason": finishReason,
			}},
		})
	}))
	defer server.Close()

	client := NewOpenAITranslator(Options{
		BaseURL: server.URL, Model: "grok-test", HTTPClient: server.Client(), MaxChunkChars: 32,
	})
	_, err := client.Translate(context.Background(), Request{
		Text: strings.Repeat("This sentence needs translation. ", 4), Format: FormatPlain,
	})
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("Translate() error = %v, want truncation", err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}

func TestOpenAITranslatorNormalizesTraditionalModelOutput(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": "醫院保存著作權檔案。"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	client := NewOpenAITranslator(Options{BaseURL: server.URL, Model: "grok-test", HTTPClient: server.Client()})
	result, err := client.Translate(context.Background(), Request{
		Text: "The hospital stores the copyright file.", Format: FormatPlain,
	})
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if result.Text != "医院保存著作权档案。" {
		t.Fatalf("result = %q, want Simplified Chinese", result.Text)
	}
}
