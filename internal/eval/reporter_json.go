package eval

import (
	"encoding/json"
	"io"
	"time"

	"webtag/internal/summarypolicy"
)

// jsonRun is the on-wire shape of an eval run. Lifted into its own
// type (rather than json-tagging RunResult directly) for two reasons:
//  1. RunResult exposes Duration as time.Duration, but consumers
//     reading the file want milliseconds; the conversion lives in
//     one place.
//  2. The JSON schema must stay stable across refactors of the
//     in-memory struct — clients (BI tools, diff command) bind to
//     it. The wire type is therefore the public contract.
type jsonRun struct {
	Schema     string           `json:"schema"`
	RunID      string           `json:"run_id"`
	StartedAt  time.Time        `json:"started_at"`
	DurationMS int64            `json:"duration_ms"`
	Config     RunConfigSummary `json:"config"`
	Cells      []jsonCell       `json:"cells"`
}

type jsonCell struct {
	CaseID            string                    `json:"case_id"`
	URL               string                    `json:"url"`
	Fetcher           FetcherName               `json:"fetcher"`
	Prompt            string                    `json:"prompt"`
	Model             string                    `json:"model"`
	FetchOK           bool                      `json:"fetch_ok"`
	FetchDurationMS   int64                     `json:"fetch_duration_ms"`
	Title             string                    `json:"title,omitempty"`
	BodyChars         int                       `json:"body_chars"`
	AnalyzeOK         bool                      `json:"analyze_ok"`
	AnalyzeDurationMS int64                     `json:"analyze_duration_ms"`
	Summary           string                    `json:"summary,omitempty"`
	SummaryProfile    string                    `json:"summary_profile,omitempty"`
	SummaryScore      float64                   `json:"summary_score"`
	SummaryChars      int                       `json:"summary_chars"`
	SummaryIssues     []string                  `json:"summary_issues"`
	ContentContract   ContentContractAssessment `json:"content_contract"`
	Tags              []string                  `json:"tags"`
	RuleRaw           int                       `json:"rule_raw"`
	RuleNorm          float64                   `json:"rule_norm"`
	RuleMax           int                       `json:"rule_max"`
	RuleBreakdown     ScoreBreakdown            `json:"rule_breakdown"`
	JudgeScore        float64                   `json:"judge_score,omitempty"`
	JudgeSummaryScore float64                   `json:"judge_summary_score,omitempty"`
	JudgeTagScore     float64                   `json:"judge_tag_score,omitempty"`
	JudgeReason       string                    `json:"judge_reason,omitempty"`
	Err               string                    `json:"err,omitempty"`
}

// RenderJSON writes the full run as a stable-key indented JSON
// document. Consumers (the diff subcommand, downstream BI scripts)
// bind to the jsonRun schema so refactors of the in-memory
// RunResult don't silently break them.
func RenderJSON(w io.Writer, result RunResult) error {
	out := jsonRun{
		Schema:     "webtag.eval/v1",
		RunID:      result.RunID,
		StartedAt:  result.StartedAt,
		DurationMS: result.Duration.Milliseconds(),
		Config:     result.Config,
		Cells:      make([]jsonCell, 0, len(result.Cells)),
	}
	for _, c := range result.Cells {
		out.Cells = append(out.Cells, jsonCell{
			CaseID:            c.CaseID,
			URL:               c.URL,
			Fetcher:           c.Fetcher,
			Prompt:            c.Prompt,
			Model:             c.Model,
			FetchOK:           c.FetchOK,
			FetchDurationMS:   c.FetchDurationMS,
			Title:             c.Title,
			BodyChars:         c.BodyChars,
			AnalyzeOK:         c.AnalyzeOK,
			AnalyzeDurationMS: c.AnalyzeDurationMS,
			Summary:           c.Summary,
			SummaryProfile:    c.SummaryProfile,
			SummaryScore:      c.SummaryRule.Score,
			SummaryChars:      c.SummaryRule.LengthRunes,
			SummaryIssues:     append([]string{}, c.SummaryRule.Issues...),
			ContentContract:   c.ContentContract,
			Tags:              c.Tags,
			RuleRaw:           c.Rule.Raw,
			RuleNorm:          c.Rule.Normalised,
			RuleMax:           c.Rule.MaxPossible,
			RuleBreakdown:     c.Rule.Breakdown,
			JudgeScore:        c.JudgeScore,
			JudgeSummaryScore: c.JudgeSummaryScore,
			JudgeTagScore:     c.JudgeTagScore,
			JudgeReason:       c.JudgeReason,
			Err:               c.Err,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// LoadJSON parses a previously-written eval JSON back into a
// RunResult so the diff subcommand can compare two runs.
func LoadJSON(r io.Reader) (RunResult, error) {
	var raw jsonRun
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return RunResult{}, err
	}
	out := RunResult{
		RunID:     raw.RunID,
		StartedAt: raw.StartedAt,
		Duration:  time.Duration(raw.DurationMS) * time.Millisecond,
		Config:    raw.Config,
		Cells:     make([]CellResult, 0, len(raw.Cells)),
	}
	for _, c := range raw.Cells {
		out.Cells = append(out.Cells, CellResult{
			CaseID:            c.CaseID,
			URL:               c.URL,
			Fetcher:           c.Fetcher,
			Prompt:            c.Prompt,
			Model:             c.Model,
			FetchOK:           c.FetchOK,
			FetchDurationMS:   c.FetchDurationMS,
			Title:             c.Title,
			BodyChars:         c.BodyChars,
			AnalyzeOK:         c.AnalyzeOK,
			AnalyzeDurationMS: c.AnalyzeDurationMS,
			Summary:           c.Summary,
			SummaryProfile:    c.SummaryProfile,
			SummaryRule: summarypolicy.Assessment{
				Score:       c.SummaryScore,
				LengthRunes: c.SummaryChars,
				Issues:      c.SummaryIssues,
			},
			ContentContract: c.ContentContract,
			Tags:            c.Tags,
			Rule: RuleScore{
				Raw:         c.RuleRaw,
				Normalised:  c.RuleNorm,
				MaxPossible: c.RuleMax,
				Breakdown:   c.RuleBreakdown,
			},
			JudgeScore:        c.JudgeScore,
			JudgeSummaryScore: c.JudgeSummaryScore,
			JudgeTagScore:     c.JudgeTagScore,
			JudgeReason:       c.JudgeReason,
			Err:               c.Err,
		})
	}
	return out, nil
}
