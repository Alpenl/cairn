package eval

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"webtag/internal/summarypolicy"
)

func sampleRun() RunResult {
	return RunResult{
		RunID:     "20260517T120000Z",
		StartedAt: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		Duration:  3500 * time.Millisecond,
		Config:    RunConfigSummary{Fetchers: []FetcherName{"basic"}, Prompts: []string{"default"}, Models: []string{"m1"}},
		Cells: []CellResult{
			{
				CaseID: "c1", URL: "https://x", Fetcher: "basic", Prompt: "default", Model: "m1",
				FetchOK: true, AnalyzeOK: true, Tags: []string{"RAG", "腾讯"},
				Rule:              RuleScore{Raw: 2, Normalised: 0.5, MaxPossible: 4, Breakdown: ScoreBreakdown{MustIncludeHit: 1}},
				SummaryProfile:    "article",
				SummaryRule:       summarypolicy.Assessment{Score: 0.75, LengthRunes: 128, Issues: []string{"too_many_sentences"}},
				ContentContract:   ContentContractAssessment{Configured: true, Passed: false, Issues: []string{"summary_missing:decision"}},
				JudgeScore:        4.0,
				JudgeSummaryScore: 4.5,
				JudgeTagScore:     3.5,
				JudgeReason:       "ok",
			},
		},
	}
}

func TestRenderJSONRoundTripsThroughLoadJSON(t *testing.T) {
	t.Parallel()
	original := sampleRun()
	var buf bytes.Buffer
	if err := RenderJSON(&buf, original); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if !strings.Contains(buf.String(), `"schema": "webtag.eval/v1"`) {
		t.Fatalf("missing schema marker:\n%s", buf.String())
	}
	rendered := buf.String()
	loaded, err := LoadJSON(&buf)
	if err != nil {
		t.Fatalf("LoadJSON: %v", err)
	}
	if loaded.RunID != original.RunID {
		t.Errorf("RunID mismatch")
	}
	if loaded.Duration != original.Duration {
		t.Errorf("Duration = %v, want %v", loaded.Duration, original.Duration)
	}
	if loaded.Cells[0].Rule.Normalised != 0.5 {
		t.Errorf("rule norm = %f, want 0.5", loaded.Cells[0].Rule.Normalised)
	}
	if loaded.Cells[0].JudgeScore != 4.0 {
		t.Errorf("judge = %f, want 4", loaded.Cells[0].JudgeScore)
	}
	if loaded.Cells[0].SummaryProfile != "article" || loaded.Cells[0].SummaryRule.Score != 0.75 || loaded.Cells[0].SummaryRule.LengthRunes != 128 {
		t.Errorf("summary quality did not round trip: %+v", loaded.Cells[0])
	}
	if loaded.Cells[0].JudgeSummaryScore != 4.5 || loaded.Cells[0].JudgeTagScore != 3.5 {
		t.Errorf("judge subscores did not round trip: %+v", loaded.Cells[0])
	}
	if !loaded.Cells[0].ContentContract.Configured || loaded.Cells[0].ContentContract.Passed {
		t.Errorf("content contract did not round trip: %+v", loaded.Cells[0].ContentContract)
	}
	for _, field := range []string{`"summary_profile": "article"`, `"summary_score": 0.75`, `"summary_chars": 128`, `"summary_issues"`, `"content_contract"`, `"summary_missing:decision"`, `"judge_summary_score": 4.5`, `"judge_tag_score": 3.5`} {
		if !strings.Contains(rendered, field) {
			t.Errorf("JSON missing %s:\n%s", field, rendered)
		}
	}
}

func TestRenderCSVProducesHeaderAndDataRows(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderCSV(&buf, sampleRun()); err != nil {
		t.Fatalf("RenderCSV: %v", err)
	}
	s := buf.String()
	if !strings.HasPrefix(s, "run_id,case_id,url,") {
		t.Fatalf("missing or wrong header row:\n%s", s)
	}
	// Tag join uses pipe so the column stays scalar.
	if !strings.Contains(s, "RAG|腾讯") {
		t.Fatalf("tags column missing pipe-joined value:\n%s", s)
	}
	for _, column := range []string{"summary_profile", "summary_score", "summary_chars", "summary_issues", "content_contract", "content_contract_issues", "judge_summary_score", "judge_tag_score"} {
		if !strings.Contains(strings.SplitN(s, "\n", 2)[0], column) {
			t.Errorf("CSV header missing %q:\n%s", column, s)
		}
	}
}

func TestLoadJSONAcceptsLegacyCellsWithoutSummaryQualityFields(t *testing.T) {
	t.Parallel()
	legacy := `{"schema":"webtag.eval/v1","run_id":"old","started_at":"2026-05-17T12:00:00Z","duration_ms":1,"config":{},"cells":[{"case_id":"c","url":"https://x","fetcher":"basic","prompt":"default","model":"m","tags":[],"rule_raw":0,"rule_norm":0,"rule_max":0,"rule_breakdown":{}}]}`
	loaded, err := LoadJSON(strings.NewReader(legacy))
	if err != nil {
		t.Fatalf("LoadJSON legacy: %v", err)
	}
	cell := loaded.Cells[0]
	if cell.SummaryRule.Score != 0 || cell.JudgeSummaryScore != 0 || cell.JudgeTagScore != 0 {
		t.Fatalf("legacy optional fields should remain zero: %+v", cell)
	}
}
