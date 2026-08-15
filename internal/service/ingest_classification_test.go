package service

import (
	"testing"

	"webtag/internal/dto"
	"webtag/internal/model"
)

func TestCaptureSignalsFromMetadataUsesOnlyAllowListedStructuralValues(t *testing.T) {
	signals := captureSignalsFromMetadata(map[string]any{
		"og_type": "article", "jsonld_types": "Article, WebApplication", "has_web_app_manifest": "true",
		"body": "must not be read", "description": "also not a classifier input",
	})
	if signals.OGType != "article" || len(signals.JSONLDTypes) != 2 || !signals.HasWebAppManifest {
		t.Fatalf("signals = %#v", signals)
	}
	if signals.SiteDescription || signals.ProseDominant {
		t.Fatalf("unexpected signal from arbitrary metadata: %#v", signals)
	}
}

func TestNormalizeIngestRequestPersistsAutomaticPredictionWithoutFinalizingKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		url      string
		metadata map[string]any
		want     model.LibraryKind
	}{
		{
			name: "web application is predicted as a site",
			url:  "https://example.com/product",
			metadata: map[string]any{
				"jsonld_types": "WebApplication",
			},
			want: model.LibraryKindSite,
		},
		{
			name: "article signals are predicted as a reading",
			url:  "https://example.com/notes/a-long-article",
			metadata: map[string]any{
				"og_type":                  "article",
				"has_author_and_published": "true",
				"prose_dominant":           "true",
			},
			want: model.LibraryKindReading,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalized, err := normalizeIngestRequest(dto.IngestRequest{Sources: []dto.IngestSource{{
				Kind: "browser_capture", URL: tt.url, Metadata: tt.metadata,
			}}})
			if err != nil {
				t.Fatalf("normalizeIngestRequest() error = %v", err)
			}
			params, err := normalized.toLinkCapture()
			if err != nil {
				t.Fatalf("toCreateLinkParams() error = %v", err)
			}
			if params.PredictedLibraryKind == nil || *params.PredictedLibraryKind != tt.want {
				t.Fatalf("PredictedLibraryKind = %v, want %q", params.PredictedLibraryKind, tt.want)
			}
			if params.RequestedLibraryKind != model.RequestedLibraryKindAuto {
				t.Fatalf("RequestedLibraryKind = %q, want auto", params.RequestedLibraryKind)
			}
		})
	}
}

func TestNormalizeIngestRequestDoesNotClassifyArbitraryMetadata(t *testing.T) {
	t.Parallel()

	normalized, err := normalizeIngestRequest(dto.IngestRequest{Sources: []dto.IngestSource{{
		Kind: "browser_capture", URL: "https://example.com/product",
		Metadata: map[string]any{
			"body":        "Article WebApplication prose application article",
			"description": "This must not become classifier evidence",
			"anything":    "WebApplication",
		},
	}}})
	if err != nil {
		t.Fatalf("normalizeIngestRequest() error = %v", err)
	}
	params, err := normalized.toLinkCapture()
	if err != nil {
		t.Fatalf("toCreateLinkParams() error = %v", err)
	}
	if params.PredictedLibraryKind != nil {
		t.Fatalf("PredictedLibraryKind = %q, want nil for arbitrary metadata", *params.PredictedLibraryKind)
	}
}
