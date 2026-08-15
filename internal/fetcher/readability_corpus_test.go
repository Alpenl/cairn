package fetcher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const (
	readabilityCorpusDir         = "testdata/readability-corpus"
	readabilityV1BaselineFile    = "baseline-v1.json"
	readabilityV2ApprovedFile    = "approved-v2.json"
	readabilityDifferenceFile    = "accepted-differences.json"
	readabilityBaselineUpdateEnv = "WEBTAG_UPDATE_READABILITY_BASELINE"
)

type readabilityCorpusManifest struct {
	Cases []readabilityCorpusCase `json:"cases"`
}

type readabilityCorpusCase struct {
	ID          string `json:"id"`
	File        string `json:"file"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Scenario    string `json:"scenario"`
	Source      string `json:"source"`
}

type readabilityBaseline struct {
	Library string                       `json:"library"`
	Cases   map[string]readabilityOutput `json:"cases"`
}

type readabilityOutput struct {
	Body         string `json:"body"`
	Title        string `json:"title"`
	DocumentHTML string `json:"document_html"`
	Author       string `json:"author"`
	Excerpt      string `json:"excerpt"`
	SiteName     string `json:"site_name"`
	Language     string `json:"language"`
}

type readabilityDifference struct {
	CaseID   string `json:"case_id"`
	Field    string `json:"field"`
	V1SHA256 string `json:"v1_sha256"`
	V2SHA256 string `json:"v2_sha256"`
	Reason   string `json:"reason"`
}

func TestReadabilityCorpusMatchesApprovedV2(t *testing.T) {
	t.Parallel()

	manifest := loadReadabilityManifest(t)
	if len(manifest.Cases) < 30 {
		t.Fatalf("fixture count = %d, want at least 30", len(manifest.Cases))
	}

	actual := make(map[string]readabilityOutput, len(manifest.Cases))
	seen := make(map[string]struct{}, len(manifest.Cases))
	for _, fixture := range manifest.Cases {
		if fixture.ID == "" || fixture.Scenario == "" || fixture.Source == "" || fixture.URL == "" || fixture.ContentType == "" {
			t.Fatalf("fixture %+v must document id, scenario, source, URL, and content type", fixture)
		}
		if filepath.Base(fixture.File) != fixture.File || filepath.Ext(fixture.File) != ".html" {
			t.Fatalf("fixture %q has unsafe or non-HTML file %q", fixture.ID, fixture.File)
		}
		if _, duplicate := seen[fixture.ID]; duplicate {
			t.Fatalf("duplicate fixture id %q", fixture.ID)
		}
		seen[fixture.ID] = struct{}{}

		html, err := os.ReadFile(filepath.Join(readabilityCorpusDir, fixture.File))
		if err != nil {
			t.Fatalf("read fixture %q: %v", fixture.ID, err)
		}
		body, title, documentHTML, metadata := extractReadableContent(fixture.URL, html, fixture.ContentType)
		actual[fixture.ID] = readabilityOutput{
			Body:         body,
			Title:        title,
			DocumentHTML: documentHTML,
			Author:       metadataString(metadata, "author"),
			Excerpt:      metadataString(metadata, "excerpt"),
			SiteName:     metadataString(metadata, "site_name"),
			Language:     metadataString(metadata, "language"),
		}
	}

	if target := os.Getenv(readabilityBaselineUpdateEnv); target != "" {
		writeReadabilityBaseline(t, target, actual)
		return
	}

	v1 := loadReadabilityBaseline(t, readabilityV1BaselineFile)
	v2 := loadReadabilityBaseline(t, readabilityV2ApprovedFile)
	assertBaselineCoverage(t, manifest, v1)
	assertBaselineCoverage(t, manifest, v2)
	if !reflect.DeepEqual(actual, v2.Cases) {
		for _, fixture := range manifest.Cases {
			if !reflect.DeepEqual(actual[fixture.ID], v2.Cases[fixture.ID]) {
				t.Errorf("fixture %q output drifted from approved v2 baseline\nactual: %#v\nwant:   %#v", fixture.ID, actual[fixture.ID], v2.Cases[fixture.ID])
			}
		}
	}

	wantDifferences := baselineDifferences(v1.Cases, v2.Cases)
	accepted := loadAcceptedReadabilityDifferences(t)
	if !reflect.DeepEqual(wantDifferences, accepted) {
		t.Fatalf("accepted v1/v2 differences are incomplete or stale\ncomputed: %#v\naccepted: %#v", wantDifferences, accepted)
	}
	for _, difference := range accepted {
		if strings.TrimSpace(difference.Reason) == "" {
			t.Errorf("accepted difference %s/%s has no review reason", difference.CaseID, difference.Field)
		}
	}
}

func loadReadabilityManifest(t *testing.T) readabilityCorpusManifest {
	t.Helper()
	var manifest readabilityCorpusManifest
	readReadabilityJSON(t, "manifest.json", &manifest)
	return manifest
}

func loadReadabilityBaseline(t *testing.T, name string) readabilityBaseline {
	t.Helper()
	var baseline readabilityBaseline
	readReadabilityJSON(t, name, &baseline)
	return baseline
}

func loadAcceptedReadabilityDifferences(t *testing.T) []readabilityDifference {
	t.Helper()
	var differences []readabilityDifference
	readReadabilityJSON(t, readabilityDifferenceFile, &differences)
	return differences
}

func readReadabilityJSON(t *testing.T, name string, target any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(readabilityCorpusDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

func writeReadabilityBaseline(t *testing.T, target string, cases map[string]readabilityOutput) {
	t.Helper()
	var name, library string
	switch target {
	case "v1":
		name = readabilityV1BaselineFile
		library = "github.com/go-shiori/go-readability v0.0.0-20251205110129-5db1dc9836f0"
	case "v2":
		name = readabilityV2ApprovedFile
		library = "codeberg.org/readeck/go-readability/v2 v2.1.2"
	default:
		t.Fatalf("%s=%q, want v1 or v2", readabilityBaselineUpdateEnv, target)
	}
	data, err := json.MarshalIndent(readabilityBaseline{Library: library, Cases: cases}, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(readabilityCorpusDir, name), data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func assertBaselineCoverage(t *testing.T, manifest readabilityCorpusManifest, baseline readabilityBaseline) {
	t.Helper()
	if len(baseline.Cases) != len(manifest.Cases) {
		t.Fatalf("%s case count = %d, want %d", baseline.Library, len(baseline.Cases), len(manifest.Cases))
	}
	for _, fixture := range manifest.Cases {
		if _, ok := baseline.Cases[fixture.ID]; !ok {
			t.Errorf("%s is missing fixture %q", baseline.Library, fixture.ID)
		}
	}
}

func baselineDifferences(v1, v2 map[string]readabilityOutput) []readabilityDifference {
	fields := []struct {
		name string
		get  func(readabilityOutput) string
	}{
		{"body", func(output readabilityOutput) string { return output.Body }},
		{"title", func(output readabilityOutput) string { return output.Title }},
		{"document_html", func(output readabilityOutput) string { return output.DocumentHTML }},
		{"author", func(output readabilityOutput) string { return output.Author }},
		{"excerpt", func(output readabilityOutput) string { return output.Excerpt }},
		{"site_name", func(output readabilityOutput) string { return output.SiteName }},
		{"language", func(output readabilityOutput) string { return output.Language }},
	}
	var differences []readabilityDifference
	for caseID, before := range v1 {
		after, ok := v2[caseID]
		if !ok {
			continue
		}
		for _, field := range fields {
			beforeValue, afterValue := field.get(before), field.get(after)
			if beforeValue == afterValue {
				continue
			}
			differences = append(differences, readabilityDifference{
				CaseID:   caseID,
				Field:    field.name,
				V1SHA256: readabilityValueHash(beforeValue),
				V2SHA256: readabilityValueHash(afterValue),
				Reason:   acceptedReadabilityDifferenceReason(caseID, field.name),
			})
		}
	}
	sort.Slice(differences, func(i, j int) bool {
		if differences[i].CaseID == differences[j].CaseID {
			return differences[i].Field < differences[j].Field
		}
		return differences[i].CaseID < differences[j].CaseID
	})
	return differences
}

func acceptedReadabilityDifferenceReason(caseID, field string) string {
	// Reasons are deliberately keyed in source so adding or changing an accepted
	// extraction difference requires a code-reviewed decision, not a fixture rewrite.
	return readabilityDifferenceReasons[caseID+"/"+field]
}

var readabilityDifferenceReasons = map[string]string{
	"news-report/body":                  "Accepted: v2 preserves the visible dateline at the start of the article; no v1 text is removed.",
	"news-report/document_html":         "Accepted: the HTML change is limited to retaining the same visible dateline node included in the approved body.",
	"scientific-abstract/body":          "Accepted: v2 preserves the visible author line before the abstract; no v1 text is removed.",
	"scientific-abstract/document_html": "Accepted: the HTML change is limited to retaining the same visible author node included in the approved body.",
	"standard-blog/body":                "Accepted: v2 preserves the visible byline before the article paragraphs; no v1 text is removed.",
	"standard-blog/document_html":       "Accepted: the HTML change is limited to retaining the same visible byline node included in the approved body.",
}

func readabilityValueHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok {
		return ""
	}
	return fmt.Sprint(value)
}
