package analyzer

import (
	"encoding/json"
	"os"
	"testing"
)

// analyzerRegressionCase is the row shape of testdata/analyzer_regression_corpus.json.
// Lives here (alongside loadAnalyzerRegressionCorpus) because the corpus is the
// only consumer; it is not exported because tests in this package are the only
// readers.
type analyzerRegressionCase struct {
	Name              string   `json:"name"`
	Raw               string   `json:"raw"`
	MaxSummaryChars   int      `json:"max_summary_chars"`
	MinTags           int      `json:"min_tags"`
	MaxTags           int      `json:"max_tags"`
	MaxTagChars       int      `json:"max_tag_chars"`
	WantSummary       string   `json:"want_summary"`
	WantTags          []string `json:"want_tags"`
	WantErrorContains string   `json:"want_error_contains"`
}

func loadAnalyzerRegressionCorpus(t *testing.T) []analyzerRegressionCase {
	t.Helper()

	data, err := os.ReadFile("testdata/analyzer_regression_corpus.json")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var cases []analyzerRegressionCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("analyzer regression corpus is empty")
	}
	return cases
}
