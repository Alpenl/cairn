package service

import (
	"testing"

	"webtag/internal/dto"
	"webtag/internal/model"
)

func TestBrowserCaptureIdentityDoesNotDependOnSourceOrder(t *testing.T) {
	t.Parallel()

	const (
		pageURL         = "https://example.com/article"
		supplementalURL = "https://source.example.com/reference"
	)
	browserCapture := dto.IngestSource{
		Kind:  "browser_capture",
		URL:   pageURL,
		Title: "Article",
		Text:  "Captured body",
	}
	reference := dto.IngestSource{Kind: "url", URL: supplementalURL}

	for _, sources := range [][]dto.IngestSource{
		{browserCapture, reference},
		{reference, browserCapture},
	} {
		normalized, err := normalizeIngestRequest(dto.IngestRequest{Sources: sources})
		if err != nil {
			t.Fatalf("normalizeIngestRequest() error = %v", err)
		}
		if normalized.sourceKey != pageURL {
			t.Fatalf("sourceKey = %q, want browser capture page URL %q", normalized.sourceKey, pageURL)
		}
		if normalized.storedURL != pageURL || normalized.realURL != pageURL || normalized.lockURL != pageURL {
			t.Fatalf(
				"storedURL/realURL/lockURL = %q/%q/%q, want browser capture page URL %q",
				normalized.storedURL,
				normalized.realURL,
				normalized.lockURL,
				pageURL,
			)
		}
	}
}

func TestBrowserCaptureFingerprintTracksSupplementalSourceIdentity(t *testing.T) {
	t.Parallel()

	const (
		pageURL    = "https://example.com/article"
		referenceA = "https://source.example.com/reference-a"
		referenceB = "https://source.example.com/reference-b"
	)
	normalize := func(capturedAt, reference string, referenceFirst bool) normalizedIngest {
		t.Helper()
		capture := dto.IngestSource{
			Kind:  "browser_capture",
			URL:   pageURL,
			Title: "Article",
			Text:  "Captured body",
			Metadata: map[string]any{
				"captured_at": capturedAt,
			},
		}
		extra := dto.IngestSource{Kind: "url", URL: reference}
		sources := []dto.IngestSource{capture, extra}
		if referenceFirst {
			sources = []dto.IngestSource{extra, capture}
		}
		got, err := normalizeIngestRequest(dto.IngestRequest{Sources: sources})
		if err != nil {
			t.Fatalf("normalizeIngestRequest() error = %v", err)
		}
		return got
	}

	first := normalize("2026-07-11T10:00:00Z", referenceA, false)
	sameIdentity := normalize("2026-07-11T11:00:00Z", referenceA, true)
	changedIdentity := normalize("2026-07-11T12:00:00Z", referenceB, false)

	firstFingerprint, ok := first.sourceMetadata[captureSourceFingerprintMetadataKey].(string)
	if !ok || firstFingerprint == "" {
		t.Fatalf("capture source fingerprint = %#v, want non-empty string", first.sourceMetadata[captureSourceFingerprintMetadataKey])
	}
	if got := sameIdentity.sourceMetadata[captureSourceFingerprintMetadataKey]; got != firstFingerprint {
		t.Fatalf("same source identity fingerprint = %#v, want %q", got, firstFingerprint)
	}
	if got := changedIdentity.sourceMetadata[captureSourceFingerprintMetadataKey]; got == firstFingerprint {
		t.Fatalf("changed source identity fingerprint = %#v, want value different from %q", got, firstFingerprint)
	}
}

func TestCaptureChangedUsesSupplementalSourceFingerprint(t *testing.T) {
	t.Parallel()

	const (
		pageURL    = "https://example.com/article"
		referenceA = "https://source.example.com/reference-a"
		referenceB = "https://source.example.com/reference-b"
	)
	normalizeParams := func(capturedAt, reference string, referenceFirst bool) LinkCapture {
		t.Helper()
		capture := dto.IngestSource{
			Kind:  "browser_capture",
			URL:   pageURL,
			Title: "Article",
			Text:  "Captured body",
			Metadata: map[string]any{
				"captured_at": capturedAt,
			},
		}
		extra := dto.IngestSource{Kind: "url", URL: reference}
		sources := []dto.IngestSource{capture, extra}
		if referenceFirst {
			sources = []dto.IngestSource{extra, capture}
		}
		normalized, err := normalizeIngestRequest(dto.IngestRequest{Sources: sources})
		if err != nil {
			t.Fatalf("normalizeIngestRequest() error = %v", err)
		}
		params, err := normalized.toLinkCapture()
		if err != nil {
			t.Fatalf("toCreateLinkParams() error = %v", err)
		}
		return params
	}

	old := normalizeParams("2026-07-11T10:00:00Z", referenceA, false)
	sameIdentity := normalizeParams("2026-07-11T11:00:00Z", referenceA, true)
	changedIdentity := normalizeParams("2026-07-11T12:00:00Z", referenceB, false)
	link := &model.Link{
		URL:            old.URL,
		SourceKind:     old.SourceKind,
		SourceKey:      old.SourceKey,
		InputTitle:     old.InputTitle,
		InputText:      old.InputText,
		InputHTML:      old.InputHTML,
		InputImages:    old.InputImages,
		SourceMetadata: old.SourceMetadata,
		Description:    old.Description,
	}

	if captureChanged(link, sameIdentity) {
		t.Fatal("captureChanged() = true for same supplemental identity with reordered records and new captured_at")
	}
	if !captureChanged(link, changedIdentity) {
		t.Fatal("captureChanged() = false after supplemental URL identity changed")
	}
}
