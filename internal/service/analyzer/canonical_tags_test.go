package analyzer

import (
	"strings"
	"testing"

	"webtag/internal/fetcher"
)

func TestCanonicalizeEvidenceBackedTagsForReviewedDomains(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content fetcher.Content
		tags    []string
		want    []string
	}{
		{
			name: "A股 concepts",
			content: fetcher.Content{
				URL:  "https://x.com/example/status/1",
				Body: "自托管 A 股量化工作台，支持选股策略和回测。",
			},
			tags: []string{"A股", "量化", "选股策略", "回测工具", "自托管"},
			want: []string{"A股", "量化交易", "选股", "回测", "自托管"},
		},
		{
			name: "API concepts",
			content: fetcher.Content{
				URL:   "https://example.com/good-api-design",
				Title: "Good API design",
				Body:  "API maintainers must not break userspace. Idempotency and pagination matter.",
			},
			tags: []string{"API", "API设计", "idempotency", "pagination", "用户空间"},
			want: []string{"API", "接口设计", "向后兼容", "幂等性", "分页"},
		},
		{
			name: "Go proposal concepts",
			content: fetcher.Content{
				URL:   "https://go.dev/blog/error-syntax",
				Title: "Error handling syntax",
				Body:  "Go error handling proposals did not reach consensus.",
			},
			tags: []string{"Go", "错误处理", "语法", "语言设计", "提案", "社区反馈"},
			want: []string{"Go", "错误处理", "语法设计", "Go提案", "社区共识"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := canonicalizeEvidenceBackedTags(tt.tags, tt.content)
			if len(got) != len(tt.want) {
				t.Fatalf("tags = %#v, want %#v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("tags[%d] = %q, want %q; all=%#v", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}

func TestCanonicalizeEvidenceBackedTagsKeepsAmbiguousTermsWithoutContext(t *testing.T) {
	t.Parallel()

	tags := []string{"量化", "提案", "用户空间"}
	got := canonicalizeEvidenceBackedTags(tags, fetcher.Content{URL: "https://example.com/general"})
	for i := range tags {
		if got[i] != tags[i] {
			t.Fatalf("ambiguous tags changed without evidence: %#v", got)
		}
	}
}

func TestCanonicalizeEvidenceBackedTagsPrioritizesCentralSourceConcepts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content fetcher.Content
		tags    []string
		want    []string
	}{
		{
			name: "API drops incidental protocols",
			content: fetcher.Content{
				Title: "Everything I know about good API design",
				Body:  "WE DO NOT BREAK USERSPACE. API versioning is a last resort. Use an idempotency key and cursor pagination.",
			},
			tags: []string{"API", "设计", "幂等性", "接口设计", "REST"},
			want: []string{"API", "接口设计", "向后兼容", "版本管理", "幂等性"},
		},
		{
			name: "Go drops incidental API",
			content: fetcher.Content{
				URL:  "https://go.dev/blog/error-syntax",
				Body: "Go error handling syntax proposals did not reach consensus.",
			},
			tags: []string{"Go", "错误处理", "Go提案", "语法设计", "API"},
			want: []string{"Go", "错误处理", "语法设计", "Go提案", "社区共识"},
		},
		{
			name: "digest represents complete issue",
			content: fetcher.Content{
				URL:  "https://github.com/example/weekly/blob/main/docs/issue-402.md",
				Body: "科技爱好者周刊\n\n这是一篇AI超短篇小说。\n\n科技动态\n\n新闻\n\n工具\n\n项目",
			},
			tags: []string{"科技周刊", "AI", "短篇小说", "工作文化", "编程"},
			want: []string{"科技周刊", "AI", "短篇小说", "科技动态", "开发工具"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := canonicalizeEvidenceBackedTags(tt.tags, tt.content)
			if len(got) != len(tt.want) {
				t.Fatalf("tags = %#v, want %#v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("tags[%d] = %q, want %q; all=%#v", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}

func TestCanonicalizeEvidenceBackedTitlePreservesQualifiedDecision(t *testing.T) {
	t.Parallel()

	goContent := fetcher.Content{
		URL:  "https://go.dev/blog/error-syntax",
		Body: "Go error handling syntax proposals did not reach consensus.",
	}
	if got := canonicalizeEvidenceBackedTitle(
		"Go 不添加错误处理语法支持",
		"Go 团队决定暂缓添加专用语法。",
		goContent,
	); got != "Go暂缓错误处理语法变更" {
		t.Fatalf("qualified Go title = %q", got)
	}

	apiContent := fetcher.Content{
		Title: "Everything I know about good API design",
		Body:  "WE DO NOT BREAK USERSPACE. API versioning should be a last resort.",
	}
	if got := canonicalizeEvidenceBackedTitle(
		"API设计黄金法则：不改接口不变化",
		"API 应保持稳定并避免破坏用户。",
		apiContent,
	); got != "API设计：稳定、兼容与简单" {
		t.Fatalf("canonical API title = %q", got)
	}

	plain := "A normal generated title"
	if got := canonicalizeEvidenceBackedTitle(plain, "summary", fetcher.Content{}); got != plain {
		t.Fatalf("unrelated title changed: %q", got)
	}
}

func TestCanonicalizeEvidenceBackedTitleRepresentsCompleteDigest(t *testing.T) {
	t.Parallel()

	content := fetcher.Content{
		URL:  "https://github.com/ruanyf/weekly/blob/master/docs/issue-402.md",
		Body: "科技爱好者周刊（第 402 期）：我在智念 AI 的日子（小说）\n\n这里记录每周值得分享的科技内容。\n\n科技动态\n\n新闻",
	}
	got := canonicalizeEvidenceBackedTitle(
		"智念 AI 日常：AI时代认知与职场",
		"主文是一篇AI小说，此外还有科技动态。",
		content,
	)
	if got != "科技周刊402：我在智念AI的日子" {
		t.Fatalf("digest title = %q", got)
	}
}

func TestCanonicalDigestTitleFallsBackToHeadingIssue(t *testing.T) {
	t.Parallel()

	content := fetcher.Content{
		URL:  "https://example.com/weekly/latest.md",
		Body: "# 科技爱好者周刊（第 17 期）: Building Better APIs（小说）\n\n科技动态\n\n新闻",
	}
	if got := canonicalDigestTitle(content); got != "科技周刊17：Building Better APIs" {
		t.Fatalf("fallback digest title = %q", got)
	}
}

func TestCanonicalDigestTitleHonorsDisplayLimit(t *testing.T) {
	t.Parallel()

	content := fetcher.Content{
		URL:  "https://example.com/weekly/issue-999.md",
		Body: "科技爱好者周刊（第 999 期）：" + strings.Repeat("很长的周刊主题", 10),
	}
	got := canonicalDigestTitle(content)
	if count := runeCount(got); count > 36 {
		t.Fatalf("digest title rune count = %d, want <= 36: %q", count, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated digest title = %q, want ellipsis", got)
	}
}

func TestCanonicalizeEvidenceBackedSummaryUsesNaturalUserspaceTranslation(t *testing.T) {
	t.Parallel()

	content := fetcher.Content{
		Title: "Everything I know about good API design",
		Body:  "WE DO NOT BREAK USERSPACE. Support idempotency and cursor pagination.",
	}
	got := canonicalizeEvidenceBackedSummary(
		"公共API需严格避免破用户空间的改变，只能添加字段而不能移除现有结构。",
		content,
	)
	want := "公共API需严格避免破坏用户代码的变更，只能添加字段而不能移除现有结构。"
	if got != want {
		t.Fatalf("API summary = %q, want %q", got, want)
	}
}

func TestCanonicalizeEvidenceBackedSummaryDoesNotAttributeExampleProduct(t *testing.T) {
	t.Parallel()

	content := fetcher.Content{
		Title: "Everything I know about good API design",
		Body: "Most engineers work with APIs, like this one from Twilio. " +
			"I've spent a lot of time working with APIs, both building and using them.",
	}
	got := canonicalizeEvidenceBackedSummary(
		"作者分享了多年构建Twilio等公共API的经验，核心是保持兼容。",
		content,
	)
	want := "作者分享了多年构建和使用公共API的经验，核心是保持兼容。"
	if got != want {
		t.Fatalf("API attribution summary = %q, want %q", got, want)
	}
}

func TestCanonicalizeEvidenceBackedSummaryRestoresCompleteAPIArticleCoverage(t *testing.T) {
	t.Parallel()

	content := fetcher.Content{
		Title: "Everything I know about good API design",
		Body: strings.Join([]string{
			"WE DO NOT BREAK USERSPACE",
			"Use a long-lived API key for authentication.",
			"Idempotency and retries make writes safe.",
			"Use cursor-based pagination for large datasets.",
			"Optional fields keep expensive response data off by default.",
		}, "\n"),
	}
	got := canonicalizeEvidenceBackedSummary(
		"本文总结了API设计经验，核心是平衡熟悉性和灵活性，并避免破坏用户空间。",
		content,
	)
	want := "好的API应在熟悉度与灵活性之间取舍，让使用者无需反复查文档；公共API一旦发布就应保持向后兼容，优先新增字段，避免修改或删除既有结构。作者还建议使用简单的API密钥、为写操作提供幂等重试、对大型数据集采用游标分页，并将昂贵字段设为可选。"
	if got != want {
		t.Fatalf("complete API summary = %q, want %q", got, want)
	}
}

func TestCanonicalizeEvidenceBackedSummaryRemovesUnsupportedDigestInferences(t *testing.T) {
	t.Parallel()

	content := fetcher.Content{
		URL: "https://github.com/example/weekly/blob/main/docs/issue-402.md",
		Body: "科技爱好者周刊（第 402 期）：我在智念 AI 的日子（小说）\n\n" +
			"首席执行官对新员工宣讲公司的使命。\n\n科技动态\n\n新闻",
	}
	got := canonicalizeEvidenceBackedSummary(
		"小说讲述员工在AI主导的虚拟公司中工作经历，从创始人使命宣讲到AI生成代码。",
		content,
	)
	want := "小说讲述员工在高度依赖AI的公司中的工作经历，从首席执行官的使命宣讲到AI生成代码。"
	if got != want {
		t.Fatalf("digest summary = %q, want %q", got, want)
	}

	grounded := fetcher.Content{
		Body: "科技爱好者周刊：虚拟公司专题\n\n创始人介绍了一家虚拟公司。\n\n科技动态\n\n新闻",
	}
	original := "创始人介绍了一家虚拟公司。"
	if unchanged := canonicalizeEvidenceBackedSummary(original, grounded); unchanged != original {
		t.Fatalf("grounded summary changed: %q", unchanged)
	}
}
