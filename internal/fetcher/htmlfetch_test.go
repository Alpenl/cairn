package fetcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const sampleArticleHTML = `<!DOCTYPE html>
<html>
<head>
  <meta property="og:title" content="Sample WeChat Article">
  <title>Default Title</title>
</head>
<body>
  <div id="js_content" class="rich_media_content">
    <p>This is the main body of a fake WeChat article used by the
    HostBoundHTMLFetcher tests. It is long enough that the readability
    extractor will pick it as the article body rather than rejecting
    it as boilerplate. The content needs at least a few sentences for
    readability to score it as the article core.</p>
    <p>A second paragraph adds more text so the extractor confidently
    selects this region. Without sufficient text content, readability
    falls back to other heuristics that may produce unexpected results.</p>
  </div>
</body>
</html>`

func TestHostBoundHTMLFetcherCanHandle(t *testing.T) {
	f := NewHostBoundHTMLFetcher(nil, HostBoundHTMLFetcherOptions{
		Hosts:      []string{"example.com"},
		FetcherTag: "test",
	})
	if !f.CanHandle("https://example.com/x") {
		t.Error("CanHandle(example.com) = false, want true")
	}
	if f.CanHandle("https://other.com/x") {
		t.Error("CanHandle(other.com) = true, want false (host not in list)")
	}
}

func TestHostBoundHTMLFetcherFetchesAndParses(t *testing.T) {
	var gotUA string
	var gotCustomHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotCustomHeader = r.Header.Get("X-Site-Header")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleArticleHTML))
	}))
	defer ts.Close()

	host := strings.TrimPrefix(ts.URL, "http://")
	client := NewHTTPClientWithOptions(HTTPClientOptions{Client: ts.Client(), allowUnsafeTargets: true})
	f := NewHostBoundHTMLFetcher(client, HostBoundHTMLFetcherOptions{
		Hosts:      []string{host},
		UserAgent:  "Test/1.0",
		Headers:    map[string]string{"X-Site-Header": "wechat-style"},
		FetcherTag: "test_html",
	})

	content, err := f.Fetch(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if content.FetcherType != "test_html" {
		t.Errorf("FetcherType = %q, want %q", content.FetcherType, "test_html")
	}
	if !strings.Contains(content.Body, "main body") {
		t.Errorf("Body missing expected text: %q", content.Body)
	}
	if content.Title == "" {
		t.Error("Title is empty, expected og:title or fallback")
	}
	if gotUA != "Test/1.0" {
		t.Errorf("upstream saw UA = %q, want %q", gotUA, "Test/1.0")
	}
	if gotCustomHeader != "wechat-style" {
		t.Errorf("upstream saw X-Site-Header = %q, want %q", gotCustomHeader, "wechat-style")
	}
}

func TestHostBoundHTMLFetcherRejectsNonMatchingHost(t *testing.T) {
	// CanHandle returns false → Router would never dispatch to us. But
	// if someone Fetches directly we just attempt it (matching basic.go's
	// behaviour); the test here is only that CanHandle filters.
	f := NewHostBoundHTMLFetcher(nil, HostBoundHTMLFetcherOptions{
		Hosts:      []string{"only-this.example"},
		FetcherTag: "test",
	})
	if f.CanHandle("https://elsewhere.example/x") {
		t.Error("CanHandle accepted non-matching host")
	}
}

func TestHostBoundHTMLFetcherAppliesRateLimit(t *testing.T) {
	// 1 request per very long window → the second call must wait beyond
	// the test's tight ctx and surface a "rate limit wait failed" error.
	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sampleArticleHTML))
	}))
	defer ts.Close()

	host := strings.TrimPrefix(ts.URL, "http://")
	client := NewHTTPClientWithOptions(HTTPClientOptions{Client: ts.Client(), allowUnsafeTargets: true})
	f := NewHostBoundHTMLFetcher(client, HostBoundHTMLFetcherOptions{
		Hosts:      []string{host},
		FetcherTag: "test_rl",
		Limiter:    NewOutboundLimiter(1, time.Hour),
	})

	if _, err := f.Fetch(context.Background(), ts.URL); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := f.Fetch(ctx, ts.URL)
	if err == nil {
		t.Fatal("second Fetch unexpectedly succeeded; rate limiter did not kick in")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("error %q does not mention rate limit", err.Error())
	}
	if hits != 1 {
		t.Errorf("upstream hit count = %d, want 1 (second request blocked at limiter)", hits)
	}
}

func TestHostBoundHTMLFetcherDefaults(t *testing.T) {
	f := NewHostBoundHTMLFetcher(nil, HostBoundHTMLFetcherOptions{
		Hosts: []string{"x.example"},
		// All other fields zero → defaults apply
	})
	if f.userAgent != defaultUserAgent {
		t.Errorf("default UA not applied: %q", f.userAgent)
	}
	if f.timeout != defaultBasicTimeout {
		t.Errorf("default timeout not applied: %v", f.timeout)
	}
	if f.maxBytes != defaultBasicMaxBytes {
		t.Errorf("default maxBytes not applied: %d", f.maxBytes)
	}
	if f.fetcherTag != "host_bound_html" {
		t.Errorf("default tag not applied: %q", f.fetcherTag)
	}
}
