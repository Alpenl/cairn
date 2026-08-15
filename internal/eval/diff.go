package eval

import (
	"fmt"
	"io"
	"sort"
)

// CellKey identifies a cell across runs. Two runs of the same config
// produce cells with identical keys, so a diff is a simple key match.
type CellKey struct {
	CaseID  string
	Fetcher FetcherName
	Prompt  string
	Model   string
}

func cellKey(c CellResult) CellKey {
	return CellKey{c.CaseID, c.Fetcher, c.Prompt, c.Model}
}

// CellDiff is the per-key change between baseline (A) and current (B).
// Either side may be missing (cell present only in one run) — the
// nil pointer for the missing side is what distinguishes "regression"
// from "newly added" from "removed".
type CellDiff struct {
	Key               CellKey
	A                 *CellResult // baseline; nil when newly added
	B                 *CellResult // current; nil when removed
	RuleDelta         float64     // B.Rule.Norm - A.Rule.Norm; 0 if either is missing
	SummaryDelta      float64
	JudgeDelta        float64
	JudgeSummaryDelta float64
	JudgeTagDelta     float64
	StatusFlip        string // "fix" / "regress" / "" — flips OK<->error between runs
}

// DiffRuns compares two runs and returns sorted cell diffs. Order:
//  1. Regressions first (rule score dropped)
//  2. Cells removed from B
//  3. Cells added to B
//  4. Improvements
//  5. Unchanged
//
// Inside each bucket: by (case_id, fetcher, prompt, model).
//
// Threshold caller-supplied — small flutter (±0.05) is noise unless
// the operator explicitly wants to see it.
func DiffRuns(a, b RunResult, threshold float64) []CellDiff {
	if threshold < 0 {
		threshold = 0
	}
	aMap := indexCells(a.Cells)
	bMap := indexCells(b.Cells)

	out := make([]CellDiff, 0, len(aMap)+len(bMap))
	seen := make(map[CellKey]struct{}, len(aMap))
	for k, aCell := range aMap {
		seen[k] = struct{}{}
		bCell, ok := bMap[k]
		if !ok {
			a := aCell
			out = append(out, CellDiff{Key: k, A: &a})
			continue
		}
		a := aCell
		b := bCell
		d := CellDiff{
			Key:               k,
			A:                 &a,
			B:                 &b,
			RuleDelta:         b.Rule.Normalised - a.Rule.Normalised,
			SummaryDelta:      b.SummaryRule.Score - a.SummaryRule.Score,
			JudgeDelta:        b.JudgeScore - a.JudgeScore,
			JudgeSummaryDelta: b.JudgeSummaryScore - a.JudgeSummaryScore,
			JudgeTagDelta:     b.JudgeTagScore - a.JudgeTagScore,
		}
		if a.Err == "" && b.Err != "" {
			d.StatusFlip = "regress"
		} else if a.Err != "" && b.Err == "" {
			d.StatusFlip = "fix"
		}
		// Drop noise: tiny delta, no status flip, no judge swing.
		if abs(d.RuleDelta) < threshold &&
			abs(d.SummaryDelta) < threshold &&
			abs(d.JudgeDelta) < threshold &&
			abs(d.JudgeSummaryDelta) < threshold &&
			abs(d.JudgeTagDelta) < threshold &&
			d.StatusFlip == "" {
			continue
		}
		out = append(out, d)
	}
	for k, bCell := range bMap {
		if _, ok := seen[k]; ok {
			continue
		}
		b := bCell
		out = append(out, CellDiff{Key: k, B: &b})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return diffBucket(out[i]) < diffBucket(out[j]) ||
			(diffBucket(out[i]) == diffBucket(out[j]) && lessKey(out[i].Key, out[j].Key))
	})
	return out
}

// diffBucket orders by "what does the operator want to read first".
// Regressions before fixes; existing-cell changes before set diffs.
func diffBucket(d CellDiff) int {
	switch {
	case d.StatusFlip == "regress":
		return 0
	case d.A != nil && d.B != nil && (d.RuleDelta < 0 || d.SummaryDelta < 0 || d.JudgeDelta < 0 || d.JudgeSummaryDelta < 0 || d.JudgeTagDelta < 0):
		return 1
	case d.A != nil && d.B == nil:
		return 2
	case d.A == nil && d.B != nil:
		return 3
	case d.StatusFlip == "fix":
		return 4
	default:
		return 5
	}
}

func lessKey(a, b CellKey) bool {
	if a.CaseID != b.CaseID {
		return a.CaseID < b.CaseID
	}
	if a.Fetcher != b.Fetcher {
		return a.Fetcher < b.Fetcher
	}
	if a.Prompt != b.Prompt {
		return a.Prompt < b.Prompt
	}
	return a.Model < b.Model
}

func indexCells(cells []CellResult) map[CellKey]CellResult {
	out := make(map[CellKey]CellResult, len(cells))
	for _, c := range cells {
		out[cellKey(c)] = c
	}
	return out
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// RenderDiffMarkdown is a compact markdown view of a diff. Designed
// to be pasted into a PR description: one section, short table, no
// per-cell deep-dive.
func RenderDiffMarkdown(w io.Writer, a, b RunResult, diffs []CellDiff) error {
	fmt.Fprintf(w, "# Eval Diff: `%s` → `%s`\n\n", a.RunID, b.RunID)
	fmt.Fprintf(w, "- A cells: %d  B cells: %d  changed: %d\n", len(a.Cells), len(b.Cells), len(diffs))
	fmt.Fprintln(w)
	if len(diffs) == 0 {
		fmt.Fprintln(w, "No changes above threshold.")
		return nil
	}
	fmt.Fprintln(w, "| Bucket | Case | Fetcher | Prompt | Model | A tag rule | B tag rule | Δ tag rule | A summary | B summary | Δ summary | A judge | B judge | Δ judge | A judge summary | B judge summary | Δ judge summary | A judge tags | B judge tags | Δ judge tags | Status |")
	fmt.Fprintln(w, "|--------|------|---------|--------|-------|------------|------------|------------|-----------|-----------|-----------|---------|---------|---------|-----------------|-----------------|-----------------|--------------|--------------|--------------|--------|")
	for _, d := range diffs {
		bucket := bucketLabel(d)
		aRule, bRule, dRule := pairWithDelta(d.A, d.B, func(c *CellResult) float64 { return c.Rule.Normalised })
		aSummary, bSummary, dSummary := pairWithDelta(d.A, d.B, func(c *CellResult) float64 { return c.SummaryRule.Score })
		aJudge, bJudge, dJudge := pairWithDelta(d.A, d.B, func(c *CellResult) float64 { return c.JudgeScore })
		aJudgeSummary, bJudgeSummary, dJudgeSummary := pairWithDelta(d.A, d.B, func(c *CellResult) float64 { return c.JudgeSummaryScore })
		aJudgeTags, bJudgeTags, dJudgeTags := pairWithDelta(d.A, d.B, func(c *CellResult) float64 { return c.JudgeTagScore })
		fmt.Fprintf(w, "| %s | `%s` | `%s` | `%s` | `%s` | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			bucket, d.Key.CaseID, d.Key.Fetcher, d.Key.Prompt, d.Key.Model,
			aRule, bRule, dRule,
			aSummary, bSummary, dSummary,
			aJudge, bJudge, dJudge,
			aJudgeSummary, bJudgeSummary, dJudgeSummary,
			aJudgeTags, bJudgeTags, dJudgeTags,
			d.StatusFlip,
		)
	}
	return nil
}

func bucketLabel(d CellDiff) string {
	switch diffBucket(d) {
	case 0:
		return "regress"
	case 1:
		return "worse"
	case 2:
		return "removed"
	case 3:
		return "added"
	case 4:
		return "fixed"
	default:
		return "changed"
	}
}

func pairWithDelta(a, b *CellResult, pick func(*CellResult) float64) (string, string, string) {
	aStr := "—"
	bStr := "—"
	if a != nil {
		aStr = fmt.Sprintf("%.2f", pick(a))
	}
	if b != nil {
		bStr = fmt.Sprintf("%.2f", pick(b))
	}
	if a == nil || b == nil {
		return aStr, bStr, "—"
	}
	delta := pick(b) - pick(a)
	sign := ""
	if delta > 0 {
		sign = "+"
	}
	return aStr, bStr, fmt.Sprintf("%s%.2f", sign, delta)
}
