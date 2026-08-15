package analyzer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"webtag/internal/fetcher"
)

func TestOpenAIAnalyzerRetriesNeverDiscloseSensitiveSourceURL(t *testing.T) {
	t.Parallel()

	var requests [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		requests = append(requests, append([]byte(nil), body.Bytes()...))
		if len(requests) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": `{"summary":"Body content summary","tags":["Go"]}`}}},
		})
	}))
	defer server.Close()

	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL: server.URL, Model: "test", HTTPClient: server.Client(), EmptyResponseRetries: 2,
		RetryDelay: time.Millisecond, MinTags: 1, MaxTags: 5,
	})
	sensitive := "https://alice:password@example.com/post?signature=query-secret#fragment-secret"
	_, err := a.Analyze(context.Background(), AnalyzeRequest{Content: fetcher.Content{
		URL: sensitive, Title: "Title", Body: "Body content for provider analysis",
	}})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("provider requests=%d, want retry pair", len(requests))
	}
	for index, request := range requests {
		text := string(request)
		if !strings.Contains(text, "https://example.com/post") {
			t.Fatalf("request %d missing projected URL: %s", index, text)
		}
		for _, sentinel := range []string{"alice", "password", "signature", "query-secret", "fragment-secret"} {
			if strings.Contains(text, sentinel) {
				t.Fatalf("request %d leaked %q", index, sentinel)
			}
		}
	}
}

func TestOpenAIAnalyzerCallsChatCompletionsAndParsesFencedJSON(t *testing.T) {
	t.Parallel()

	var gotAuth string
	var gotPath string
	var gotModel string
	var gotMessages []map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path

		var body struct {
			Model    string           `json:"model"`
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}

		gotModel = body.Model
		gotMessages = body.Messages

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": "```json\n{\"summary\":\"精炼摘要\",\"tags\":[\" Go \",\"AI\"]}\n```",
					},
				},
			},
		})
	}))
	defer server.Close()

	description := "用户备注"
	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              server.URL,
		APIKey:               "secret-key",
		Model:                "gpt-test",
		HTTPClient:           server.Client(),
		BodyPreviewChars:     32,
		EmptyResponseRetries: 1,
		MaxSummaryChars:      50,
		MinTags:              1,
		MaxTags:              5,
		MaxTagChars:          12,
	})

	got, err := analyzer.Analyze(context.Background(), AnalyzeRequest{
		Content: fetcher.Content{
			URL:   "https://example.com/post",
			Title: "Example title",
			Body:  strings.Repeat("A", 64),
		},
		ExistingTags:    []string{"Go", "AI"},
		ContentType:     "article",
		UserDescription: &description,
	})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}

	if gotPath != "/chat/completions" {
		t.Fatalf("request path = %q, want /chat/completions", gotPath)
	}

	if gotAuth != "Bearer secret-key" {
		t.Fatalf("authorization = %q, want Bearer secret-key", gotAuth)
	}

	if gotModel != "gpt-test" {
		t.Fatalf("model = %q, want gpt-test", gotModel)
	}

	if len(gotMessages) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(gotMessages))
	}

	systemContent, ok := gotMessages[0]["content"].(string)
	if !ok {
		t.Fatalf("system content type = %T, want string", gotMessages[0]["content"])
	}
	if !strings.Contains(systemContent, "已有标签库") {
		t.Fatalf("system prompt = %q, want existing tag hint", systemContent)
	}

	userContent, ok := gotMessages[1]["content"].(string)
	if !ok {
		t.Fatalf("user content type = %T, want string", gotMessages[1]["content"])
	}
	if !strings.Contains(userContent, "用户备注: 用户备注") {
		t.Fatalf("user prompt = %q, want user description", userContent)
	}

	bodySection := strings.SplitN(userContent, "正文:\n", 2)
	if len(bodySection) != 2 || runeCount(bodySection[1]) != 32 {
		t.Fatalf("user prompt body preview should keep the 32-rune budget: %q", userContent)
	}
	if !strings.Contains(bodySection[1], "中间内容已省略") {
		t.Fatalf("user prompt body preview should preserve head and tail: %q", userContent)
	}

	if got.Summary != "精炼摘要" {
		t.Fatalf("summary = %q, want 精炼摘要", got.Summary)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "Go" || got.Tags[1] != "AI" {
		t.Fatalf("tags = %#v, want [Go AI]", got.Tags)
	}
}

func TestOpenAIAnalyzerSamplesBodyAcrossBeginningMiddleAndEndingWithinBudget(t *testing.T) {
	t.Parallel()

	var userPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		userPrompt, _ = body.Messages[1]["content"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"摘要\",\"tags\":[\"Go\"]}"}}]}`))
	}))
	t.Cleanup(server.Close)

	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              server.URL,
		Model:                "grok-test",
		HTTPClient:           server.Client(),
		BodyPreviewChars:     96,
		EmptyResponseRetries: 1,
		MinTags:              1,
	})
	body := "HEAD-" + strings.Repeat("A", 60) + "-MIDDLE-ACTIONABLE-" + strings.Repeat("B", 60) + "-TAIL"
	if _, err := a.Analyze(context.Background(), AnalyzeRequest{
		Content: fetcher.Content{URL: "https://example.com/long", Title: "Long", Body: body},
	}); err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}

	for _, want := range []string{"HEAD-", "MIDDLE-ACTIONABLE", "-TAIL", "中间内容已省略"} {
		if !strings.Contains(userPrompt, want) {
			t.Fatalf("user prompt missing %q: %s", want, userPrompt)
		}
	}
	bodySection := strings.SplitN(userPrompt, "正文:\n", 2)
	if len(bodySection) != 2 || runeCount(bodySection[1]) != 96 {
		t.Fatalf("sampled body should use the exact 96-rune budget: %q", userPrompt)
	}
}

func TestSampleBodyPrioritizesExplicitConclusionBeforePageChrome(t *testing.T) {
	t.Parallel()

	body := "Opening\n\n" + strings.Repeat("intro ", 80) +
		"\n\nMiddle\n\n" + strings.Repeat("detail ", 80) +
		"\n\nSummary\n\nUse API keys, idempotency, rate limits, cursor pagination, and optional fields.\n\n" +
		strings.Repeat("related post footer ", 100)
	got := sampleBody(body, 240)

	for _, want := range []string{"Opening", "Summary", "cursor pagination", "optional fields"} {
		if !strings.Contains(got, want) {
			t.Fatalf("sampled body missing %q:\n%s", want, got)
		}
	}
	if runeCount(got) != 240 {
		t.Fatalf("sampled body length = %d, want 240", runeCount(got))
	}
}

func TestBuildUserPromptIncludesDocumentSectionOutline(t *testing.T) {
	t.Parallel()

	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{BodyPreviewChars: 4000})
	body := "Everything about APIs\n\nOpening paragraph.\n\nAuthentication\n\nAdvice.\n\n" +
		"Idempotency and retries\n\nAdvice.\n\nSafety and rate limiting\n\nAdvice.\n\n" +
		"Pagination\n\nAdvice.\n\nSummary\n\nFinal recommendations."
	prompt := a.buildUserPrompt(AnalyzeRequest{Content: fetcher.Content{
		URL:   "https://example.com/api",
		Title: "API article",
		Body:  body,
	}})

	for _, want := range []string{"文档结构概览", "Authentication", "Idempotency and retries", "Safety and rate limiting", "Pagination", "Summary"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("user prompt missing outline entry %q:\n%s", want, prompt)
		}
	}
}

func TestBuildUserPromptAddsDigestCoverageDirective(t *testing.T) {
	t.Parallel()

	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{BodyPreviewChars: 4000})
	body := "科技周刊第402期\n\n主文内容。\n\n科技动态\n\n1、驯化浣熊\n\n新闻内容。\n\n文章\n\n1、如何设计良好的 API\n\n文章内容。\n\n工具\n\n1、Deno Desktop\n\n工具内容。\n\nAI相关\n\n1、编程代理\n\n项目内容。"
	prompt := a.buildUserPrompt(AnalyzeRequest{
		Content: fetcher.Content{
			URL:  "https://github.com/example/weekly/blob/main/docs/issue-402.md",
			Body: body,
		},
		ContentType: "listing",
	})

	for _, want := range []string{
		"完整周刊", "第一句", "第二句", "此外，本期还", "至少2个其他栏目",
		"章节代表片段", "[科技动态] 1、驯化浣熊", "[文章] 1、如何设计良好的 API", "[工具] 1、Deno Desktop",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("digest user prompt missing directive %q:\n%s", want, prompt)
		}
	}
}

func TestBuildUserPromptBalancesLongDigestLeadAgainstLaterSections(t *testing.T) {
	t.Parallel()

	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{BodyPreviewChars: 12000})
	body := "周刊标题\n\n主文\n\nLEAD-START " + strings.Repeat("主文很长 ", 1000) + " LEAD-END\n\n" +
		"科技动态\n\n1、驯化浣熊\n\n工具\n\n1、Deno Desktop\n\n资源\n\n1、测试教程"
	prompt := a.buildUserPrompt(AnalyzeRequest{
		Content: fetcher.Content{
			URL:  "https://github.com/example/weekly/blob/main/docs/issue-402.md",
			Body: body,
		},
		ContentType: "listing",
	})

	bodySection := strings.SplitN(prompt, "正文:\n", 2)
	if len(bodySection) != 2 {
		t.Fatalf("user prompt missing body section:\n%s", prompt)
	}
	if !strings.Contains(bodySection[1], "LEAD-START") {
		t.Fatalf("balanced digest body dropped the lead opening:\n%s", bodySection[1])
	}
	if strings.Contains(bodySection[1], "LEAD-END") {
		t.Fatalf("balanced digest body retained the entire dominant lead:\n%s", bodySection[1])
	}
	if !strings.Contains(prompt, "[科技动态] 1、驯化浣熊") || !strings.Contains(prompt, "[工具] 1、Deno Desktop") {
		t.Fatalf("balanced digest prompt dropped later-section previews:\n%s", prompt)
	}
}

func TestEnsureDigestCoverageAddsFactualLaterSectionsWithinLimit(t *testing.T) {
	t.Parallel()

	summary := "本期周刊重点分享超短篇小说《我在智念AI的日子》，通过AI生成代码、幻灯片和零审查等场景讽刺职场对AI的过度依赖，展现员工逐渐失去独立思考能力的过程。"
	body := "周刊标题\n\n主文\n\n小说内容。\n\n科技动态\n\n1、驯化浣熊\n\n文章\n\n1、如何设计良好的 API\n\n工具\n\n1、Deno Desktop\n\n资源\n\n1、测试教程"
	got := ensureDigestCoverage(summary, body, 160)

	for _, want := range []string{"超短篇小说", "此外", "科技动态", "驯化浣熊", "文章", "API", "工具", "Deno Desktop"} {
		if !strings.Contains(got, want) {
			t.Fatalf("digest coverage fallback missing %q: %s", want, got)
		}
	}
	if runeCount(got) > 160 {
		t.Fatalf("digest coverage length = %d, want <= 160: %s", runeCount(got), got)
	}
	if strings.Contains(got, "…") {
		t.Fatalf("digest coverage should cut the lead at a natural clause: %s", got)
	}
	if strings.Contains(got, "。 此外") {
		t.Fatalf("digest coverage inserted an ASCII gap between Chinese sentences: %s", got)
	}
}

func TestEnsureDigestCoverageKeepsAlreadyRepresentativeSummary(t *testing.T) {
	t.Parallel()

	summary := "主文讨论AI职场依赖。此外，本期还介绍科技动态、开发工具和学习资源。"
	body := "周刊\n\n主文。\n\n科技动态\n\n新闻。\n\n开发工具\n\n项目。\n\n学习资源\n\n教程。"
	if got := ensureDigestCoverage(summary, body, 160); got != summary {
		t.Fatalf("representative digest summary changed: %q", got)
	}
}

func TestEnsureDigestCoverageFindsCategoriesAfterLongLead(t *testing.T) {
	t.Parallel()

	var body strings.Builder
	for i := 0; i < maxOutlineHeadings+10; i++ {
		body.WriteString("Lead section ")
		body.WriteString(strconv.Itoa(i))
		body.WriteString("\n\nLead detail.\n\n")
	}
	body.WriteString("科技动态\n\n1、驯化浣熊\n\n文章\n\n1、API设计\n\n工具\n\n1、Deno Desktop")

	got := ensureDigestCoverage("主文是一篇AI超短篇小说。", body.String(), 160)
	for _, want := range []string{"科技动态", "驯化浣熊", "文章", "API设计", "工具", "Deno Desktop"} {
		if !strings.Contains(got, want) {
			t.Fatalf("late digest category missing %q: %s", want, got)
		}
	}
}

func TestCanonicalizeDigestTagsUsesExplicitSourceEvidence(t *testing.T) {
	t.Parallel()

	body := "本期主文是一篇超短篇小说。\n\n工具\n\n1、Deno Desktop\n\n科技动态\n\n1、新闻"
	got := canonicalizeDigestTags([]string{"周刊", "AI", "小说", "工具", "科技动态"}, body)
	want := []string{"科技周刊", "AI", "短篇小说", "开发工具", "科技动态"}
	if len(got) != len(want) {
		t.Fatalf("canonical digest tags = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("canonical digest tags[%d] = %q, want %q; all=%#v", i, got[i], want[i], got)
		}
	}
}

func TestCanonicalizeDigestTagsDoesNotInventUnsupportedSpecificity(t *testing.T) {
	t.Parallel()

	got := canonicalizeDigestTags([]string{"小说", "工具"}, "这是一部长篇小说，没有工具栏目。")
	if len(got) != 2 || got[0] != "小说" || got[1] != "工具" {
		t.Fatalf("unsupported canonicalization changed tags: %#v", got)
	}
}

func TestDefaultBodyPreviewBudgetSupportsLongArticles(t *testing.T) {
	t.Parallel()

	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{})
	if a.bodyPreviewChars < 12000 {
		t.Fatalf("default body preview = %d, want at least 12000", a.bodyPreviewChars)
	}
}

func TestAnalyzeClampsProductionSummaryToAdaptiveProfileLimit(t *testing.T) {
	t.Parallel()

	longSummary := strings.Repeat("这是一句包含关键事实并且能够独立阅读的完整摘要。", 12)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"content": `{"summary":` + strconv.Quote(longSummary) + `,"tags":["API"]}`,
				},
			}},
		})
	}))
	t.Cleanup(server.Close)

	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              server.URL,
		APIKey:               "test",
		Model:                "grok-test",
		HTTPClient:           server.Client(),
		EmptyResponseRetries: 1,
		MaxSummaryChars:      240,
	})
	got, err := a.Analyze(context.Background(), AnalyzeRequest{
		Content:     fetcher.Content{URL: "https://example.com/article", Body: "source"},
		ContentType: "article",
	})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if runeCount(got.Summary) > 180 {
		t.Fatalf("article summary length = %d, want <= 180: %q", runeCount(got.Summary), got.Summary)
	}
	if !strings.HasSuffix(got.Summary, "。") {
		t.Fatalf("article summary should end at a complete sentence: %q", got.Summary)
	}
}

func TestAnalyzeRemovesReportTemplatePhrasesFromProductionSummary(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"背景/问题：现有 API 难以维护。核心结论是优先保持向后兼容。\",\"tags\":[\"API\"]}"}}]}`))
	}))
	t.Cleanup(server.Close)

	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL: server.URL, APIKey: "test", Model: "grok-test",
		HTTPClient: server.Client(), EmptyResponseRetries: 1,
	})
	got, err := a.Analyze(context.Background(), AnalyzeRequest{
		Content: fetcher.Content{URL: "https://example.com/article", Body: "source"},
	})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	for _, forbidden := range []string{"背景/问题", "核心结论"} {
		if strings.Contains(got.Summary, forbidden) {
			t.Fatalf("summary retained report template %q: %q", forbidden, got.Summary)
		}
	}
}

func TestAnalyzeAppliesEvidenceBackedSummaryCanonicalization(t *testing.T) {
	t.Parallel()

	rawSummary := "公共API需严格避免破用户空间的改变，只能添加字段而不能移除现有结构。"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"content": `{"summary":` + strconv.Quote(rawSummary) + `,"tags":["API"]}`,
				},
			}},
		})
	}))
	t.Cleanup(server.Close)

	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL: server.URL, APIKey: "test", Model: "grok-test",
		HTTPClient: server.Client(), EmptyResponseRetries: 1,
	})
	got, err := a.Analyze(context.Background(), AnalyzeRequest{
		Content: fetcher.Content{
			Title: "Everything I know about good API design",
			Body:  "WE DO NOT BREAK USERSPACE. Additive fields preserve compatibility.",
		},
		ContentType: "article",
	})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	want := "公共API需严格避免破坏用户代码的变更，只能添加字段而不能移除现有结构。"
	if got.Summary != want {
		t.Fatalf("Analyze() summary = %q, want %q", got.Summary, want)
	}
}

func TestAnalyzeConformsHomepageBulletsToProse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"- 提供团队知识库。\\n- 支持全文检索。\\n- 面向研发团队。\",\"tags\":[\"知识库\"]}"}}]}`))
	}))
	t.Cleanup(server.Close)

	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL: server.URL, APIKey: "test", Model: "grok-test",
		HTTPClient: server.Client(), EmptyResponseRetries: 1,
	})
	got, err := a.Analyze(context.Background(), AnalyzeRequest{
		Content:     fetcher.Content{URL: "https://example.com/", Body: "source"},
		ContentType: "homepage",
	})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if strings.Contains(got.Summary, "\n") || strings.Contains(got.Summary, "- ") {
		t.Fatalf("homepage summary retained bullet formatting: %q", got.Summary)
	}
}

func TestOpenAIAnalyzerSendsMultimodalUserMessageWhenImagesPresent(t *testing.T) {
	t.Parallel()

	var gotMessages []map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}

		gotMessages = body.Messages

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": "{\"summary\":\"图文摘要\",\"tags\":[\"Vision\",\"Go\"]}",
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
		BodyPreviewChars:     24,
		EmptyResponseRetries: 1,
		MaxSummaryChars:      50,
		MinTags:              1,
		MaxTags:              5,
		MaxTagChars:          12,
	})

	got, err := analyzer.Analyze(context.Background(), AnalyzeRequest{
		Content: fetcher.Content{
			URL:   "https://example.com/post",
			Title: "Example title",
			Body:  "Body with context",
			ImageURLs: []string{
				testVisionPNGDataURL,
				testVisionPNGDataURL,
				testVisionPNGDataURL,
				testVisionPNGDataURL,
				testVisionPNGDataURL,
			},
		},
	})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}

	if len(gotMessages) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(gotMessages))
	}

	contentParts, ok := gotMessages[1]["content"].([]any)
	if !ok {
		t.Fatalf("user content type = %T, want []any", gotMessages[1]["content"])
	}
	if len(contentParts) != 4 {
		t.Fatalf("len(user content parts) = %d, want text + 3 bounded images", len(contentParts))
	}

	firstPart, ok := contentParts[0].(map[string]any)
	if !ok {
		t.Fatalf("first user content part type = %T, want map[string]any", contentParts[0])
	}
	if firstPart["type"] != "text" {
		t.Fatalf("first part type = %#v, want text", firstPart["type"])
	}

	textValue, ok := firstPart["text"].(string)
	if !ok {
		t.Fatalf("first part text type = %T, want string", firstPart["text"])
	}
	if !strings.Contains(textValue, "Body with context") {
		t.Fatalf("text part = %q, want analyzer prompt body", textValue)
	}

	for i, wantURL := range []string{testVisionPNGDataURL, testVisionPNGDataURL, testVisionPNGDataURL} {
		part, ok := contentParts[i+1].(map[string]any)
		if !ok {
			t.Fatalf("image part %d type = %T, want map[string]any", i, contentParts[i+1])
		}
		if part["type"] != "image_url" {
			t.Fatalf("image part %d type = %#v, want image_url", i, part["type"])
		}
		imageURL, ok := part["image_url"].(map[string]any)
		if !ok {
			t.Fatalf("image part %d image_url type = %T, want map[string]any", i, part["image_url"])
		}
		if imageURL["url"] != wantURL {
			t.Fatalf("image part %d url = %#v, want %q", i, imageURL["url"], wantURL)
		}
		if imageURL["detail"] != "auto" {
			t.Fatalf("image part %d detail = %#v, want auto", i, imageURL["detail"])
		}
	}

	if got.Summary != "图文摘要" {
		t.Fatalf("summary = %q, want 图文摘要", got.Summary)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "Vision" || got.Tags[1] != "Go" {
		t.Fatalf("tags = %#v, want [Vision Go]", got.Tags)
	}
}

func TestOpenAIAnalyzerBuildUserPromptIncludesSearchSummarySeparately(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              "https://example.com",
		APIKey:               "secret-key",
		Model:                "gpt-test",
		BodyPreviewChars:     16,
		EmptyResponseRetries: 1,
		MaxSummaryChars:      50,
		MinTags:              1,
		MaxTags:              5,
		MaxTagChars:          12,
	})

	prompt := analyzer.buildUserPrompt(AnalyzeRequest{
		Content: fetcher.Content{
			URL:   "https://example.com/post",
			Title: "Example title",
			Body:  strings.Repeat("A", 64),
			Metadata: map[string]any{
				"search_summary": "1. Search hit one\n2. Search hit two",
			},
		},
	})

	if !strings.Contains(prompt, "正文:\n"+strings.Repeat("A", 16)) {
		t.Fatalf("prompt = %q, want truncated body section", prompt)
	}
	if !strings.Contains(prompt, "辅助搜索上下文:") {
		t.Fatalf("prompt = %q, want separate search context section", prompt)
	}
	if !strings.Contains(prompt, "Search hit two") {
		t.Fatalf("prompt = %q, want preserved search summary", prompt)
	}
}

func TestOpenAIAnalyzerBuildUserPromptUsesNormalizedBestTitle(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              "https://example.com",
		APIKey:               "secret-key",
		Model:                "gpt-test",
		BodyPreviewChars:     16,
		EmptyResponseRetries: 1,
		MaxSummaryChars:      50,
		MinTags:              1,
		MaxTags:              5,
		MaxTagChars:          12,
	})

	prompt := analyzer.buildUserPrompt(AnalyzeRequest{
		Content: fetcher.Content{
			URL:   "https://example.com/post",
			Title: "Home",
			Body:  "content",
			Metadata: map[string]any{
				"best_title": "Recovered Title",
			},
		},
	})

	if !strings.Contains(prompt, "标题: Recovered Title") {
		t.Fatalf("prompt = %q, want normalized best title before analyzer call", prompt)
	}
	if strings.Contains(prompt, "标题: Home") {
		t.Fatalf("prompt = %q, should not keep generic original title when better title exists", prompt)
	}
}
