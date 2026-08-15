package service

import (
	"testing"

	"webtag/internal/model"
)

func TestIngestContentPrefersReadableTextOverRawHTML(t *testing.T) {
	t.Parallel()

	text := "Captured readable article body"
	html := "<html><body><p>Duplicated raw article body</p></body></html>"
	content := ingestContent(model.Link{
		URL:        "https://example.com/article",
		SourceKind: "browser_capture",
		InputText:  &text,
		InputHTML:  &html,
	})

	if content.Body != text {
		t.Fatalf("Body = %q, want readable text only", content.Body)
	}
	if content.HTML != html {
		t.Fatalf("HTML = %q, want retained compatibility fallback", content.HTML)
	}
}

func TestIngestContentUsesHTMLWhenReadableTextIsAbsent(t *testing.T) {
	t.Parallel()

	html := "<main>HTML-only ingest</main>"
	content := ingestContent(model.Link{
		URL:        "webtag://ingest/html-only",
		SourceKind: "text",
		InputHTML:  &html,
	})

	if content.Body != html {
		t.Fatalf("Body = %q, want HTML fallback", content.Body)
	}
}
