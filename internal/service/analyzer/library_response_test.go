package analyzer

import (
	"strings"
	"testing"

	"webtag/internal/model"
)

func TestParseLibraryV2ReadingResponse(t *testing.T) {
	a := newLibraryResponseAnalyzer()
	got, err := a.parseAnalysisResponseForRequest(`{"schema_version":2,"library_kind":"reading","classification_confidence":0.92,"classification_reason":"ai_reading_resolution","classification_explanation":"长文为主","reading_profile":{"title":"游标分页","summary":"解释游标分页的一致性设计。","tags":["API","分页"]}}`, 120, model.RequestedLibraryKindAuto)
	if err != nil {
		t.Fatal(err)
	}
	if got.LibraryKind != model.LibraryKindReading || got.Summary == "" || got.SiteName != "" {
		t.Fatalf("result = %#v", got)
	}
}

func TestParseLibraryV2SiteResponseForExplicitSite(t *testing.T) {
	a := newLibraryResponseAnalyzer()
	got, err := a.parseAnalysisResponseForRequest(`{"schema_version":2,"library_kind":"site","classification_confidence":1,"classification_reason":"explicit_site","classification_explanation":"用户选择网站","site_profile":{"name":"Excalidraw","intro":"一个可在浏览器使用的开源白板和图表工具。","entry_name":"首页","purpose":"在线绘图与协作","tags":["白板","绘图"]}}`, 120, model.RequestedLibraryKindSite)
	if err != nil {
		t.Fatal(err)
	}
	if got.LibraryKind != model.LibraryKindSite || got.SiteName != "Excalidraw" || got.Summary != "" {
		t.Fatalf("result = %#v", got)
	}
}

func TestLibraryV2RejectsExplicitKindConflictAndLegacySite(t *testing.T) {
	a := newLibraryResponseAnalyzer()
	_, err := a.parseAnalysisResponseForRequest(`{"schema_version":2,"library_kind":"reading","classification_confidence":0.8,"classification_reason":"x","classification_explanation":"x","reading_profile":{"title":"t","summary":"摘要","tags":[]}}`, 120, model.RequestedLibraryKindSite)
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("error = %v", err)
	}
	_, err = a.parseAnalysisResponseForRequest(`{"title":"t","summary":"摘要","tags":[]}`, 120, model.RequestedLibraryKindSite)
	if err == nil || !strings.Contains(err.Error(), "schema_version=2") {
		t.Fatalf("error = %v", err)
	}
}

func TestLibraryPromptRequestsDiscriminatedProfile(t *testing.T) {
	a := newLibraryResponseAnalyzer()
	payload := a.buildAnalyzePayload(AnalyzeRequest{RequestedLibraryKind: model.RequestedLibraryKindSite})
	messages := payload["messages"].([]map[string]any)
	prompt := messages[0]["content"].(string)
	for _, want := range []string{"schema_version=2", `"site_profile"`, "必须为 site", "不得生成 summary"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %s", want, prompt)
		}
	}
}

func newLibraryResponseAnalyzer() *OpenAIAnalyzer {
	return NewOpenAIAnalyzer(OpenAIAnalyzerOptions{BaseURL: "https://example.com", APIKey: "test", Model: "test", MaxSummaryChars: 120, MaxTagChars: 32})
}
