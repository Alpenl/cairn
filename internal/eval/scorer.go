package eval

import (
	"strings"
	"unicode"
)

// ScoreBreakdown carries the per-component counts behind a rule
// score so the reporter can render "1/2 must, 2/4 nice, 0 violations"
// rather than just the final number — operators evaluating tag
// quality care which knob moved.
type ScoreBreakdown struct {
	MustIncludeHit       int
	MustIncludeMiss      int
	NiceToHaveHit        int
	NiceToHaveMiss       int
	MustNotIncludeBreach int
}

// RuleScore is the rule-based score for one (case, tags) pair. Raw
// is the unbounded signed integer the rules produce; Normalised is
// clamped to [0, 1] for cross-case comparability. Both are surfaced
// so a power user can sort by raw (which case had the most absolute
// hits?) without losing the headline normalised number.
type RuleScore struct {
	Raw         int
	Normalised  float64
	MaxPossible int
	Breakdown   ScoreBreakdown
}

// ContentContractAssessment reports whether generated title/summary text kept
// the reviewed facts encoded in a case file. It complements the generic shape
// score: length and formatting can pass while the principal decision is absent.
type ContentContractAssessment struct {
	Configured bool     `json:"configured"`
	Passed     bool     `json:"passed"`
	Issues     []string `json:"issues,omitempty"`
}

const (
	mustIncludePoints     = 2
	niceToHavePoints      = 1
	mustNotIncludePenalty = 3
	emptyTagSlicePenalty  = 2
)

// ScoreTags runs the rule-based scorer. Tags are matched
// case-insensitively against the expected lists; surrounding
// whitespace is trimmed on both sides. Duplicate tags in the input
// score at most once per expectation slot — re-listing "RAG" twice
// does not double-count.
//
// Empty-tag-slice penalty: a fetcher/analyzer combo that returns
// zero tags is strictly worse than returning a non-matching tag,
// because zero tags means the link is invisible in tag-based
// browsing. The -2 penalty bottoms out the score even when the case
// had no must_include expectations.
func ScoreTags(tags []string, expected CaseExpected) RuleScore {
	normalisedTags := normaliseSet(tags)

	bd := ScoreBreakdown{}
	for _, t := range expected.MustInclude {
		if normalisedTags[normalise(t)] {
			bd.MustIncludeHit++
		} else {
			bd.MustIncludeMiss++
		}
	}
	for _, t := range expected.NiceToHave {
		if normalisedTags[normalise(t)] {
			bd.NiceToHaveHit++
		} else {
			bd.NiceToHaveMiss++
		}
	}
	for _, t := range expected.MustNotInclude {
		if normalisedTags[normalise(t)] {
			bd.MustNotIncludeBreach++
		}
	}

	raw := bd.MustIncludeHit*mustIncludePoints +
		bd.NiceToHaveHit*niceToHavePoints -
		bd.MustNotIncludeBreach*mustNotIncludePenalty
	if len(tags) == 0 {
		raw -= emptyTagSlicePenalty
	}

	maxPossible := len(expected.MustInclude)*mustIncludePoints + len(expected.NiceToHave)*niceToHavePoints
	normalised := 0.0
	if maxPossible > 0 {
		v := float64(raw) / float64(maxPossible)
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		normalised = v
	}

	return RuleScore{
		Raw:         raw,
		Normalised:  normalised,
		MaxPossible: maxPossible,
		Breakdown:   bd,
	}
}

// AssessContentContract performs deterministic, whitespace-insensitive
// substring checks against the reviewed semantic anchors in an eval case.
func AssessContentContract(title, summary string, expected CaseExpected) ContentContractAssessment {
	configured := textExpectedConfigured(expected.Title) || textExpectedConfigured(expected.Summary)
	assessment := ContentContractAssessment{Configured: configured, Passed: true}
	if !configured {
		return assessment
	}

	assessment.Issues = append(assessment.Issues, assessText("summary", summary, expected.Summary)...)
	assessment.Issues = append(assessment.Issues, assessText("title", title, expected.Title)...)
	assessment.Passed = len(assessment.Issues) == 0
	return assessment
}

func assessText(scope, value string, expected TextExpected) []string {
	normalized := normaliseText(value)
	issues := make([]string, 0)
	for _, phrase := range expected.MustInclude {
		if !strings.Contains(normalized, normaliseText(phrase)) {
			issues = append(issues, scope+"_missing:"+phrase)
		}
	}
	if len(expected.MustIncludeAny) > 0 {
		found := false
		for _, phrase := range expected.MustIncludeAny {
			if strings.Contains(normalized, normaliseText(phrase)) {
				found = true
				break
			}
		}
		if !found {
			issues = append(issues, scope+"_missing_any:"+strings.Join(expected.MustIncludeAny, "|"))
		}
	}
	for _, phrase := range expected.MustNotInclude {
		if strings.Contains(normalized, normaliseText(phrase)) {
			issues = append(issues, scope+"_forbidden:"+phrase)
		}
	}
	return issues
}

func textExpectedConfigured(expected TextExpected) bool {
	return len(expected.MustInclude) > 0 || len(expected.MustIncludeAny) > 0 || len(expected.MustNotInclude) > 0
}

func normaliseText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, strings.TrimSpace(value))
}

func normalise(tag string) string {
	return strings.ToLower(strings.TrimSpace(tag))
}

func normaliseSet(tags []string) map[string]bool {
	out := make(map[string]bool, len(tags))
	for _, t := range tags {
		k := normalise(t)
		if k != "" {
			out[k] = true
		}
	}
	return out
}
