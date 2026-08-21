//go:build integration

// integration_test.go exercises every fetcher (and the Manager fallback chain)
// against real upstream services. These tests are gated behind the
// `integration` build tag so default `go test ./...` runs stay offline and
// deterministic.
//
// Run with:
//
//	go test -tags=integration -timeout=120s ./internal/fetcher/...
//
// Optional environment variables:
//
//	WEBTAG_TEST_GITHUB_TOKEN — GitHub PAT to avoid 60/h anonymous rate limit
//	WEBTAG_TEST_SKIP_JINA    — set to "1" to skip Jina (r.jina.ai) probes
//
// Each test asserts the *shape* of a successful fetch (FetcherType, Title /
// Body presence, key metadata) without pinning brittle exact strings —
// upstream services occasionally tweak copy and we don't want spurious
// failures.

package fetcher

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// integrationDeadline is the per-fetch wall-clock budget. Jina cold starts can
// take more than 20 seconds in practice.
const integrationDeadline = 45 * time.Second

func integrationContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), integrationDeadline)
}

func skipIfEnv(t *testing.T, key, reason string) {
	t.Helper()
	if os.Getenv(key) == "1" {
		t.Skipf("%s=1; %s", key, reason)
	}
}

func newIntegrationClient() *HTTPClient {
	// Default HTTPClient already enforces SSRF guards. Real public URLs pass
	// through fine; localhost / RFC1918 fixtures would be rejected, which is
	// the desired posture for live tests.
	return NewHTTPClient(nil)
}

// ---------------------------------------------------------------------------
// Per-fetcher live probes
// ---------------------------------------------------------------------------

func TestIntegrationBasicFetcherV2EXExcludesReplies(t *testing.T) {
	ctx, cancel := integrationContext(t)
	defer cancel()

	liveClient := &http.Client{
		Transport: http.DefaultTransport.(*http.Transport).Clone(),
		Timeout:   integrationDeadline,
	}
	fetcher := NewBasicFetcher(NewHTTPClient(liveClient))
	fetcher.Timeout = integrationDeadline
	content, err := fetcher.Fetch(ctx, "https://v2ex.com/t/1224558")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	for _, want := range []string{"今年年初", "大量核心代码", "问问各位大神"} {
		if !strings.Contains(content.Body, want) {
			t.Errorf("Body missing original-post text %q", want)
		}
	}
	for _, unwanted := range []string{"ä»Šå¹´", "icanfork", "196 replies"} {
		if strings.Contains(content.Body, unwanted) || strings.Contains(content.HTML, unwanted) {
			t.Errorf("extracted V2EX content contains %q", unwanted)
		}
	}
}

func TestIntegrationArxivFetcher(t *testing.T) {
	t.Parallel()

	ctx, cancel := integrationContext(t)
	defer cancel()

	fetcher := NewArxivFetcher(newIntegrationClient())
	const url = "https://arxiv.org/abs/1706.03762" // "Attention Is All You Need"

	if !fetcher.CanHandle(url) {
		t.Fatalf("ArxivFetcher.CanHandle(%q) = false, want true", url)
	}

	content, err := fetcher.Fetch(ctx, url)
	if err != nil {
		t.Fatalf("ArxivFetcher.Fetch error: %v", err)
	}

	if content.FetcherType != "arxiv" {
		t.Errorf("FetcherType = %q, want %q", content.FetcherType, "arxiv")
	}
	if content.Title == "" {
		t.Error("Title is empty")
	}
	if !strings.Contains(strings.ToLower(content.Title), "attention") {
		t.Errorf("Title does not contain 'attention': %q", content.Title)
	}
	if len(content.Body) < 200 {
		t.Errorf("Body too short (%d chars): %q", len(content.Body), content.Body)
	}
	if id, _ := content.Metadata["arxiv_id"].(string); id == "" {
		t.Errorf("metadata.arxiv_id missing; metadata=%+v", content.Metadata)
	}
	if authors, _ := content.Metadata["authors"].([]string); len(authors) == 0 {
		t.Errorf("metadata.authors missing; metadata=%+v", content.Metadata)
	}
	t.Logf("arxiv → title=%q body_len=%d meta_keys=%v", content.Title, len(content.Body), keys(content.Metadata))
}

func TestIntegrationGitHubFetcher(t *testing.T) {
	t.Parallel()

	ctx, cancel := integrationContext(t)
	defer cancel()

	token := strings.TrimSpace(os.Getenv("WEBTAG_TEST_GITHUB_TOKEN"))
	fetcher := NewGitHubFetcher(newIntegrationClient(), token)
	const url = "https://github.com/golang/go"

	if !fetcher.CanHandle(url) {
		t.Fatalf("GitHubFetcher.CanHandle(%q) = false, want true", url)
	}

	content, err := fetcher.Fetch(ctx, url)
	if err != nil {
		// Anonymous GitHub gets 60 req/h. Treat rate-limit as a soft skip so a
		// flaky shared IP doesn't fail the suite — the test still surfaces it.
		if strings.Contains(err.Error(), "rate limited") && token == "" {
			t.Skipf("GitHub rate-limited and no WEBTAG_TEST_GITHUB_TOKEN provided: %v", err)
		}
		t.Fatalf("GitHubFetcher.Fetch error: %v", err)
	}

	if content.FetcherType != "github" {
		t.Errorf("FetcherType = %q, want %q", content.FetcherType, "github")
	}
	if !strings.EqualFold(content.Title, "golang/go") {
		t.Errorf("Title = %q, want %q (case-insensitive)", content.Title, "golang/go")
	}
	if !strings.Contains(content.Body, "# golang/go") {
		t.Errorf("Body missing '# golang/go' header: first200=%q", head(content.Body, 200))
	}
	if !strings.Contains(content.Body, "Stars:") {
		t.Errorf("Body missing 'Stars:' line: first200=%q", head(content.Body, 200))
	}
	if lang, _ := content.Metadata["language"].(string); lang == "" {
		t.Errorf("metadata.language missing; metadata=%+v", content.Metadata)
	}
	t.Logf("github → title=%q body_len=%d stars=%v", content.Title, len(content.Body), content.Metadata["stars"])
}

func TestIntegrationBasicFetcher(t *testing.T) {
	t.Parallel()

	ctx, cancel := integrationContext(t)
	defer cancel()

	fetcher := NewBasicFetcher(newIntegrationClient())
	// example.com is the canonical "stable boring HTML page" — IANA promises
	// to keep it serving a known shape forever.
	const url = "https://example.com/"

	if !fetcher.CanHandle(url) {
		t.Fatalf("BasicFetcher.CanHandle(%q) = false, want true", url)
	}

	content, err := fetcher.Fetch(ctx, url)
	if err != nil {
		t.Fatalf("BasicFetcher.Fetch error: %v", err)
	}

	if content.FetcherType != "basic" {
		t.Errorf("FetcherType = %q, want %q", content.FetcherType, "basic")
	}
	if content.Title == "" {
		t.Error("Title is empty")
	}
	if !strings.Contains(strings.ToLower(content.Title), "example") {
		t.Errorf("Title does not look like example.com: %q", content.Title)
	}
	if len(content.Body) == 0 {
		t.Error("Body is empty")
	}
	t.Logf("basic → title=%q body_len=%d", content.Title, len(content.Body))
}

func TestIntegrationPDFFetcher(t *testing.T) {
	t.Parallel()

	ctx, cancel := integrationContext(t)
	defer cancel()

	fetcher := NewPDFFetcher(newIntegrationClient())
	// Mozilla's pdf.js demo bundle: small, stable, served as application/pdf
	// over CDN with predictable text content.
	const url = "https://mozilla.github.io/pdf.js/web/compressed.tracemonkey-pldi-09.pdf"

	if !fetcher.CanHandle(url) {
		t.Fatalf("PDFFetcher.CanHandle(%q) = false, want true", url)
	}

	content, err := fetcher.Fetch(ctx, url)
	if err != nil {
		t.Fatalf("PDFFetcher.Fetch error: %v", err)
	}

	if content.FetcherType != "pdf" {
		t.Errorf("FetcherType = %q, want %q", content.FetcherType, "pdf")
	}
	if content.Title == "" {
		t.Error("Title is empty (should fall back to filename)")
	}
	if len(content.Body) == 0 {
		t.Error("Body is empty (PDF text extraction failed)")
	}
	t.Logf("pdf → title=%q body_len=%d", content.Title, len(content.Body))
}

func TestIntegrationJinaFetcher(t *testing.T) {
	t.Parallel()
	skipIfEnv(t, "WEBTAG_TEST_SKIP_JINA", "skipping Jina integration probe")

	ctx, cancel := integrationContext(t)
	defer cancel()

	fetcher := NewJinaFetcher(newIntegrationClient())
	// example.com — IANA-guaranteed stable HTML; Jina Reader handles it
	// reliably and the response shape never changes.
	const url = "https://example.com/"

	content, err := fetcher.Fetch(ctx, url)
	if err != nil {
		t.Fatalf("JinaFetcher.Fetch error: %v", err)
	}

	if content.FetcherType != "jina" {
		t.Errorf("FetcherType = %q, want %q", content.FetcherType, "jina")
	}
	if content.Body == "" {
		t.Error("Body is empty")
	}
	t.Logf("jina → title=%q body_len=%d", content.Title, len(content.Body))
}

// ---------------------------------------------------------------------------
// Manager end-to-end
// ---------------------------------------------------------------------------

func TestIntegrationManagerRouting(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		url         string
		wantFetcher string
	}{
		{name: "arxiv_abs", url: "https://arxiv.org/abs/1706.03762", wantFetcher: "arxiv"},
		{name: "github_repo", url: "https://github.com/golang/go", wantFetcher: "github"},
		{name: "plain_html", url: "https://example.com/", wantFetcher: "basic"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			githubToken := strings.TrimSpace(os.Getenv("WEBTAG_TEST_GITHUB_TOKEN"))

			manager := NewDefaultManager(newIntegrationClient(), githubToken)

			ctx, cancel := integrationContext(t)
			defer cancel()

			content, err := manager.Fetch(ctx, tc.url)
			if err != nil {
				if tc.wantFetcher == "github" && strings.Contains(err.Error(), "rate limited") && githubToken == "" {
					t.Skipf("GitHub rate-limited; supply WEBTAG_TEST_GITHUB_TOKEN to test reliably: %v", err)
				}
				t.Fatalf("Manager.Fetch(%s) error: %v", tc.url, err)
			}

			// Manager may suffix "+thin"; accept exact match or prefix-with-plus.
			if !fetcherTypeMatches(content.FetcherType, tc.wantFetcher) {
				t.Errorf("FetcherType = %q, want %q (or %q+...)", content.FetcherType, tc.wantFetcher, tc.wantFetcher)
			}
			if content.Title == "" {
				t.Error("Title is empty")
			}
			if strings.TrimSpace(content.Body) == "" {
				t.Error("Body is empty")
			}
			t.Logf("%s → fetcher=%s title=%q body_len=%d", tc.name, content.FetcherType, content.Title, len(content.Body))
		})
	}
}

// fetcherTypeMatches accepts either the exact fetcher name or a "name+suffix"
// composition like "basic+thin" used when Manager records a
// degraded path. We deliberately do *not* accept arbitrary substrings to keep
// the assertion meaningful.
func fetcherTypeMatches(actual, want string) bool {
	if actual == want {
		return true
	}
	if strings.HasPrefix(actual, want+"+") {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func keys(m map[string]any) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
