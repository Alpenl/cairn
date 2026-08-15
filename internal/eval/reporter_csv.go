package eval

import (
	"encoding/csv"
	"io"
	"strconv"
	"strings"
)

// RenderCSV writes one row per cell with a flat header layout, ready
// for `csv.import` into a spreadsheet / BI tool / pandas notebook.
// Tags are joined with `|` (a character that already cannot appear
// in a single CSV field without quoting) so the column stays
// scalar — `,` would conflict with the field separator.
func RenderCSV(w io.Writer, result RunResult) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	header := []string{
		"run_id", "case_id", "url",
		"fetcher", "prompt", "model",
		"fetch_ok", "fetch_ms",
		"analyze_ok", "analyze_ms",
		"body_chars", "title", "summary",
		"summary_profile", "summary_score", "summary_chars", "summary_issues",
		"content_contract", "content_contract_issues",
		"tags",
		"rule_raw", "rule_norm", "rule_max",
		"must_hit", "must_miss", "nice_hit", "nice_miss", "violations",
		"judge_score", "judge_summary_score", "judge_tag_score", "judge_reason",
		"error",
	}
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, c := range result.Cells {
		row := []string{
			result.RunID,
			c.CaseID,
			c.URL,
			string(c.Fetcher), c.Prompt, c.Model,
			strconv.FormatBool(c.FetchOK), strconv.FormatInt(c.FetchDurationMS, 10),
			strconv.FormatBool(c.AnalyzeOK), strconv.FormatInt(c.AnalyzeDurationMS, 10),
			strconv.Itoa(c.BodyChars), c.Title, c.Summary,
			c.SummaryProfile,
			strconv.FormatFloat(c.SummaryRule.Score, 'f', 4, 64),
			strconv.Itoa(c.SummaryRule.LengthRunes),
			strings.Join(c.SummaryRule.Issues, "|"),
			fmtContentContract(c.ContentContract),
			strings.Join(c.ContentContract.Issues, "|"),
			strings.Join(c.Tags, "|"),
			strconv.Itoa(c.Rule.Raw), strconv.FormatFloat(c.Rule.Normalised, 'f', 4, 64), strconv.Itoa(c.Rule.MaxPossible),
			strconv.Itoa(c.Rule.Breakdown.MustIncludeHit),
			strconv.Itoa(c.Rule.Breakdown.MustIncludeMiss),
			strconv.Itoa(c.Rule.Breakdown.NiceToHaveHit),
			strconv.Itoa(c.Rule.Breakdown.NiceToHaveMiss),
			strconv.Itoa(c.Rule.Breakdown.MustNotIncludeBreach),
			strconv.FormatFloat(c.JudgeScore, 'f', 2, 64),
			strconv.FormatFloat(c.JudgeSummaryScore, 'f', 2, 64),
			strconv.FormatFloat(c.JudgeTagScore, 'f', 2, 64),
			c.JudgeReason,
			c.Err,
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func fmtContentContract(contract ContentContractAssessment) string {
	if !contract.Configured {
		return ""
	}
	if contract.Passed {
		return "pass"
	}
	return "fail"
}
