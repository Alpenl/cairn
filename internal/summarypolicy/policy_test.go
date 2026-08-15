package summarypolicy

import (
	"slices"
	"strings"
	"testing"
)

func TestForHomepageKeepsSummaryCompactAndProseOnly(t *testing.T) {
	t.Parallel()

	profile := For(Input{ContentType: "homepage"})

	if profile.MinRunes != 40 || profile.MaxRunes != 90 {
		t.Fatalf("homepage length = %d-%d, want 40-90", profile.MinRunes, profile.MaxRunes)
	}
	if profile.AllowBullets {
		t.Fatal("homepage profile should not allow bullet lists")
	}
	for _, want := range []string{"1-2", "自然", "不要使用项目符号"} {
		if !strings.Contains(profile.PromptInstructions(), want) {
			t.Fatalf("homepage instructions missing %q: %s", want, profile.PromptInstructions())
		}
	}
}

func TestForGitHubURLUsesProjectProfile(t *testing.T) {
	t.Parallel()

	profile := For(Input{
		URL:         "https://github.com/openai/openai-python",
		ContentType: "unknown",
	})

	if profile.Name != "project" {
		t.Fatalf("profile name = %q, want project", profile.Name)
	}
	if profile.MinRunes != 60 || profile.MaxRunes != 120 {
		t.Fatalf("project length = %d-%d, want 60-120", profile.MinRunes, profile.MaxRunes)
	}
	if profile.AllowBullets {
		t.Fatal("project profile should be concise prose, not a feature list")
	}
	if !strings.Contains(profile.PromptInstructions(), "解决什么问题") {
		t.Fatalf("project instructions should focus on user value: %s", profile.PromptInstructions())
	}
}

func TestForArticleAllowsOnlyConditionalShortBullets(t *testing.T) {
	t.Parallel()

	profile := For(Input{ContentType: "article"})

	if profile.Name != "article" {
		t.Fatalf("profile name = %q, want article", profile.Name)
	}
	if profile.MinRunes != 100 || profile.MaxRunes != 180 {
		t.Fatalf("article length = %d-%d, want 100-180", profile.MinRunes, profile.MaxRunes)
	}
	if profile.MinSentences != 2 || profile.MaxSentences != 4 {
		t.Fatalf("article sentences = %d-%d, want 2-4", profile.MinSentences, profile.MaxSentences)
	}
	if !profile.AllowBullets || profile.MaxBullets != 3 {
		t.Fatalf("article bullet policy = allow:%v max:%d, want allow:true max:3", profile.AllowBullets, profile.MaxBullets)
	}
	for _, want := range []string{"至少3个", "最多3个", "否则写成自然段"} {
		if !strings.Contains(profile.PromptInstructions(), want) {
			t.Fatalf("article instructions missing %q: %s", want, profile.PromptInstructions())
		}
	}
}

func TestGitHubDocumentURLDoesNotUseProjectProfile(t *testing.T) {
	t.Parallel()

	profile := For(Input{
		URL:         "https://github.com/ruanyf/weekly/blob/master/docs/issue-402.md",
		ContentType: "unknown",
	})

	if profile.Name == "project" {
		t.Fatalf("GitHub document URL incorrectly classified as project: %+v", profile)
	}
}

func TestForGitHubWeeklyIssueUsesDigestProfile(t *testing.T) {
	t.Parallel()

	profile := For(Input{
		URL:         "https://github.com/ruanyf/weekly/blob/master/docs/issue-402.md",
		ContentType: "listing",
	})

	if profile.Name != "digest" {
		t.Fatalf("profile name = %q, want digest", profile.Name)
	}
	if profile.MinRunes != 90 || profile.MaxRunes != 160 {
		t.Fatalf("digest length = %d-%d, want 90-160", profile.MinRunes, profile.MaxRunes)
	}
	if profile.MinSentences != 2 || profile.MaxSentences != 3 {
		t.Fatalf("digest sentences = %d-%d, want 2-3", profile.MinSentences, profile.MaxSentences)
	}
	for _, want := range []string{"周刊", "开篇主内容", "至少2个其他栏目", "整个条目"} {
		if !strings.Contains(profile.PromptInstructions(), want) {
			t.Fatalf("digest instructions missing %q: %s", want, profile.PromptInstructions())
		}
	}
}

func TestForListingDescribesScopeWithoutInventingAConclusion(t *testing.T) {
	t.Parallel()

	profile := For(Input{ContentType: "listing"})

	if profile.Name != "listing" || profile.MinRunes != 40 || profile.MaxRunes != 90 {
		t.Fatalf("listing profile = %+v, want listing 40-90", profile)
	}
	if profile.AllowBullets {
		t.Fatal("listing profile should use a short prose description")
	}
	if !strings.Contains(profile.PromptInstructions(), "内容范围") {
		t.Fatalf("listing instructions should describe scope: %s", profile.PromptInstructions())
	}
}

func TestForSocialPostAllowsOneConciseSentence(t *testing.T) {
	t.Parallel()

	profile := For(Input{URL: "https://x.com/GitHub_Daily/status/2073708506319098344"})
	if profile.Name != "social" || profile.MinRunes != 50 || profile.MaxRunes != 120 {
		t.Fatalf("social profile = %+v, want social 50-120", profile)
	}
	if profile.MinSentences != 1 || profile.MaxSentences != 2 || profile.AllowBullets {
		t.Fatalf("social shape = %+v, want 1-2 prose sentences", profile)
	}
	if !strings.Contains(profile.Focus, "短帖") {
		t.Fatalf("social focus should avoid article-style expansion: %q", profile.Focus)
	}
}

func TestForGeneralAllowsOneSubstantiveSentence(t *testing.T) {
	t.Parallel()

	profile := For(Input{URL: "https://example.com/unknown-page"})
	if profile.Name != "general" || profile.MinSentences != 1 || profile.MaxSentences != 3 {
		t.Fatalf("general profile = %+v, want 1-3 sentences", profile)
	}
}

func TestClampEndsAtCompleteSentence(t *testing.T) {
	t.Parallel()

	input := "第一句完整结束。第二句也很有价值。第三句会超过限制而被截断。"
	got := Clamp(input, 20)
	want := "第一句完整结束。第二句也很有价值。"
	if got != want {
		t.Fatalf("Clamp() = %q, want %q", got, want)
	}
}

func TestClampNormalizesSemicolonBoundaryToSentenceEnding(t *testing.T) {
	t.Parallel()

	input := "第一句提供必要背景。第二句表达完整观点；后面的细节会超过长度限制并被省略。"
	got := Clamp(input, 24)
	if !strings.HasSuffix(got, "。") {
		t.Fatalf("Clamp() should turn a semicolon cutoff into a complete sentence: %q", got)
	}
	if strings.HasSuffix(got, "；") || strings.HasSuffix(got, ";") {
		t.Fatalf("Clamp() left a dangling semicolon: %q", got)
	}
}

func TestClampDoesNotTreatCodeOperatorAsSentenceEnding(t *testing.T) {
	t.Parallel()

	input := "第一句说明提案背景。第二句讨论 if err != nil 的现有写法并给出完整判断。第三句会超过限制。"
	got := Clamp(input, 46)
	if strings.HasSuffix(got, "!") || strings.HasSuffix(got, "if err !") {
		t.Fatalf("Clamp() mistook != for sentence punctuation: %q", got)
	}
	if !strings.HasSuffix(got, "。") {
		t.Fatalf("Clamp() should retain a complete sentence: %q", got)
	}
}

func TestClampDoesNotTreatQuestionMarkOperatorAsSentenceEnding(t *testing.T) {
	t.Parallel()

	input := "文章说明Go团队决定不再为错误处理添加语法糖，并交代决定形成的背景。" +
		"随后回顾多个提案，包括check/handle、try内置函数及?操作符，并解释这些方案为何没有进入语言。" +
		"最后一段继续补充提案讨论中的更多历史细节，因此会超过摘要长度限制。"
	got := Clamp(input, 80)
	if strings.HasSuffix(got, "?") || strings.HasSuffix(got, "及?") {
		t.Fatalf("Clamp() mistook ? operator for sentence punctuation: %q", got)
	}
	if !strings.HasSuffix(got, "。") {
		t.Fatalf("Clamp() should retain a complete sentence or clause: %q", got)
	}
}

func TestClampStillTreatsASCIIQuestionAsSentenceEnding(t *testing.T) {
	t.Parallel()

	input := "第一句说明背景。这个方案是否可行? 后面的补充说明会超过限制。"
	got := Clamp(input, 24)
	if got != "第一句说明背景。这个方案是否可行?" {
		t.Fatalf("Clamp() = %q, want complete ASCII question", got)
	}
}

func TestClampPrefersEarlierCompleteSentenceOverPartialEllipsis(t *testing.T) {
	t.Parallel()

	input := "这是一句可独立阅读的介绍。第二句包含很多补充细节而且会一直延伸到长度限制之外无法完整保留。"
	got := Clamp(input, 34)
	if got != "这是一句可独立阅读的介绍。" {
		t.Fatalf("Clamp() = %q, want earlier complete sentence", got)
	}
}

func TestClampUsesLateChineseClauseBoundaryBeforeEarlyShortSentence(t *testing.T) {
	t.Parallel()

	input := "这是项目简介。该工具支持选股、实时监控和策略回测，内置十八个策略，还能根据用户想法生成自定义策略，并继续补充很多会超过限制的信息。"
	got := Clamp(input, 55)
	if strings.HasSuffix(got, "…") || strings.HasSuffix(got, "，") {
		t.Fatalf("Clamp() should finish a late parallel clause cleanly: %q", got)
	}
	if !strings.HasSuffix(got, "。") || got == "这是项目简介。" {
		t.Fatalf("Clamp() should preserve useful later clauses and end with a period: %q", got)
	}
}

func TestCleanRemovesReportTemplateFormattingWithoutDroppingFacts(t *testing.T) {
	t.Parallel()

	input := "## **背景/问题**：现有接口难以维护。\n**关键过程**：作者比较了多种方案。核心结论是保持兼容性。"
	got := Clean(input)
	for _, forbidden := range []string{"##", "**", "背景/问题", "关键过程", "核心结论"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Clean() retained %q: %q", forbidden, got)
		}
	}
	for _, fact := range []string{"现有接口难以维护", "作者比较了多种方案", "保持兼容性"} {
		if !strings.Contains(got, fact) {
			t.Fatalf("Clean() dropped fact %q: %q", fact, got)
		}
	}
}

func TestConformTurnsForbiddenBulletsIntoProse(t *testing.T) {
	t.Parallel()

	profile := For(Input{ContentType: "homepage"})
	input := "- 提供团队知识库。\n- 支持全文检索。\n- 面向研发团队。"
	got := Conform(input, profile)
	if strings.Contains(got, "\n") || strings.Contains(got, "- ") {
		t.Fatalf("Conform() retained prose-forbidden list formatting: %q", got)
	}
	for _, fact := range []string{"提供团队知识库", "支持全文检索", "面向研发团队"} {
		if !strings.Contains(got, fact) {
			t.Fatalf("Conform() dropped fact %q: %q", fact, got)
		}
	}
}

func TestAssessFlagsProductionReportTemplate(t *testing.T) {
	t.Parallel()

	profile := For(Input{ContentType: "article"})
	text := "背景/问题：用户需要一个工具。\n- 第一项功能。\n- 第二项功能。\n\n**明确的结论/结果**：这是一个值得使用的解决方案。" + strings.Repeat("补充说明。", 30)
	got := Assess(text, profile)

	if got.Score > 0.25 {
		t.Fatalf("template report score = %.2f, want <= 0.25; assessment=%+v", got.Score, got)
	}
	for _, issue := range []string{"too_long", "template_heading", "markdown_emphasis"} {
		if !slices.Contains(got.Issues, issue) {
			t.Fatalf("assessment missing %q: %+v", issue, got)
		}
	}
}

func TestAssessAcceptsNaturalArticleSummary(t *testing.T) {
	t.Parallel()

	text := "文章比较 REST 与 GraphQL API 的长期维护成本，指出熟悉、可预测的接口比炫技式抽象更容易被团队采用。" +
		"作者建议默认保持向后兼容，只在无法避免破坏性变化时引入版本，并通过幂等键让调用方安全重试。" +
		"最终选择应服务产品价值和真实用户，而不是追求形式上的纯粹。"
	got := Assess(text, For(Input{ContentType: "article"}))

	if got.Score != 1 || len(got.Issues) != 0 {
		t.Fatalf("natural article assessment = %+v, want perfect score", got)
	}
}

func TestAssessDoesNotCountBangOperatorAsSentence(t *testing.T) {
	t.Parallel()

	text := "Go 团队决定暂缓错误处理语法变更。现有 if err != nil 写法虽然冗长，但工具可以辅助。" +
		"团队会关闭相关提案，并把精力投入其他改进。"
	profile := Profile{MinRunes: 1, MaxRunes: 200, MinSentences: 2, MaxSentences: 3, AllowBullets: false}
	got := Assess(text, profile)
	if got.Score != 1 || len(got.Issues) != 0 {
		t.Fatalf("bang operator distorted sentence assessment: %+v", got)
	}
}
