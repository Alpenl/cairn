package analyzer

import "testing"

// TestNormalizeTag covers the format safety-net applied to model-emitted
// tags before length/dedup/count validation: hashtag/bracket stripping,
// slash-hierarchy collapse, whitespace folding, and the one-in-one-out
// guarantee (it never splits or mints extra tags).
func TestNormalizeTag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain unchanged", "Go", "Go"},
		{"strip hashtag", "#Go", "Go"},
		{"strip fullwidth hashtag", "＃错误处理", "错误处理"},
		{"strip wrapping brackets", "[Go]", "Go"},
		{"strip wrapping quotes", `"try proposal"`, "try proposal"},
		{"slash keeps longest segment", "check/handle", "handle"},
		{"backslash hierarchy", "a\\bbbb", "bbbb"},
		{"collapse inner whitespace", "Go   2", "Go 2"},
		{"trim outer whitespace", "  错误处理  ", "错误处理"},
		{"empty stays empty", "   ", ""},
		{"hashtag only stays empty", "#", ""},
	}

	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeTag(tt.in); got != tt.want {
				t.Fatalf("normalizeTag(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestNormalizeTagFlowsThroughParser verifies the parser applies
// normalizeTag end-to-end: a hashtagged, sloppily-spaced, slash-hierarchied
// tag set comes out clean while count/dedup still hold.
func TestNormalizeTagFlowsThroughParser(t *testing.T) {
	t.Parallel()

	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		APIKey:          "secret-key",
		Model:           "gpt-test",
		MaxSummaryChars: 20,
		MinTags:         2,
		MaxTags:         5,
		MaxTagChars:     20,
	})

	got, err := analyzer.parseAnalysisResponse(`{"summary":"Go错误处理提案","tags":["#Go","check/handle","  语言提案  "]}`)
	if err != nil {
		t.Fatalf("parseAnalysisResponse() error = %v, want success", err)
	}
	if len(got.Tags) != 3 || got.Tags[0] != "Go" || got.Tags[1] != "handle" || got.Tags[2] != "语言提案" {
		t.Fatalf("tags = %#v, want [Go handle 语言提案]", got.Tags)
	}
}
