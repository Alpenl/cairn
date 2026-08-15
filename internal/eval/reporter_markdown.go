package eval

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// RenderMarkdown writes a single self-contained markdown document.
// Section order: run header → headline per-case summary table → full
// matrix (one row per cell) → per-case details with raw tag lists,
// breakdowns, error messages. Designed for git-commitable diff
// reading: stable row ordering, no timestamps inside table cells.
func RenderMarkdown(w io.Writer, result RunResult) error {
	fmt.Fprintf(w, "# WebTag Eval Run %s\n\n", result.RunID)
	fmt.Fprintf(w, "- Started: %s\n", result.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintf(w, "- Duration: %s\n", result.Duration.Round(1e6))
	fmt.Fprintf(w, "- Fetchers: `%s`\n", joinFetcherNames(result.Config.Fetchers, ", "))
	fmt.Fprintf(w, "- Prompts: `%s`\n", strings.Join(result.Config.Prompts, ", "))
	fmt.Fprintf(w, "- Models: `%s`\n", strings.Join(result.Config.Models, ", "))
	fmt.Fprintf(w, "- Cells: %d\n\n", len(result.Cells))

	if err := renderHeadlineTable(w, result.Cells); err != nil {
		return err
	}
	if err := renderMatrix(w, result.Cells); err != nil {
		return err
	}
	return renderDetails(w, result.Cells)
}

// renderHeadlineTable groups by case and shows the best score
// achieved across the matrix for each one. Lets an operator
// triage at a glance: "case foo never breaks 0.4 across any
// combination → the URL or the expected list needs a rethink".
func renderHeadlineTable(w io.Writer, cells []CellResult) error {
	type agg struct {
		caseID       string
		url          string
		bestTag      float64
		worstTag     float64
		bestSummary  float64
		worstSummary float64
		cells        int
		errors       int
	}
	byCase := make(map[string]*agg)
	caseOrder := []string{}
	for _, c := range cells {
		a, ok := byCase[c.CaseID]
		if !ok {
			a = &agg{
				caseID: c.CaseID, url: c.URL,
				bestTag: -1, worstTag: 2,
				bestSummary: -1, worstSummary: 2,
			}
			byCase[c.CaseID] = a
			caseOrder = append(caseOrder, c.CaseID)
		}
		a.cells++
		if c.Err != "" {
			a.errors++
		}
		if c.Rule.Normalised > a.bestTag {
			a.bestTag = c.Rule.Normalised
		}
		if c.Rule.Normalised < a.worstTag {
			a.worstTag = c.Rule.Normalised
		}
		if c.SummaryRule.Score > a.bestSummary {
			a.bestSummary = c.SummaryRule.Score
		}
		if c.SummaryRule.Score < a.worstSummary {
			a.worstSummary = c.SummaryRule.Score
		}
	}
	if _, err := fmt.Fprintln(w, "## Headline"); err != nil {
		return err
	}
	fmt.Fprintln(w)
	if _, err := fmt.Fprintln(w, "| Case | Best tag | Worst tag | Best summary | Worst summary | Cells | Errors |"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "|------|----------|-----------|--------------|---------------|-------|--------|"); err != nil {
		return err
	}
	for _, id := range caseOrder {
		a := byCase[id]
		fmt.Fprintf(w, "| `%s` | %s | %s | %s | %s | %d | %d |\n", id,
			fmtScore(a.bestTag), fmtScore(a.worstTag),
			fmtScore(a.bestSummary), fmtScore(a.worstSummary),
			a.cells, a.errors)
	}
	fmt.Fprintln(w)
	return nil
}

// renderMatrix is the full one-row-per-cell view. Sorted by
// (case_id, fetcher, prompt, model) so two runs of the same config
// diff cleanly under `git diff`.
func renderMatrix(w io.Writer, cells []CellResult) error {
	sorted := append([]CellResult(nil), cells...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].CaseID != sorted[j].CaseID {
			return sorted[i].CaseID < sorted[j].CaseID
		}
		if sorted[i].Fetcher != sorted[j].Fetcher {
			return sorted[i].Fetcher < sorted[j].Fetcher
		}
		if sorted[i].Prompt != sorted[j].Prompt {
			return sorted[i].Prompt < sorted[j].Prompt
		}
		return sorted[i].Model < sorted[j].Model
	})

	fmt.Fprintln(w, "## Matrix")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Case | Fetcher | Prompt | Model | Tag rule | Summary rule | Content contract | Judge overall | Judge summary | Judge tags | Tags | Fetch ms | Analyze ms | Error |")
	fmt.Fprintln(w, "|------|---------|--------|-------|----------|--------------|------------------|---------------|---------------|------------|------|----------|------------|-------|")
	for _, c := range sorted {
		fmt.Fprintf(w, "| `%s` | `%s` | `%s` | `%s` | %s | %s | %s | %s | %s | %s | %s | %d | %d | %s |\n",
			c.CaseID, c.Fetcher, c.Prompt, c.Model,
			fmtScore(c.Rule.Normalised),
			fmtScore(c.SummaryRule.Score),
			fmtContract(c.ContentContract),
			fmtJudge(c.JudgeScore),
			fmtJudge(c.JudgeSummaryScore),
			fmtJudge(c.JudgeTagScore),
			fmtTagsInline(c.Tags),
			c.FetchDurationMS, c.AnalyzeDurationMS,
			escapePipe(c.Err),
		)
	}
	fmt.Fprintln(w)
	return nil
}

// fmtJudge renders the LLM judge column. 0 means "not judged" — the
// run did not have --judge enabled, OR the cell errored before
// reaching the judge. Either way `—` is more honest than `0.00`.
func fmtJudge(s float64) string {
	if s <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f", s)
}

// renderDetails prints per-cell breakdowns: raw tags, scoring
// rationale, error message, body sample. Surfaces enough so the
// operator can tell whether a low score is a tag quality issue, a
// fetch failure, or an expected-list bug — without rerunning.
func renderDetails(w io.Writer, cells []CellResult) error {
	fmt.Fprintln(w, "## Details")
	fmt.Fprintln(w)
	byCase := map[string][]CellResult{}
	order := []string{}
	for _, c := range cells {
		if _, ok := byCase[c.CaseID]; !ok {
			order = append(order, c.CaseID)
		}
		byCase[c.CaseID] = append(byCase[c.CaseID], c)
	}
	for _, id := range order {
		fmt.Fprintf(w, "### %s\n\n", id)
		fmt.Fprintf(w, "URL: %s\n\n", byCase[id][0].URL)
		for _, c := range byCase[id] {
			fmt.Fprintf(w, "- **%s / %s / %s** — tag rule %s (raw %d / max %d)\n",
				c.Fetcher, c.Prompt, c.Model, fmtScore(c.Rule.Normalised), c.Rule.Raw, c.Rule.MaxPossible)
			bd := c.Rule.Breakdown
			fmt.Fprintf(w, "  - must: %d/%d  nice: %d/%d  violations: %d  fetched: %d chars  body title: %s\n",
				bd.MustIncludeHit, bd.MustIncludeHit+bd.MustIncludeMiss,
				bd.NiceToHaveHit, bd.NiceToHaveHit+bd.NiceToHaveMiss,
				bd.MustNotIncludeBreach, c.BodyChars, escapePipe(c.Title))
			if len(c.Tags) > 0 {
				fmt.Fprintf(w, "  - tags: %s\n", fmtTagsInline(c.Tags))
			}
			if c.Summary != "" {
				fmt.Fprintf(w, "  - summary: %s\n", oneLine(c.Summary))
			}
			fmt.Fprintf(w, "  - summary quality: profile %s, rule %s, %d chars, issues: %s\n",
				coalesceReport(c.SummaryProfile), fmtScore(c.SummaryRule.Score),
				c.SummaryRule.LengthRunes, fmtIssues(c.SummaryRule.Issues))
			if c.ContentContract.Configured {
				fmt.Fprintf(w, "  - content contract: %s, issues: %s\n",
					fmtContract(c.ContentContract), fmtIssues(c.ContentContract.Issues))
			}
			if c.JudgeScore > 0 || c.JudgeReason != "" {
				fmt.Fprintf(w, "  - judge: overall %s, summary %s, tags %s — %s\n",
					fmtJudge(c.JudgeScore), fmtJudge(c.JudgeSummaryScore), fmtJudge(c.JudgeTagScore), oneLine(c.JudgeReason))
			}
			if c.Err != "" {
				fmt.Fprintf(w, "  - error: %s\n", escapePipe(c.Err))
			}
		}
		fmt.Fprintln(w)
	}
	return nil
}

func fmtContract(contract ContentContractAssessment) string {
	if !contract.Configured {
		return "—"
	}
	if contract.Passed {
		return "pass"
	}
	return "**FAIL**"
}

func fmtScore(s float64) string {
	if s < 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f", s)
}

func fmtTagsInline(tags []string) string {
	if len(tags) == 0 {
		return "—"
	}
	quoted := make([]string, len(tags))
	for i, t := range tags {
		quoted[i] = "`" + t + "`"
	}
	return strings.Join(quoted, " ")
}

func escapePipe(s string) string {
	if s == "" {
		return ""
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func fmtIssues(issues []string) string {
	if len(issues) == 0 {
		return "none"
	}
	return strings.Join(issues, ", ")
}

func coalesceReport(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}

func joinFetcherNames(ns []FetcherName, sep string) string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = string(n)
	}
	return strings.Join(out, sep)
}
