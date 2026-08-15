package eval

import (
	"math"
	"slices"
	"testing"
)

func TestScoreTagsAllMustAndNiceHit(t *testing.T) {
	t.Parallel()
	s := ScoreTags(
		[]string{"RAG", "腾讯", "ima"},
		CaseExpected{
			MustInclude: []string{"RAG"},
			NiceToHave:  []string{"腾讯", "ima"},
		},
	)
	// raw = 1*2 (must) + 2*1 (nice) = 4; max = 1*2 + 2*1 = 4; → 1.0
	if s.Raw != 4 || s.MaxPossible != 4 || math.Abs(s.Normalised-1.0) > 1e-9 {
		t.Fatalf("score = %+v, want raw=4 max=4 norm=1.0", s)
	}
}

func TestScoreTagsMustNotIncludeBreachLowersScore(t *testing.T) {
	t.Parallel()
	s := ScoreTags(
		[]string{"RAG", "技术"},
		CaseExpected{
			MustInclude:    []string{"RAG"},
			MustNotInclude: []string{"技术"},
		},
	)
	// raw = 2 - 3 = -1 → normalised clamped to 0
	if s.Raw != -1 || s.Normalised != 0 {
		t.Fatalf("score = %+v, want raw=-1 norm=0", s)
	}
	if s.Breakdown.MustNotIncludeBreach != 1 {
		t.Fatalf("breach count = %d, want 1", s.Breakdown.MustNotIncludeBreach)
	}
}

func TestScoreTagsEmptyTagsApplyPenalty(t *testing.T) {
	t.Parallel()
	s := ScoreTags(
		nil,
		CaseExpected{MustInclude: []string{"RAG"}},
	)
	// raw = 0 (no hits) - 2 (empty tags) = -2 → norm clamped to 0
	if s.Raw != -2 || s.Normalised != 0 {
		t.Fatalf("score = %+v, want raw=-2 norm=0", s)
	}
}

func TestScoreTagsCaseInsensitiveMatching(t *testing.T) {
	t.Parallel()
	s := ScoreTags(
		[]string{"rag", "ImA "},
		CaseExpected{
			MustInclude: []string{"RAG"},
			NiceToHave:  []string{"ima"},
		},
	)
	if s.Normalised != 1.0 {
		t.Fatalf("case-insensitive match should land at 1.0; got %+v", s)
	}
}

func TestScoreTagsZeroExpectedListsProduceZeroScore(t *testing.T) {
	t.Parallel()
	s := ScoreTags([]string{"anything"}, CaseExpected{})
	if s.MaxPossible != 0 || s.Normalised != 0 {
		t.Fatalf("empty expected should produce zero max + zero score; got %+v", s)
	}
}

func TestScoreTagsPartialHitNormalises(t *testing.T) {
	t.Parallel()
	s := ScoreTags(
		[]string{"RAG"},
		CaseExpected{
			MustInclude: []string{"RAG"},
			NiceToHave:  []string{"腾讯", "ima"},
		},
	)
	// raw = 2 (must hit) + 0 (no nice hit) = 2; max = 2 + 2 = 4; → 0.5
	if math.Abs(s.Normalised-0.5) > 1e-9 {
		t.Fatalf("normalised = %f, want 0.5", s.Normalised)
	}
}

func TestAssessContentContractChecksSummaryAndTitleSemantics(t *testing.T) {
	t.Parallel()

	got := AssessContentContract(
		"Go 暂缓错误处理语法变更",
		"Go 团队决定在可预见的未来停止推进错误处理语法变更，并关闭以语法为主的提案。",
		CaseExpected{
			Summary: TextExpected{
				MustInclude:    []string{"决定", "可预见的未来", "关闭"},
				MustNotInclude: []string{"永久取消"},
			},
			Title: TextExpected{
				MustIncludeAny: []string{"暂缓", "可预见"},
				MustNotInclude: []string{"停止引入"},
			},
		},
	)

	if !got.Configured || !got.Passed || len(got.Issues) != 0 {
		t.Fatalf("content contract = %+v, want configured pass", got)
	}
}

func TestAssessContentContractReportsMissingAndForbiddenFacts(t *testing.T) {
	t.Parallel()

	got := AssessContentContract(
		"Go 停止引入错误处理语法",
		"文章只回顾了几个历史提案。",
		CaseExpected{
			Summary: TextExpected{MustInclude: []string{"团队决定", "可预见的未来"}},
			Title: TextExpected{
				MustIncludeAny: []string{"暂缓", "可预见"},
				MustNotInclude: []string{"停止引入"},
			},
		},
	)

	if got.Passed {
		t.Fatalf("content contract unexpectedly passed: %+v", got)
	}
	for _, issue := range []string{
		"summary_missing:团队决定",
		"summary_missing:可预见的未来",
		"title_missing_any:暂缓|可预见",
		"title_forbidden:停止引入",
	} {
		if !slices.Contains(got.Issues, issue) {
			t.Errorf("content contract missing issue %q: %+v", issue, got)
		}
	}
}
