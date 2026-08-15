package analyzer

import (
	"strings"
	"testing"

	"webtag/internal/model"
)

// TestTruncatedSummaryFallbackSetsLibraryKind 锁住截断回退路径的归属。
//
// 该路径调用 v1 校验器 validateAnalysisResponse，它不设 LibraryKind；而回退结果
// 会被 parseAnalysisResponse 直接返回，绕过 validateAnalysisResponseForRequest
// 里补默认值的那一步。曾因此产出空归属，一路流到 CompleteReadingParse（要求
// Kind 必须是 reading）导致本可成功的解析失败。
func TestTruncatedSummaryFallbackSetsLibraryKind(t *testing.T) {
	t.Parallel()

	a := &OpenAIAnalyzer{}
	// summary 超出上限触发截断回退；不带 schema_version / library_kind，
	// 走的正是 v1 校验器。
	data := map[string]any{
		"summary": strings.Repeat("这是一段很长的摘要内容。", 200),
		"tags":    []any{"Go"},
		"title":   "标题",
	}

	result, ok := a.truncatedSummaryFallback(data, 120)
	if !ok {
		t.Fatal("截断回退未生效，用例前提不成立")
	}
	if result.LibraryKind != model.LibraryKindReading {
		t.Fatalf("LibraryKind = %q, want reading——analyzer 不应对外产出非法归属", result.LibraryKind)
	}
}
