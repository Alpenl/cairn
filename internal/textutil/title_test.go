package textutil

import (
	"strings"
	"testing"
)

// TestIsGenericTitle 锁定 fetch / analyzer / pipeline 三处 fallback 判定
// 共同依赖的 "无意义标题" 集合。这个集合决定了：抓到 "Home" / "404" /
// "Untitled" 这类壳标题时是否触发 og:title 二次回退或降低 fetch
// quality。回归会让前端把"Home"当成真实标题展示给用户。
func TestIsGenericTitle(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		// 集合里的字面值，覆盖每条 entry。
		{"home_lower", "home", true},
		{"homepage_lower", "homepage", true},
		{"index_lower", "index", true},
		{"untitled_lower", "untitled", true},
		{"404_literal", "404", true},
		{"403_literal", "403", true},
		{"not_found_with_space", "not found", true},
		{"x_shell_title", "X", true},
		{"twitter_shell_title", "Twitter", true},

		// 大小写不敏感 + 多空格折叠 + 前后 trim。
		{"home_uppercase", "HOME", true},
		{"home_titlecase", "Home", true},
		{"home_padded", "  Home  ", true},
		{"not_found_multispace", "Not   Found", true},
		{"not_found_tabs", "\tnot\tfound\n", true},

		// 空字符串 / 全空白都视为"无标题"。
		{"empty", "", true},
		{"whitespace_only", "   \t\n", true},

		// 反例：真实标题不应被误判。
		{"real_article_title", "Understanding context cancellation in Go", false},
		{"real_chinese_title", "深入理解 pgx 连接池", false},
		{"home_substring_inside_real_title", "Welcome home, traveler", false},
		{"404_inside_title", "Build error 4040 in production", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsGenericTitle(tc.in); got != tc.want {
				t.Errorf("IsGenericTitle(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestChooseTitle(t *testing.T) {
	t.Parallel()

	const longPost = "(19) X 上的 GitHubDaily：“想验证自己的选股思路，现成炒股软件改不动，自己搭环境又太折腾。tickflow-stock-panel 是个自托管的 A 股量化工作台。” / X"
	tests := []struct {
		name      string
		generated string
		source    string
		summary   string
		url       string
		want      string
	}{
		{
			name:      "generated title wins over social page title",
			generated: "tickflow A股量化工作台",
			source:    longPost,
			summary:   "无关摘要",
			want:      "tickflow A股量化工作台",
		},
		{
			name:      "long model title falls back to summary topic",
			generated: longPost,
			source:    longPost,
			summary:   "tickflow-stock-panel 是一个自托管的 A 股量化工作台，整合选股、监控和回测功能。",
			want:      "tickflow-stock-panel：自托管的 A 股量化工作台",
		},
		{
			name:    "short source title remains compatible",
			source:  "深入理解 Go 错误处理",
			summary: "摘要不应覆盖已有短标题。",
			want:    "深入理解 Go 错误处理",
		},
		{
			name: "missing title uses social account fallback",
			url:  "https://x.com/GitHub_Daily/status/123456",
			want: "@GitHub_Daily 的 X 动态",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := ChooseTitle(test.generated, test.source, test.summary, test.url)
			if got != test.want {
				t.Fatalf("ChooseTitle() = %q, want %q", got, test.want)
			}
			if Count(got) > MaxTitleRunes {
				t.Fatalf("ChooseTitle() returned %d runes: %q", Count(got), got)
			}
			if strings.Contains(got, "现成炒股软件改不动") {
				t.Fatalf("ChooseTitle() copied social post body: %q", got)
			}
		})
	}
}
