package analyzer

import (
	"strings"
	"testing"

	"webtag/internal/fetcher"
)

func TestParseAnalysisResponseRejectsMissingSummary(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		APIKey:          "secret-key",
		Model:           "gpt-test",
		MaxSummaryChars: 10,
		MinTags:         2,
		MaxTags:         4,
		MaxTagChars:     5,
	})

	_, err := analyzer.parseAnalysisResponse(`{"summary":"","tags":["Go","AI"]}`)
	if err == nil {
		t.Fatal("parseAnalysisResponse() error = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "summary") {
		t.Fatalf("error = %q, want summary validation error", err.Error())
	}
}

func TestOpenAIAnalyzerBuildUserPromptTruncatesBodyByRunes(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:          "https://example.com",
		APIKey:           "secret-key",
		Model:            "gpt-test",
		BodyPreviewChars: 4,
	})

	prompt := analyzer.buildUserPrompt(AnalyzeRequest{
		Content: fetcher.Content{
			URL:   "https://example.com/post",
			Title: "标题",
			Body:  "你好世界再见",
		},
	})

	if !strings.Contains(prompt, "正文:\n你好世界") {
		t.Fatalf("prompt = %q, want rune-safe preview", prompt)
	}
	if strings.Contains(prompt, "正文:\n你") && !strings.Contains(prompt, "正文:\n你好世界") {
		t.Fatalf("prompt = %q, appears to be byte-truncated", prompt)
	}
}

func TestParseAnalysisResponseAcceptsChineseLengthsByRunes(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		APIKey:          "secret-key",
		Model:           "gpt-test",
		MaxSummaryChars: 4,
		MinTags:         1,
		MaxTags:         3,
		MaxTagChars:     2,
	})

	got, err := analyzer.parseAnalysisResponse(`{"summary":"你好世界","tags":["机器"]}`)
	if err != nil {
		t.Fatalf("parseAnalysisResponse() error = %v, want success", err)
	}
	if got.Summary != "你好世界" {
		t.Fatalf("summary = %q, want 你好世界", got.Summary)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "机器" {
		t.Fatalf("tags = %#v, want [机器]", got.Tags)
	}
}

func TestParseAnalysisResponseKeepsSummaryWhenTagCountIsBelowTarget(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		APIKey:          "secret-key",
		Model:           "gpt-test",
		MaxSummaryChars: 20,
		MinTags:         3,
		MaxTags:         5,
		MaxTagChars:     10,
	})

	got, err := analyzer.parseAnalysisResponse(`{"summary":"摘要已经可用","tags":["Go","AI"]}`)
	if err != nil {
		t.Fatalf("parseAnalysisResponse() error = %v, want partial success", err)
	}
	if got.Summary != "摘要已经可用" {
		t.Fatalf("summary = %q, want preserved summary", got.Summary)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "Go" || got.Tags[1] != "AI" {
		t.Fatalf("tags = %#v, want the two valid tags", got.Tags)
	}
}

func TestParseAnalysisResponseKeepsSummaryWhenTagsAreMissing(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		APIKey:          "secret-key",
		Model:           "gpt-test",
		MaxSummaryChars: 20,
		MinTags:         3,
		MaxTags:         5,
		MaxTagChars:     10,
	})

	got, err := analyzer.parseAnalysisResponse(`{"summary":"只有摘要也有价值"}`)
	if err != nil {
		t.Fatalf("parseAnalysisResponse() error = %v, want summary-only success", err)
	}
	if got.Summary != "只有摘要也有价值" || len(got.Tags) != 0 {
		t.Fatalf("result = %#v, want summary with no tags", got)
	}
}

func TestParseAnalysisResponseDropsMalformedTagsIndividually(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		APIKey:          "secret-key",
		Model:           "gpt-test",
		MaxSummaryChars: 20,
		MinTags:         1,
		MaxTags:         5,
		MaxTagChars:     4,
	})

	got, err := analyzer.parseAnalysisResponse(`{"summary":"保留可用标签","tags":["Go",42,"这是一个过长标签","AI"]}`)
	if err != nil {
		t.Fatalf("parseAnalysisResponse() error = %v, want best-effort tags", err)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "Go" || got.Tags[1] != "AI" {
		t.Fatalf("tags = %#v, want [Go AI]", got.Tags)
	}
}

func TestParseAnalysisResponseTruncatesExcessTags(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		APIKey:          "secret-key",
		Model:           "gpt-test",
		MaxSummaryChars: 20,
		MinTags:         1,
		MaxTags:         3,
		MaxTagChars:     10,
	})

	got, err := analyzer.parseAnalysisResponse(`{"summary":"标签有稳定上限","tags":["Go","AI","Postgres","River","Queue"]}`)
	if err != nil {
		t.Fatalf("parseAnalysisResponse() error = %v, want bounded success", err)
	}
	want := []string{"Go", "AI", "Postgres"}
	if len(got.Tags) != len(want) {
		t.Fatalf("tags = %#v, want %#v", got.Tags, want)
	}
	for i := range want {
		if got.Tags[i] != want[i] {
			t.Fatalf("tags = %#v, want %#v", got.Tags, want)
		}
	}
}

func TestParseAnalysisResponseClampsOverlongSummaryByRunes(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		APIKey:          "secret-key",
		Model:           "gpt-test",
		MaxSummaryChars: 8,
		MinTags:         1,
		MaxTags:         3,
		MaxTagChars:     8,
	})

	got, err := analyzer.parseAnalysisResponse(`{"summary":"这段摘要明显超过限制并且没有其他候选","tags":["Go"]}`)
	if err != nil {
		t.Fatalf("parseAnalysisResponse() error = %v, want success", err)
	}
	if got.Summary != "这段摘要明显超…" {
		t.Fatalf("summary = %q, want rune-safe clamped summary", got.Summary)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "Go" {
		t.Fatalf("tags = %#v, want [Go]", got.Tags)
	}
}

func TestParseAnalysisResponseClampsOverlongSummaryAtSentenceBoundary(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		Model:           "grok-test",
		MaxSummaryChars: 20,
		MinTags:         1,
		MaxTags:         3,
		MaxTagChars:     8,
	})

	got, err := analyzer.parseAnalysisResponse(`{"summary":"第一句完整结束。第二句也很有价值。第三句会超过限制而被截断。","tags":["Go"]}`)
	if err != nil {
		t.Fatalf("parseAnalysisResponse() error = %v, want success", err)
	}
	want := "第一句完整结束。第二句也很有价值。"
	if got.Summary != want {
		t.Fatalf("summary = %q, want sentence-safe %q", got.Summary, want)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "Go" {
		t.Fatalf("tags = %#v, want [Go]", got.Tags)
	}
}

func TestDefaultAnalyzerBoundsSummaryToReadableHeadroom(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL: "https://example.com",
		Model:   "grok-test",
		MinTags: 1,
	})
	longSummary := strings.Repeat("这是一个完整句子。", 40)
	raw := `{"summary":"` + longSummary + `","tags":["Go"]}`

	got, err := analyzer.parseAnalysisResponse(raw)
	if err != nil {
		t.Fatalf("parseAnalysisResponse() error = %v, want success", err)
	}
	if runeCount(got.Summary) > 240 {
		t.Fatalf("default summary length = %d, want <= 240", runeCount(got.Summary))
	}
	if !strings.HasSuffix(got.Summary, "。") {
		t.Fatalf("default summary should end at a complete sentence: %q", got.Summary)
	}
}

func TestParseAnalysisResponseParsesPrettyPrintedJSONEmbeddedInText(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		APIKey:          "secret-key",
		Model:           "gpt-test",
		MaxSummaryChars: 20,
		MinTags:         1,
		MaxTags:         5,
		MaxTagChars:     10,
	})

	raw := "下面是最终结果：\n{\n  \"summary\": \"结构化输出\",\n  \"tags\": [\"Go\", \"Parser\"]\n}\n谢谢。"
	got, err := analyzer.parseAnalysisResponse(raw)
	if err != nil {
		t.Fatalf("parseAnalysisResponse() error = %v, want success", err)
	}
	if got.Summary != "结构化输出" {
		t.Fatalf("summary = %q, want 结构化输出", got.Summary)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "Go" || got.Tags[1] != "Parser" {
		t.Fatalf("tags = %#v, want [Go Parser]", got.Tags)
	}
}

func TestParseAnalysisResponseSkipsInvalidJSONCandidatesAndFindsValidObject(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		APIKey:          "secret-key",
		Model:           "gpt-test",
		MaxSummaryChars: 4,
		MinTags:         1,
		MaxTags:         5,
		MaxTagChars:     10,
	})

	raw := "示例：{\"summary\":\"太长太长太长\",\"tags\":[\"Go\"]}\n最终：{\"summary\":\"可用结果\",\"tags\":[\"Go\",\"JSON\"]}"
	got, err := analyzer.parseAnalysisResponse(raw)
	if err != nil {
		t.Fatalf("parseAnalysisResponse() error = %v, want success", err)
	}
	if got.Summary != "可用结果" {
		t.Fatalf("summary = %q, want 可用结果", got.Summary)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "Go" || got.Tags[1] != "JSON" {
		t.Fatalf("tags = %#v, want [Go JSON]", got.Tags)
	}
}

func TestParseAnalysisResponseDeduplicatesTagsCaseInsensitively(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		APIKey:          "secret-key",
		Model:           "gpt-test",
		MaxSummaryChars: 20,
		MinTags:         2,
		MaxTags:         5,
		MaxTagChars:     20,
	})

	got, err := analyzer.parseAnalysisResponse(`{"summary":"标签去重","tags":["Go","go"," AI ","AI","Parser"]}`)
	if err != nil {
		t.Fatalf("parseAnalysisResponse() error = %v, want success", err)
	}
	if len(got.Tags) != 3 {
		t.Fatalf("len(tags) = %d, want 3 unique tags", len(got.Tags))
	}
	if got.Tags[0] != "Go" || got.Tags[1] != "AI" || got.Tags[2] != "Parser" {
		t.Fatalf("tags = %#v, want [Go AI Parser]", got.Tags)
	}
}

func TestParseAnalysisResponseRegressionCorpus(t *testing.T) {
	t.Parallel()

	cases := loadAnalyzerRegressionCorpus(t)
	for _, tt := range cases {
		tt := tt
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
				BaseURL:         "https://example.com",
				APIKey:          "secret-key",
				Model:           "gpt-test",
				MaxSummaryChars: tt.MaxSummaryChars,
				MinTags:         tt.MinTags,
				MaxTags:         tt.MaxTags,
				MaxTagChars:     tt.MaxTagChars,
			})

			got, err := analyzer.parseAnalysisResponse(tt.Raw)
			if tt.WantErrorContains != "" {
				if err == nil {
					t.Fatal("parseAnalysisResponse() error = nil, want failure")
				}
				if !strings.Contains(err.Error(), tt.WantErrorContains) {
					t.Fatalf("error = %q, want substring %q", err.Error(), tt.WantErrorContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAnalysisResponse() error = %v, want success", err)
			}
			if got.Summary != tt.WantSummary {
				t.Fatalf("summary = %q, want %q", got.Summary, tt.WantSummary)
			}
			if len(got.Tags) != len(tt.WantTags) {
				t.Fatalf("len(tags) = %d, want %d", len(got.Tags), len(tt.WantTags))
			}
			for i := range tt.WantTags {
				if got.Tags[i] != tt.WantTags[i] {
					t.Fatalf("tags[%d] = %q, want %q", i, got.Tags[i], tt.WantTags[i])
				}
			}
		})
	}
}
