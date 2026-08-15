package fetcher

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// longHTML is intentionally larger than defaultLightMaxChars so that
// truncation logic gets exercised. Body is set up so that key tag
// keywords (Project / FrameworkX) appear early — within the first 200
// chars of extracted text — to mirror the real-world tag-precision
// pattern.
var longHTML = `<!DOCTYPE html>
<html>
<head>
  <meta property="og:title" content="Project announcement: open-sourcing FrameworkX">
  <meta property="og:description" content="Today we announce Project, our open-source FrameworkX for knowledge bases.">
  <title>Project announcement</title>
</head>
<body>
  <article>
    <h1>Project announcement: open-sourcing FrameworkX</h1>
    <p>We are pleased to announce Project today. The codebase, called FrameworkX,
    has been released under an open-source license. It is built around a RAG
    architecture with first-class support for multi-format document ingestion.
    Below we cover the motivation, the technical design, and how to deploy
    it in a private environment.</p>
    <p>This is the second paragraph, which expands on each pillar with
    examples and code snippets that a tagger would consider lower-signal —
    deployment commands, environment variables, performance numbers,
    benchmarks, and a long appendix.` + strings.Repeat(" Filler content. Filler content.", 500) + `</p>
  </article>
</body>
</html>`

func TestLightFetcherCanHandle(t *testing.T) {
	f := NewLightFetcher(nil)
	for _, url := range []string{
		"https://example.com/",
		"https://anything.test/path?q=1",
	} {
		if !f.CanHandle(url) {
			t.Errorf("CanHandle(%q) = false, want true (universal default)", url)
		}
	}
}

func TestLightFetcherTruncatesBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(longHTML))
	}))
	defer ts.Close()

	client := NewHTTPClientWithOptions(HTTPClientOptions{Client: ts.Client(), AllowUnsafeTargets: true})
	f := NewLightFetcher(client)
	f.MaxChars = 200 // tighter cap for the test

	content, err := f.Fetch(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if content.FetcherType != "light" {
		t.Errorf("FetcherType = %q, want %q", content.FetcherType, "light")
	}
	got := utf8.RuneCountInString(content.Body)
	if got > 200 {
		t.Errorf("body length = %d runes, want ≤ 200 (truncation failed)", got)
	}
	// Tag-relevant keywords MUST survive truncation — they appear in the
	// first paragraph by construction.
	for _, kw := range []string{"Project", "FrameworkX"} {
		if !strings.Contains(content.Body, kw) {
			t.Errorf("truncated body missing keyword %q; got %q", kw, content.Body)
		}
	}
	if content.Title == "" {
		t.Error("title is empty; expected og:title or fallback")
	}
}

func TestLightFetcherSendsRangeHeader(t *testing.T) {
	var gotRange string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(longHTML))
	}))
	defer ts.Close()

	client := NewHTTPClientWithOptions(HTTPClientOptions{Client: ts.Client(), AllowUnsafeTargets: true})
	f := NewLightFetcher(client)
	f.MaxBytes = 4096

	if _, err := f.Fetch(context.Background(), ts.URL); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := fmt.Sprintf("bytes=0-%d", f.MaxBytes-1)
	if gotRange != want {
		t.Errorf("Range header = %q, want %q", gotRange, want)
	}
}

func TestLightFetcherAcceptsPartialContent(t *testing.T) {
	// Server that honours Range and returns 206.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Range", "bytes 0-1023/4096")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(longHTML[:1024]))
	}))
	defer ts.Close()

	client := NewHTTPClientWithOptions(HTTPClientOptions{Client: ts.Client(), AllowUnsafeTargets: true})
	f := NewLightFetcher(client)

	content, err := f.Fetch(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("Fetch returned error on 206: %v", err)
	}
	if content.Title == "" {
		t.Error("title empty on 206 response; readability should still extract from truncated head")
	}
}

func TestLightFetcherRejectsBadStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	client := NewHTTPClientWithOptions(HTTPClientOptions{Client: ts.Client(), AllowUnsafeTargets: true})
	f := NewLightFetcher(client)

	if _, err := f.Fetch(context.Background(), ts.URL); err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}

func TestLightFetcherRejectsBinaryContentType(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0, 1, 2, 3})
	}))
	defer ts.Close()

	client := NewHTTPClientWithOptions(HTTPClientOptions{Client: ts.Client(), AllowUnsafeTargets: true})
	f := NewLightFetcher(client)

	if _, err := f.Fetch(context.Background(), ts.URL); err == nil {
		t.Fatal("expected error on binary content-type, got nil")
	}
}

func TestLightFetcherEmptyExtractionFails(t *testing.T) {
	// Page with no text, no title, no meta — readability returns empty.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body></body></html>"))
	}))
	defer ts.Close()

	client := NewHTTPClientWithOptions(HTTPClientOptions{Client: ts.Client(), AllowUnsafeTargets: true})
	f := NewLightFetcher(client)

	if _, err := f.Fetch(context.Background(), ts.URL); err == nil {
		t.Fatal("expected error on empty extraction, got nil")
	}
}

func TestManagerFetchLightUsesLightFirst(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(longHTML))
	}))
	defer ts.Close()

	client := NewHTTPClientWithOptions(HTTPClientOptions{Client: ts.Client(), AllowUnsafeTargets: true})
	mgr := NewManager(NewRouter(), nil, nil, ManagerOptions{})
	mgr.SetLightFetcher(NewLightFetcher(client))

	content, err := mgr.FetchLight(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("FetchLight: %v", err)
	}
	if content.FetcherType != "light" {
		t.Errorf("FetcherType = %q, want %q (no jina fallback expected)", content.FetcherType, "light")
	}
}

func TestManagerFetchLightWithoutLightConfigured(t *testing.T) {
	mgr := NewManager(NewRouter(), nil, nil, ManagerOptions{})
	// Note: no SetLightFetcher call
	if _, err := mgr.FetchLight(context.Background(), "https://example.com/"); err == nil {
		t.Fatal("expected error when light fetcher not configured, got nil")
	}
}

// stubJina is a Fetcher used only by TestManagerFetchLightFallsBackToJina.
// It returns a body large enough to clearly exceed the truncation cap so
// the test can prove FetchLight applies MaxChars to jina output too.
type stubJina struct {
	body string
}

func (s *stubJina) CanHandle(string) bool { return true }
func (s *stubJina) Fetch(_ context.Context, url string) (Content, error) {
	return Content{
		URL:         url,
		Title:       "From Jina",
		Body:        s.body,
		FetcherType: "jina",
	}, nil
}

func TestManagerFetchLightFallsBackToJinaWhenLightEmpty(t *testing.T) {
	// LightFetcher pointed at a server that returns empty body — error
	// path. FetchLight should then ask jina.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body></body></html>"))
	}))
	defer ts.Close()

	client := NewHTTPClientWithOptions(HTTPClientOptions{Client: ts.Client(), AllowUnsafeTargets: true})
	longJinaBody := strings.Repeat("Jina-served content. ", 500) // ~10KB
	jina := &stubJina{body: longJinaBody}

	mgr := NewManager(NewRouter(), jina, nil, ManagerOptions{})
	light := NewLightFetcher(client)
	light.MaxChars = 150
	mgr.SetLightFetcher(light)

	content, err := mgr.FetchLight(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("FetchLight: %v", err)
	}
	if !strings.HasPrefix(content.FetcherType, "jina") {
		t.Errorf("FetcherType = %q, want jina-prefixed", content.FetcherType)
	}
	if got := utf8.RuneCountInString(content.Body); got > 150 {
		t.Errorf("jina body not truncated to MaxChars: got %d runes, want ≤ 150", got)
	}
}
