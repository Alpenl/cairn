package translator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAITranslatorTranslatesMarkdownToSimplifiedChinese(t *testing.T) {
	t.Parallel()

	var payload struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		content := "# 标题\n\n这是正文。"
		if placeholders := modelPlaceholderPattern.FindAllString(payload.Messages[1].Content, -1); len(placeholders) == 1 {
			content += "\n\n" + placeholders[0]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{
					"content": content,
				},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	client := NewOpenAITranslator(Options{
		BaseURL:        server.URL,
		APIKey:         "test-key",
		Model:          "grok-4.3-fast",
		HTTPClient:     server.Client(),
		RequestTimeout: time.Second,
	})
	source := "# Heading\n\nThis is the body.\n\n```go\nfmt.Println(\"ok\")\n```"
	result, err := client.Translate(context.Background(), Request{
		Text:   source,
		Format: FormatMarkdown,
	})
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	wantTranslation := "# 标题\n\n这是正文。\n\n```go\nfmt.Println(\"ok\")\n```"
	if result.Text != wantTranslation {
		t.Fatalf("translated text = %q, want %q", result.Text, wantTranslation)
	}
	if result.Model != "grok-4.3-fast" {
		t.Fatalf("result metadata = %+v", result)
	}
	if payload.Model != "grok-4.3-fast" || len(payload.Messages) != 2 {
		t.Fatalf("request payload = %+v", payload)
	}
	system := payload.Messages[0].Content
	for _, want := range []string{"简体中文", "保留 Markdown", "不可信"} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing %q: %q", want, system)
		}
	}
	modelInput := payload.Messages[1].Content
	if payload.Messages[1].Role != "user" || strings.Contains(modelInput, "fmt.Println") ||
		len(modelPlaceholderPattern.FindAllString(modelInput, -1)) != 1 {
		t.Fatalf("user message = %+v, want code-free source with one placeholder", payload.Messages[1])
	}
}

func TestOpenAITranslatorRejectsOversizedSourceBeforeCallingUpstream(t *testing.T) {
	t.Parallel()

	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer server.Close()

	client := NewOpenAITranslator(Options{
		BaseURL:       server.URL,
		Model:         "grok-test",
		HTTPClient:    server.Client(),
		MaxInputChars: 4,
	})
	_, err := client.Translate(context.Background(), Request{Text: "12345", Format: FormatPlain})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("Translate() error = %v, want too long", err)
	}
	if called {
		t.Fatal("upstream was called for oversized input")
	}
}

func TestOpenAITranslatorRetriesUntranslatedNaturalLanguage(t *testing.T) {
	t.Parallel()

	const source = "This article verifies that selected passages can be translated."
	calls := 0
	var systems []string
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
		systems = append(systems, payload.Messages[0].Content)
		content := source
		if calls == 2 {
			content = "这篇文章验证了所选段落可以被翻译。"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
	}))
	defer server.Close()

	client := NewOpenAITranslator(Options{
		BaseURL: server.URL, Model: "grok-test", HTTPClient: server.Client(),
	})
	result, err := client.Translate(context.Background(), Request{Text: source, Format: FormatPlain})
	if err != nil {
		t.Fatalf("Translate() error = %v", err)
	}
	if calls != 2 || result.Text != "这篇文章验证了所选段落可以被翻译。" {
		t.Fatalf("calls=%d result=%+v", calls, result)
	}
	if len(systems) != 2 || !strings.Contains(systems[1], "上一次输出未通过简体中文或结构校验") {
		t.Fatalf("corrective prompts = %q", systems)
	}
}

func TestOpenAITranslatorRejectsRepeatedUntranslatedNaturalLanguage(t *testing.T) {
	t.Parallel()

	const source = "Persistent translation should survive a page reload."
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": source}}},
		})
	}))
	defer server.Close()
	client := NewOpenAITranslator(Options{
		BaseURL: server.URL, Model: "grok-test", HTTPClient: server.Client(),
	})

	_, err := client.Translate(context.Background(), Request{Text: source, Format: FormatPlain})
	if err == nil || !strings.Contains(err.Error(), "Simplified Chinese") {
		t.Fatalf("Translate() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestOpenAITranslatorAcceptsAlreadyChineseAndCodeWithoutRetry(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		source    string
		wantCalls int
	}{
		{source: "这段文字已经是中文。", wantCalls: 1},
		{source: `fmt.Println("translated")`, wantCalls: 1},
		{source: "https://example.com/docs?id=42", wantCalls: 0},
	} {
		test := test
		t.Run(test.source, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []any{map[string]any{"message": map[string]any{"content": test.source}}},
				})
			}))
			defer server.Close()
			client := NewOpenAITranslator(Options{
				BaseURL: server.URL, Model: "grok-test", HTTPClient: server.Client(),
			})

			result, err := client.Translate(context.Background(), Request{Text: test.source, Format: FormatPlain})
			if err != nil || result.Text != test.source || calls != test.wantCalls {
				t.Fatalf("Translate() = %+v, %v; calls=%d", result, err, calls)
			}
		})
	}
}

func TestValidSimplifiedChineseOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source string
		output string
		want   bool
	}{
		{name: "short English unchanged", source: "Hello", output: "Hello", want: false},
		{name: "English punctuation unchanged", source: "This (short) sentence", output: "This (short) sentence", want: false},
		{name: "English translated", source: "Hello", output: "你好", want: true},
		{name: "Japanese unchanged", source: "東京で働いています", output: "東京で働いています", want: false},
		{name: "Japanese residue", source: "東京で働いています", output: "我在東京で工作", want: false},
		{name: "Japanese translated", source: "東京で働いています", output: "我在东京工作", want: true},
		{name: "Korean residue", source: "서울에서 일합니다", output: "我在서울工作", want: false},
		{name: "Cyrillic residue", source: "Работа", output: "这是Работа", want: false},
		{name: "Arabic residue", source: "مرحبا", output: "中文مرحبا", want: false},
		{name: "Thai residue", source: "สวัสดี", output: "问候สวัสดี", want: false},
		{name: "Devanagari residue", source: "नमस्ते", output: "问候नमस्ते", want: false},
		{name: "Greek residue", source: "Καλημέρα", output: "早上好Καλημέρα", want: false},
		{name: "Hebrew residue", source: "שלום", output: "你好שלום", want: false},
		{name: "Traditional unchanged", source: "這是一篇關於軟體學習的文章", output: "這是一篇關於軟體學習的文章", want: false},
		{name: "Traditional hospital unchanged", source: "醫院", output: "醫院", want: false},
		{name: "Traditional copyright unchanged", source: "著作權", output: "著作權", want: false},
		{name: "Traditional file unchanged", source: "檔案", output: "檔案", want: false},
		{name: "Traditional converted", source: "這是一篇關於軟體學習的文章", output: "这是一篇关于软件学习的文章", want: true},
		{name: "Simplified unchanged", source: "这是一篇关于软件学习的文章", output: "这是一篇关于软件学习的文章", want: true},
		{name: "code unchanged", source: `fmt.Println("translated")`, output: `fmt.Println("translated")`, want: true},
		{name: "identifier unchanged", source: "translation_id", output: "translation_id", want: true},
		{name: "URL unchanged", source: "https://example.com/docs?id=42", output: "https://example.com/docs?id=42", want: true},
		{name: "code replaced by English", source: `fmt.Println("translated")`, output: "Not translated", want: false},
		{name: "Chinese with long URL", source: "Read the documentation URL and continue.", output: "请阅读 https://very-long-domain.example/documentation/reference 并继续。", want: true},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := validSimplifiedChineseOutput(test.source, test.output); got != test.want {
				t.Fatalf("validSimplifiedChineseOutput(%q, %q) = %v, want %v", test.source, test.output, got, test.want)
			}
		})
	}
}

func TestDefaultJobTimeoutCoversAllCorrectiveChunkCalls(t *testing.T) {
	t.Parallel()

	if got := DefaultJobTimeout(time.Minute); got != 41*time.Minute {
		t.Fatalf("DefaultJobTimeout(1m) = %s, want 41m", got)
	}
}

func TestOpenAITranslatorDefaultClientBlocksLoopback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("default hardened client reached loopback server")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewOpenAITranslator(Options{BaseURL: server.URL, Model: "grok-test"})
	_, err := client.Translate(context.Background(), Request{Text: "Hello", Format: FormatPlain})
	if err == nil {
		t.Fatal("Translate() error = nil, want unsafe-target rejection")
	}
}
