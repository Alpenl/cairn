package service

import (
	"testing"

	"webtag/internal/model"
	"webtag/internal/service/urlmeta"
)

func TestClassifyLibraryPriority(t *testing.T) {
	existing := ClassificationDecision{Kind: model.LibraryKindSite, Source: model.LibraryKindSourceAuto, Confidence: .9}
	got := ClassifyLibrary(ClassificationInput{RequestedKind: model.RequestedLibraryKindReading, ExistingDecision: &existing})
	if got.Kind != model.LibraryKindSite || got.Reason != "existing_final_decision" {
		t.Fatalf("existing decision was not preserved: %#v", got)
	}

	got = ClassifyLibrary(ClassificationInput{RequestedKind: model.RequestedLibraryKindReading, CaptureSignals: CaptureSignals{JSONLDTypes: []string{"WebApplication"}}})
	if got.Kind != model.LibraryKindReading || !got.Locked || got.Source != model.LibraryKindSourceUser {
		t.Fatalf("explicit reading = %#v", got)
	}
}

func TestClassifyLibraryHardSourceRules(t *testing.T) {
	got := ClassifyLibrary(ClassificationInput{SourceKind: "text"})
	if got.Kind != model.LibraryKindReading || got.Reason != "unstable_ingest_source" {
		t.Fatalf("text ingest = %#v", got)
	}
	got = ClassifyLibrary(ClassificationInput{SourceKind: "rss", URL: "https://example.com"})
	if got.Kind != model.LibraryKindReading || got.Reason != "feed_reading_source" {
		t.Fatalf("rss ingest = %#v", got)
	}
}

func TestClassifyLibraryScoresAndDefersAmbiguous(t *testing.T) {
	reading := ClassifyLibrary(ClassificationInput{URL: "https://example.com/blog/a-long-post", URLMetadata: urlmeta.ClassifyURL("https://example.com/blog/a-long-post"), CaptureSignals: CaptureSignals{OGType: "article", HasAuthorAndPublished: true, ProseDominant: true}})
	if reading.Kind != model.LibraryKindReading || reading.Source != model.LibraryKindSourceAuto || reading.Confidence < .7 {
		t.Fatalf("reading decision = %#v", reading)
	}

	site := ClassifyLibrary(ClassificationInput{URL: "https://tool.example.com/", URLMetadata: urlmeta.ClassifyURL("https://tool.example.com/"), CaptureSignals: CaptureSignals{JSONLDTypes: []string{"WebApplication"}, HasWebAppManifest: true}})
	if site.Kind != model.LibraryKindSite || site.Source != model.LibraryKindSourceAuto {
		t.Fatalf("site decision = %#v", site)
	}

	ambiguous := ClassifyLibrary(ClassificationInput{URL: "https://example.com/docs/getting-started", URLMetadata: urlmeta.ClassifyURL("https://example.com/docs/getting-started")})
	if !ambiguous.Ambiguous || !ambiguous.NeedsReview || ambiguous.Kind != "" {
		t.Fatalf("ambiguous decision = %#v", ambiguous)
	}
}
