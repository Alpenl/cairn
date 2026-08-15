package service

import (
	"strings"
	"testing"
)

func TestBuildSiteEmbeddingInputUsesOnlySiteProfileFields(t *testing.T) {
	t.Parallel()
	bodySentinel := "DO-NOT-EMBED-CAPTURED-BODY"
	input := BuildSiteEmbeddingInput(SiteEmbeddingDocument{
		Name: "Example", Intro: "Useful developer tool", DisplayHost: "example.com",
		Tags:    []string{"Go", "go", " tools "},
		Entries: []SiteEmbeddingEntry{{Name: "API", Purpose: "Create integrations"}, {Name: "Guide", Purpose: "Read setup instructions"}},
	})
	for _, want := range []string{"site: Example", "intro: Useful developer tool", "host: example.com", "tag: Go", "tag: tools", "entry: API", "purpose: Create integrations"} {
		if !strings.Contains(input, want) {
			t.Fatalf("input missing %q: %s", want, input)
		}
	}
	if strings.Count(input, "tag: Go") != 1 || strings.Contains(input, bodySentinel) {
		t.Fatalf("input has duplicate tag or body sentinel: %s", input)
	}
}

func TestBuildSiteEmbeddingInputDropsBlankFields(t *testing.T) {
	t.Parallel()
	if got := BuildSiteEmbeddingInput(SiteEmbeddingDocument{Tags: []string{" ", ""}, Entries: []SiteEmbeddingEntry{{Name: " ", Purpose: ""}}}); got != "" {
		t.Fatalf("input = %q, want empty", got)
	}
}
