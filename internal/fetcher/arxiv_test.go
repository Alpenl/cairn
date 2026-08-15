package fetcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

const arxivSampleAtomFeed = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:arxiv="http://arxiv.org/schemas/atom">
  <title type="html">ArXiv Query: search_query=&amp;id_list=2401.12345</title>
  <id>http://arxiv.org/api/query?id_list=2401.12345</id>
  <updated>2024-02-01T00:00:00Z</updated>
  <entry>
    <id>http://arxiv.org/abs/2401.12345v2</id>
    <updated>2024-02-01T00:00:00Z</updated>
    <published>2024-01-15T12:34:56Z</published>
    <title>A Study of Foo Bar Baz</title>
    <summary>这是一篇关于 Foo Bar 的论文摘要。涵盖核心方法和实验结果。</summary>
    <author><name>Alice Example</name></author>
    <author><name>Bob Sample</name></author>
    <link href="http://arxiv.org/abs/2401.12345v2" rel="alternate" type="text/html"/>
    <link title="pdf" rel="related" href="http://arxiv.org/pdf/2401.12345v2" type="application/pdf"/>
    <arxiv:primary_category xmlns:arxiv="http://arxiv.org/schemas/atom" term="cs.LG" scheme="http://arxiv.org/schemas/atom"/>
    <arxiv:doi>10.1234/foobar.2024.12345</arxiv:doi>
    <arxiv:journal_ref>Journal of Foo, vol. 1, 2024</arxiv:journal_ref>
  </entry>
</feed>`

const arxivEmptyAtomFeed = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>ArXiv Query</title>
  <id>http://arxiv.org/api/query?id_list=9999.99999</id>
  <updated>2024-02-01T00:00:00Z</updated>
</feed>`

func TestArxivFetcherCanHandle(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"abs new id", "https://arxiv.org/abs/2401.12345", true},
		{"abs new id versioned", "https://arxiv.org/abs/2401.12345v2", true},
		{"abs legacy id", "https://arxiv.org/abs/cs.AI/0703001", true},
		{"pdf with extension", "https://arxiv.org/pdf/2401.12345.pdf", true},
		{"pdf versioned with extension", "https://arxiv.org/pdf/2401.12345v2.pdf", true},
		{"pdf without extension", "https://arxiv.org/pdf/2401.12345v2", true},
		{"html variant", "https://arxiv.org/html/2401.12345v2", true},
		{"ps variant", "https://arxiv.org/ps/2401.12345", true},
		{"export host", "https://export.arxiv.org/abs/2401.12345", true},
		{"www host", "https://www.arxiv.org/abs/2401.12345", true},
		{"with query", "https://arxiv.org/abs/2401.12345?context=cs.LG", true},
		{"with fragment", "https://arxiv.org/abs/2401.12345#section.1", true},

		{"github url", "https://github.com/example/project", false},
		{"example url", "https://example.com/abs/2401.12345", false},
		{"arxiv homepage", "https://arxiv.org/", false},
		{"arxiv list", "https://arxiv.org/list/cs.LG/24", false},
		{"missing id", "https://arxiv.org/abs/", false},
	}

	fetcher := NewArxivFetcher(nil)
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := fetcher.CanHandle(c.url)
			if got != c.want {
				t.Fatalf("CanHandle(%q) = %v, want %v", c.url, got, c.want)
			}
		})
	}
}

func TestArxivFetcherFetchHappyPath(t *testing.T) {
	t.Parallel()

	var capturedQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
		_, _ = w.Write([]byte(arxivSampleAtomFeed))
	}))
	defer server.Close()

	fetcher := NewArxivFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	fetcher.APIBaseURL = server.URL

	got, err := fetcher.Fetch(context.Background(), "https://arxiv.org/abs/2401.12345v2")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	if got.FetcherType != "arxiv" {
		t.Fatalf("FetcherType = %q, want arxiv", got.FetcherType)
	}
	if got.Title != "A Study of Foo Bar Baz" {
		t.Fatalf("Title = %q, want %q", got.Title, "A Study of Foo Bar Baz")
	}
	if !strings.Contains(got.Body, "Foo Bar") {
		t.Fatalf("Body = %q, want substring %q", got.Body, "Foo Bar")
	}
	if !utf8.ValidString(got.Body) {
		t.Fatalf("Body = %q, want valid UTF-8", got.Body)
	}
	if !strings.Contains(capturedQuery, "id_list=2401.12345v2") {
		t.Fatalf("captured query = %q, want id_list=2401.12345v2", capturedQuery)
	}

	if id, ok := got.Metadata["arxiv_id"].(string); !ok || id != "2401.12345v2" {
		t.Fatalf("Metadata[arxiv_id] = %v, want %q", got.Metadata["arxiv_id"], "2401.12345v2")
	}
	authors, ok := got.Metadata["authors"].([]string)
	if !ok || len(authors) != 2 || authors[0] != "Alice Example" || authors[1] != "Bob Sample" {
		t.Fatalf("Metadata[authors] = %v, want [Alice Example Bob Sample]", got.Metadata["authors"])
	}
	if cat, ok := got.Metadata["primary_category"].(string); !ok || cat != "cs.LG" {
		t.Fatalf("Metadata[primary_category] = %v, want cs.LG", got.Metadata["primary_category"])
	}
	if doi, ok := got.Metadata["doi"].(string); !ok || doi != "10.1234/foobar.2024.12345" {
		t.Fatalf("Metadata[doi] = %v, want 10.1234/foobar.2024.12345", got.Metadata["doi"])
	}
	if ref, ok := got.Metadata["journal_ref"].(string); !ok || ref != "Journal of Foo, vol. 1, 2024" {
		t.Fatalf("Metadata[journal_ref] = %v, want Journal of Foo, vol. 1, 2024", got.Metadata["journal_ref"])
	}
	if pub, ok := got.Metadata["published"].(string); !ok || pub != "2024-01-15T12:34:56Z" {
		t.Fatalf("Metadata[published] = %v, want 2024-01-15T12:34:56Z", got.Metadata["published"])
	}
	if pdfURL, ok := got.Metadata["pdf_url"].(string); !ok || pdfURL != "http://arxiv.org/pdf/2401.12345v2" {
		t.Fatalf("Metadata[pdf_url] = %v, want http://arxiv.org/pdf/2401.12345v2", got.Metadata["pdf_url"])
	}
	if htmlURL, ok := got.Metadata["html_url"].(string); !ok || htmlURL != "http://arxiv.org/abs/2401.12345v2" {
		t.Fatalf("Metadata[html_url] = %v, want http://arxiv.org/abs/2401.12345v2", got.Metadata["html_url"])
	}
}

func TestArxivFetcherFetchExtractsLegacyID(t *testing.T) {
	t.Parallel()

	var capturedQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(arxivSampleAtomFeed))
	}))
	defer server.Close()

	fetcher := NewArxivFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	fetcher.APIBaseURL = server.URL

	if _, err := fetcher.Fetch(context.Background(), "https://arxiv.org/abs/cs.AI/0703001"); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !strings.Contains(capturedQuery, "cs.AI") || !strings.Contains(capturedQuery, "0703001") {
		t.Fatalf("captured query = %q, want id_list to include cs.AI/0703001", capturedQuery)
	}
}

func TestArxivFetcherErrorBranches(t *testing.T) {
	t.Parallel()

	type serverBehavior func(w http.ResponseWriter, r *http.Request)

	cases := []struct {
		name          string
		handler       serverBehavior
		wantSubstring string
	}{
		{
			name: "404 not found",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.NotFound(w, r)
			},
			wantSubstring: "not found",
		},
		{
			name: "500 server error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantSubstring: "HTTP 500",
		},
		{
			name: "non-xml content type",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("not xml"))
			},
			wantSubstring: "content-type",
		},
		{
			name: "empty entries",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/atom+xml")
				_, _ = w.Write([]byte(arxivEmptyAtomFeed))
			},
			wantSubstring: "no entries",
		},
		{
			name: "malformed xml",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/atom+xml")
				_, _ = w.Write([]byte("<feed><entry><title>oops"))
			},
			wantSubstring: "decode",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(c.handler))
			defer server.Close()

			fetcher := NewArxivFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
			fetcher.APIBaseURL = server.URL

			_, err := fetcher.Fetch(context.Background(), "https://arxiv.org/abs/2401.12345")
			if err == nil {
				t.Fatalf("Fetch() error = nil, want error containing %q", c.wantSubstring)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.wantSubstring)) {
				t.Fatalf("Fetch() error = %v, want substring %q", err, c.wantSubstring)
			}
		})
	}
}

func TestArxivFetcherUnknownURLReturnsError(t *testing.T) {
	t.Parallel()

	fetcher := NewArxivFetcher(NewHTTPClientWithOptions(HTTPClientOptions{AllowUnsafeTargets: true}))
	if _, err := fetcher.Fetch(context.Background(), "https://example.com/abs/2401.12345"); err == nil {
		t.Fatalf("Fetch() error = nil, want error for non-arxiv URL")
	}
}

func TestArxivFetcherEntryWithoutTitleErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <summary>only summary, no title</summary>
  </entry>
</feed>`))
	}))
	defer server.Close()

	fetcher := NewArxivFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	fetcher.APIBaseURL = server.URL

	_, err := fetcher.Fetch(context.Background(), "https://arxiv.org/abs/2401.12345")
	if err == nil {
		t.Fatalf("Fetch() error = nil, want missing title error")
	}
	if !strings.Contains(err.Error(), "missing title") {
		t.Fatalf("Fetch() error = %v, want missing title", err)
	}
}
