package eval

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func cell(id, fetcher, prompt, model string, ruleNorm float64, judge float64, err string) CellResult {
	return CellResult{
		CaseID: id, Fetcher: FetcherName(fetcher), Prompt: prompt, Model: model,
		Rule: RuleScore{Normalised: ruleNorm}, JudgeScore: judge, Err: err,
	}
}

func withSummaryScore(c CellResult, score float64) CellResult {
	c.SummaryRule.Score = score
	return c
}

func runWith(id string, cells ...CellResult) RunResult {
	return RunResult{
		RunID:     id,
		StartedAt: time.Now(),
		Cells:     cells,
	}
}

func TestDiffRunsClassifiesRegressFixAddedRemoved(t *testing.T) {
	t.Parallel()
	a := runWith("a",
		cell("c1", "basic", "p", "m", 0.8, 4.0, ""),     // will regress (rule down)
		cell("c2", "basic", "p", "m", 0.6, 3.0, "boom"), // will be fixed (err clears in B)
		cell("c3", "basic", "p", "m", 0.5, 3.5, ""),     // removed from B
		cell("c4", "basic", "p", "m", 0.4, 2.0, ""),     // unchanged
	)
	b := runWith("b",
		cell("c1", "basic", "p", "m", 0.4, 4.0, ""), // regress
		cell("c2", "basic", "p", "m", 0.6, 3.0, ""), // fix (err gone)
		// c3 missing — removed
		cell("c4", "basic", "p", "m", 0.4, 2.0, ""), // unchanged
		cell("c5", "basic", "p", "m", 0.9, 5.0, ""), // added
	)

	diffs := DiffRuns(a, b, 0.05)

	// c4 should be filtered (unchanged within threshold)
	for _, d := range diffs {
		if d.Key.CaseID == "c4" {
			t.Errorf("unchanged cell should be filtered by threshold")
		}
	}

	bucketCount := map[string]int{}
	for _, d := range diffs {
		bucketCount[bucketLabel(d)]++
	}
	// c1: rule dropped but no err flip → "worse"
	if bucketCount["worse"] == 0 {
		t.Errorf("missing worse bucket: %+v", bucketCount)
	}
	// c2: err cleared → "fixed"
	if bucketCount["fixed"] == 0 {
		t.Errorf("missing fixed bucket: %+v", bucketCount)
	}
	if bucketCount["removed"] != 1 || bucketCount["added"] != 1 {
		t.Errorf("expected 1 removed + 1 added; got %+v", bucketCount)
	}
}

func TestDiffRunsRegressBucketFiresOnNewError(t *testing.T) {
	t.Parallel()
	a := runWith("a", cell("c1", "basic", "p", "m", 0.8, 4.0, ""))
	b := runWith("b", cell("c1", "basic", "p", "m", 0.0, 0, "fetch: deadline"))
	d := DiffRuns(a, b, 0.05)
	if len(d) != 1 || bucketLabel(d[0]) != "regress" {
		t.Fatalf("expected regress; got %v", d)
	}
}

func TestDiffRunsZeroThresholdShowsEveryChange(t *testing.T) {
	t.Parallel()
	a := runWith("a", cell("c1", "f", "p", "m", 0.50, 0, ""))
	b := runWith("b", cell("c1", "f", "p", "m", 0.51, 0, ""))
	d := DiffRuns(a, b, 0)
	if len(d) != 1 {
		t.Fatalf("zero threshold should show 0.01 delta; got %d diffs", len(d))
	}
}

func TestDiffRunsDetectsSummaryRegressionWhenTagsAreUnchanged(t *testing.T) {
	t.Parallel()
	a := runWith("a", withSummaryScore(cell("c1", "f", "p", "m", 0.8, 4, ""), 1))
	b := runWith("b", withSummaryScore(cell("c1", "f", "p", "m", 0.8, 4, ""), 0.25))
	diffs := DiffRuns(a, b, 0.05)
	if len(diffs) != 1 {
		t.Fatalf("summary-only regression should produce one diff, got %d", len(diffs))
	}
	if diffs[0].SummaryDelta != -0.75 || bucketLabel(diffs[0]) != "worse" {
		t.Fatalf("summary regression = %+v, want delta -0.75 in worse bucket", diffs[0])
	}
}

func TestRenderDiffMarkdownProducesPRReadyTable(t *testing.T) {
	t.Parallel()
	a := runWith("base", cell("c1", "basic", "p", "m", 0.8, 4.0, ""))
	b := runWith("after", cell("c1", "basic", "p", "m", 0.5, 4.0, ""))
	diffs := DiffRuns(a, b, 0.05)
	var buf bytes.Buffer
	if err := RenderDiffMarkdown(&buf, a, b, diffs); err != nil {
		t.Fatalf("RenderDiffMarkdown: %v", err)
	}
	s := buf.String()
	for _, want := range []string{"# Eval Diff", "`base` → `after`", "worse", "-0.30", "A summary", "Δ summary"} {
		if !strings.Contains(s, want) {
			t.Errorf("diff markdown missing %q\n%s", want, s)
		}
	}
}
