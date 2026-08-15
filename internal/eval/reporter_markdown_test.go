package eval

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"webtag/internal/summarypolicy"
)

func TestRenderMarkdownIncludesHeadlineMatrixAndDetails(t *testing.T) {
	t.Parallel()
	res := RunResult{
		RunID:     "20260517T120000Z",
		StartedAt: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		Duration:  3 * time.Second,
		Config: RunConfigSummary{
			Fetchers: []FetcherName{"basic", "light"},
			Prompts:  []string{"default", "v2"},
			Models:   []string{"gpt-4.1-mini"},
		},
		Cells: []CellResult{
			{
				CaseID: "z1", URL: "https://example.com/1",
				Fetcher: "basic", Prompt: "default", Model: "gpt-4.1-mini",
				FetchOK: true, AnalyzeOK: true, Tags: []string{"RAG"},
				Rule:              RuleScore{Raw: 2, Normalised: 0.5, MaxPossible: 4, Breakdown: ScoreBreakdown{MustIncludeHit: 1}},
				Summary:           "知识库 RAG 框架",
				SummaryProfile:    "article",
				SummaryRule:       summarypolicy.Assessment{Score: 0.75, LengthRunes: 128, Issues: []string{"too_many_sentences"}},
				ContentContract:   ContentContractAssessment{Configured: true, Passed: false, Issues: []string{"summary_missing:decision"}},
				JudgeScore:        4,
				JudgeSummaryScore: 5,
				JudgeTagScore:     3,
			},
			{
				CaseID: "z1", URL: "https://example.com/1",
				Fetcher: "light", Prompt: "v2", Model: "gpt-4.1-mini",
				Err: "fetch: deadline", Rule: RuleScore{Normalised: 0},
			},
		},
	}
	var buf bytes.Buffer
	if err := RenderMarkdown(&buf, res); err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	s := buf.String()
	for _, marker := range []string{
		"# WebTag Eval Run 20260517T120000Z",
		"## Headline",
		"## Matrix",
		"## Details",
		"`z1`",
		"`basic`",
		"`light`",
		"https://example.com/1",
		"fetch: deadline",
		"知识库 RAG 框架",
		"Summary rule",
		"Content contract",
		"Judge summary",
		"Judge tags",
		"article",
		"too_many_sentences",
		"summary_missing:decision",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("report missing %q\n--- report ---\n%s\n", marker, s)
		}
	}
}

func TestRenderMarkdownEscapesPipeInErrors(t *testing.T) {
	t.Parallel()
	res := RunResult{
		Cells: []CellResult{{
			CaseID: "c", Fetcher: "basic", Prompt: "p", Model: "m",
			Err: "weird | pipe | message",
		}},
		Config: RunConfigSummary{
			Fetchers: []FetcherName{"basic"}, Prompts: []string{"p"}, Models: []string{"m"},
		},
	}
	var buf bytes.Buffer
	if err := RenderMarkdown(&buf, res); err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	if !strings.Contains(buf.String(), `weird \| pipe \| message`) {
		t.Fatalf("pipe not escaped:\n%s", buf.String())
	}
}
