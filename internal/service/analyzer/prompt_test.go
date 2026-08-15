package analyzer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/summarypolicy"
)

// TestBuildRetrieveSelectPromptInjectsCandidates verifies the v3 prompt
// embeds the candidate list verbatim, keeps the JSON contract, and carries
// the "at most one new tag" + good/bad-tag guidance.
func TestBuildRetrieveSelectPromptInjectsCandidates(t *testing.T) {
	t.Parallel()

	candidates := []string{"RAG", "检索增强生成", "WeKnora"}
	got := buildRetrieveSelectPrompt(candidates, "article")

	for _, c := range candidates {
		if !strings.Contains(got, c) {
			t.Errorf("retrieve-select prompt missing candidate %q\nprompt:\n%s", c, got)
		}
	}
	if !strings.Contains(got, `"title"`) || !strings.Contains(got, `"summary"`) || !strings.Contains(got, `"tags"`) {
		t.Errorf("retrieve-select prompt dropped the JSON output contract:\n%s", got)
	}
	if !strings.Contains(got, "（优先从中原样选择）：") {
		t.Errorf("retrieve-select prompt missing the injected candidate block:\n%s", got)
	}
	if !strings.Contains(got, "至多") || !strings.Contains(got, "1 个") {
		t.Errorf("retrieve-select prompt missing the at-most-one-new-tag rule:\n%s", got)
	}
	// content_type hint for "article" must still be appended.
	if !strings.Contains(got, contentTypeHints["article"]) {
		t.Errorf("retrieve-select prompt missing article content-type hint:\n%s", got)
	}
}

// TestBuildRetrieveSelectPromptEmptyCandidatesNoBlock guards the
// degenerate call: an empty candidate slice must not emit an empty 候选标签
// block (the pipeline never calls it that way, but the function stays safe).
func TestBuildRetrieveSelectPromptEmptyCandidatesNoBlock(t *testing.T) {
	t.Parallel()

	got := buildRetrieveSelectPrompt(nil, "")
	// The base prompt text mentions 候选标签 in its instructions, so we key
	// on the injected-block marker (the "（优先从中原样选择）" suffix the
	// candidate list is prefixed with) rather than the bare word.
	if strings.Contains(got, "（优先从中原样选择）：") {
		t.Errorf("empty candidates should not emit a candidate block:\n%s", got)
	}
}

func TestSummaryPromptsDiscourageTemplateHeadings(t *testing.T) {
	t.Parallel()

	prompts := map[string]string{
		"default":         buildSystemPrompt(nil, "article"),
		"retrieve-select": buildRetrieveSelectPrompt([]string{"Go"}, "article"),
		"url-direct":      buildURLDirectPrompt(nil, "article"),
	}
	for name, prompt := range prompts {
		name, prompt := name, prompt
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if strings.Contains(prompt, "详细中文摘要") {
				t.Fatalf("%s prompt still asks for a verbose detailed summary:\n%s", name, prompt)
			}
			for _, want := range []string{"不要输出", "背景/问题", "明确结论/结果", "不要加粗"} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("%s prompt missing anti-template guidance %q:\n%s", name, want, prompt)
				}
			}
		})
	}
}

// TestAnalysisPromptsGuardAgainstInterstitialPages pins the interstitial
// guard onto every built-in prompt path. Fetchers only reject hard failures,
// so a Cloudflare challenge / login wall that returns a 200 with body text
// reaches the model as ordinary content; without this rule it invents tags
// for the interstitial, and those tags land in the shared concept vocabulary.
func TestAnalysisPromptsGuardAgainstInterstitialPages(t *testing.T) {
	t.Parallel()

	// URL-direct is deliberately absent: it has its own accessible=false
	// contract for these pages, covered by the test below.
	prompts := map[string]string{
		"default":         buildSystemPrompt(nil, "article"),
		"retrieve-select": buildRetrieveSelectPrompt([]string{"Go"}, "article"),
	}
	for name, prompt := range prompts {
		name, prompt := name, prompt
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// One representative marker per interstitial family, plus the
			// empty-array instruction and the clause that makes the guard
			// win over the "3-5 tags" / "2-4 tags" counts below it.
			for _, want := range []string{
				"Cloudflare", "CAPTCHA", "登录墙", "Cookie 同意", "404",
				"tags 必须输出空数组 []",
				"不得依据 URL、标题或站点名推断、补全或编造正文内容",
				"优先于下面所有关于标签数量的要求",
			} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("%s prompt missing interstitial guard %q:\n%s", name, want, prompt)
				}
			}
		})
	}
}

// TestURLDirectPromptKeepsAccessibleContractOverGuard pins the deliberate
// exclusion. Both rules fire on the same pages (404 / login wall / anti-bot)
// but demand opposite outputs, and only accessible=false triggers the local
// fetcher retry in runURLDirect — the guard's "describe it and return empty
// tags" would finalise a link titled "Just a moment..." instead.
func TestURLDirectPromptKeepsAccessibleContractOverGuard(t *testing.T) {
	t.Parallel()

	got := buildURLDirectPrompt(nil, "article")
	if !strings.Contains(got, `{"accessible": false}`) {
		t.Fatalf("url-direct prompt lost the accessible=false fallback contract:\n%s", got)
	}
	if strings.Contains(got, "tags 必须输出空数组 []") {
		t.Fatalf("url-direct prompt must not carry the interstitial guard — it conflicts with accessible=false:\n%s", got)
	}
}

// TestInterstitialGuardIsFieldAgnostic: the site (v2) contract has no
// summary field at all — it emits name / intro / purpose — so a guard that
// only constrains "summary" would leave those free to be invented.
func TestInterstitialGuardIsFieldAgnostic(t *testing.T) {
	t.Parallel()

	got := buildSystemPrompt(nil, "article")
	for _, want := range []string{"intro", "purpose", "以本次要求的输出格式为准"} {
		if !strings.Contains(got, want) {
			t.Fatalf("interstitial guard is not field-agnostic, missing %q:\n%s", want, got)
		}
	}
}

// TestLibraryPromptKeepsInterstitialGuard covers the site/library path,
// which appends libraryOutputContract to whichever base prompt was picked
// rather than replacing it — the guard must survive that append.
func TestLibraryPromptKeepsInterstitialGuard(t *testing.T) {
	t.Parallel()

	a := &OpenAIAnalyzer{}
	payload := a.buildAnalyzePayload(AnalyzeRequest{
		Content:              fetcher.Content{URL: "https://example.com", Title: "Example"},
		ContentType:          "article",
		RequestedLibraryKind: model.RequestedLibraryKindSite,
	})
	messages, ok := payload["messages"].([]map[string]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("payload has no messages: %#v", payload)
	}
	systemPrompt, _ := messages[0]["content"].(string)
	if !strings.Contains(systemPrompt, "tags 必须输出空数组 []") {
		t.Fatalf("library prompt dropped the interstitial guard:\n%s", systemPrompt)
	}
	if !strings.Contains(systemPrompt, "site_profile") {
		t.Fatalf("library prompt lost its own v2 output contract:\n%s", systemPrompt)
	}
}

func TestAnalysisPromptsRequestShortGeneratedTitle(t *testing.T) {
	t.Parallel()

	prompts := map[string]string{
		"default":         buildSystemPrompt(nil, "article"),
		"retrieve-select": buildRetrieveSelectPrompt([]string{"Go"}, "article"),
		"url-direct":      buildURLDirectPrompt(nil, "article"),
	}
	for name, prompt := range prompts {
		name, prompt := name, prompt
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, want := range []string{`"title"`, "短标题", "36", "不要直接复制"} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("%s prompt missing short-title guidance %q:\n%s", name, want, prompt)
				}
			}
		})
	}
}

func TestAnalysisPromptsPreserveFactualQualifiersInTitle(t *testing.T) {
	t.Parallel()

	prompts := map[string]string{
		"default":         buildSystemPrompt(nil, "article"),
		"retrieve-select": buildRetrieveSelectPrompt([]string{"Go"}, "article"),
		"url-direct":      buildURLDirectPrompt(nil, "article"),
	}
	for name, prompt := range prompts {
		name, prompt := name, prompt
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, want := range []string{"事实限定", "暂缓", "可预见的未来", "可能"} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("%s prompt missing title qualifier guidance %q:\n%s", name, want, prompt)
				}
			}
		})
	}
}

func TestAnalysisPromptsRequireCentralReusableTags(t *testing.T) {
	t.Parallel()

	prompts := map[string]string{
		"default":         buildSystemPrompt(nil, "article"),
		"retrieve-select": buildRetrieveSelectPrompt([]string{"Go"}, "article"),
		"url-direct":      buildURLDirectPrompt(nil, "article"),
	}
	for name, prompt := range prompts {
		name, prompt := name, prompt
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, want := range []string{"全文核心主题", "顺带提及", "作者名", "账号名", "同义标签"} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("%s prompt missing central-tag guidance %q:\n%s", name, want, prompt)
				}
			}
		})
	}
}

func TestArticlePromptRequiresConclusionAndCrossSectionRecommendations(t *testing.T) {
	t.Parallel()

	prompt := buildSystemPrompt(nil, "article")
	for _, want := range []string{"最终结论或决定", "可执行建议", "多个章节", "至少4项"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("article prompt missing coverage guidance %q:\n%s", want, prompt)
		}
	}
}

func TestAnalysisPromptsPreferNaturalChineseTechnicalTerms(t *testing.T) {
	t.Parallel()

	for name, prompt := range map[string]string{
		"default":    buildSystemPrompt(nil, "article"),
		"url-direct": buildURLDirectPrompt(nil, "article"),
	} {
		name, prompt := name, prompt
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, want := range []string{"通行中文术语", "idempotency → 幂等性", "cursor pagination → 游标分页"} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("%s prompt missing terminology rule %q:\n%s", name, want, prompt)
				}
			}
		})
	}
}

func TestAnalysisPromptsDoNotConflateExamplesWithAuthorship(t *testing.T) {
	t.Parallel()

	for name, prompt := range map[string]string{
		"default":         buildSystemPrompt(nil, "article"),
		"retrieve-select": buildRetrieveSelectPrompt([]string{"API"}, "article"),
		"url-direct":      buildURLDirectPrompt(nil, "article"),
	} {
		name, prompt := name, prompt
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, want := range []string{"示例产品", "作者参与构建", "职位升级"} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("%s prompt missing attribution rule %q:\n%s", name, want, prompt)
				}
			}
		})
	}
}

func TestListingPromptCoversLeadAndCategoryBreadth(t *testing.T) {
	t.Parallel()

	prompt := buildSystemPromptFor(nil, summarypolicy.Input{
		URL:         "https://github.com/ruanyf/weekly/blob/master/docs/issue-402.md",
		ContentType: "listing",
	})
	for _, want := range []string{"周刊", "开篇主内容", "至少2个其他栏目", "整个条目"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("listing prompt missing digest coverage guidance %q:\n%s", want, prompt)
		}
	}
}

func TestAnalyzeURLDirectInjectsExistingTagVocabulary(t *testing.T) {
	t.Parallel()

	systemPrompt := captureSystemPrompt(t, AnalyzeRequest{
		URLDirect:    true,
		ExistingTags: []string{"API", "接口设计", "向后兼容"},
		Content: fetcher.Content{
			URL: "https://example.com/article",
		},
		ContentType: "article",
	})

	for _, want := range []string{"已有标签库", "API", "接口设计", "向后兼容"} {
		if !strings.Contains(systemPrompt, want) {
			t.Fatalf("url-direct prompt missing existing tag vocabulary %q:\n%s", want, systemPrompt)
		}
	}
}

func TestAnalyzeUsesAdaptiveArticleSummaryPolicy(t *testing.T) {
	t.Parallel()

	systemPrompt := captureSystemPrompt(t, AnalyzeRequest{
		Content: fetcher.Content{
			URL:         "https://example.com/article",
			Title:       "Article",
			Body:        "Body",
			FetcherType: "basic",
		},
		ContentType: "article",
	})

	for _, want := range []string{"100-180字", "2-4个自然", "至少3个并列", "否则写成自然段"} {
		if !strings.Contains(systemPrompt, want) {
			t.Fatalf("article prompt missing %q:\n%s", want, systemPrompt)
		}
	}
	for _, forbidden := range []string{"180-360字", "最后自然写出", "必须给出明确"} {
		if strings.Contains(systemPrompt, forbidden) {
			t.Fatalf("article prompt still contains old forced-report rule %q:\n%s", forbidden, systemPrompt)
		}
	}
}

// captureSystemPrompt runs Analyze against a stub chat-completions server
// and returns the system message content the analyzer actually sent.
func captureSystemPrompt(t *testing.T, req AnalyzeRequest) string {
	t.Helper()

	var systemContent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		for _, m := range body.Messages {
			if m["role"] == "system" {
				if s, ok := m["content"].(string); ok {
					systemContent = s
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": `{"summary":"摘要","tags":["RAG"]}`}},
			},
		})
	}))
	t.Cleanup(server.Close)

	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:              server.URL,
		Model:                "gpt-test",
		HTTPClient:           server.Client(),
		EmptyResponseRetries: 1,
		MinTags:              1,
		MaxTags:              5,
	})
	if req.Content.URL == "" {
		req.Content = fetcher.Content{URL: "https://example.com", Title: "T", Body: "B"}
	}
	if _, err := a.Analyze(context.Background(), req); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return systemContent
}

// TestAnalyzeSelectsRetrievePromptWhenCandidatesPresent verifies Analyze's
// prompt-selection precedence by inspecting the system message actually
// sent over the wire: candidates → retrieve-select; override still wins
// over candidates; no candidates → free-generation.
func TestAnalyzeSelectsRetrievePromptWhenCandidatesPresent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		req        AnalyzeRequest
		wantSubstr string
		notSubstr  string
	}{
		{
			name:       "candidates use retrieve-select prompt",
			req:        AnalyzeRequest{ExistingTags: []string{"Go", "AI"}, Candidates: []string{"RAG", "WeKnora"}},
			wantSubstr: "候选标签",
		},
		{
			name:       "override beats candidates",
			req:        AnalyzeRequest{Candidates: []string{"RAG"}, SystemPromptOverride: "CUSTOM_EVAL_PROMPT"},
			wantSubstr: "CUSTOM_EVAL_PROMPT",
			notSubstr:  "候选标签",
		},
		{
			name:       "no candidates keeps free-generation",
			req:        AnalyzeRequest{ExistingTags: []string{"Go", "AI"}},
			wantSubstr: "已有标签库",
			notSubstr:  "候选标签",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			systemPrompt := captureSystemPrompt(t, tc.req)
			if tc.wantSubstr != "" && !strings.Contains(systemPrompt, tc.wantSubstr) {
				t.Errorf("prompt missing %q:\n%s", tc.wantSubstr, systemPrompt)
			}
			if tc.notSubstr != "" && strings.Contains(systemPrompt, tc.notSubstr) {
				t.Errorf("prompt should not contain %q:\n%s", tc.notSubstr, systemPrompt)
			}
		})
	}
}
