package urlmeta

import (
	"testing"

	"webtag/internal/model"
)

func TestClassifyURLMatchesCurrentHeuristics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want URLMetadata
	}{
		{
			name: "homepage",
			url:  "https://example.com/",
			want: URLMetadata{
				Domain:      "example.com",
				ContentType: model.ContentTypeHomepage,
				PathDepth:   0,
				ParentPath:  "/",
			},
		},
		{
			name: "numeric article",
			url:  "https://example.com/posts/12345",
			want: URLMetadata{
				Domain:      "example.com",
				ContentType: model.ContentTypeArticle,
				PathDepth:   2,
				ParentPath:  "/posts/",
			},
		},
		{
			name: "dated article",
			url:  "https://example.com/blog/2024/05/the-future-of-web-crawling",
			want: URLMetadata{
				Domain:      "example.com",
				ContentType: model.ContentTypeArticle,
				PathDepth:   4,
				ParentPath:  "/blog/2024/05/",
			},
		},
		{
			name: "listing keyword",
			url:  "https://example.com/articles/",
			want: URLMetadata{
				Domain:      "example.com",
				ContentType: model.ContentTypeListing,
				PathDepth:   1,
				ParentPath:  "/",
			},
		},
		{
			name: "unknown leaf",
			url:  "https://example.com/about",
			want: URLMetadata{
				Domain:      "example.com",
				ContentType: model.ContentTypeUnknown,
				PathDepth:   1,
				ParentPath:  "/",
			},
		},
		{
			name: "domain preserves port",
			url:  "https://example.com:8443/posts/12345",
			want: URLMetadata{
				Domain:      "example.com:8443",
				ContentType: model.ContentTypeArticle,
				PathDepth:   2,
				ParentPath:  "/posts/",
			},
		},
		{
			name: "domain preserves userinfo and port",
			url:  "https://user:pass@example.com:8443/posts/12345",
			want: URLMetadata{
				Domain:      "user:pass@example.com:8443",
				ContentType: model.ContentTypeArticle,
				PathDepth:   2,
				ParentPath:  "/posts/",
			},
		},
		{
			name: "malformed percent path still classifies",
			url:  "https://example.com/%zz",
			want: URLMetadata{
				Domain:      "example.com",
				ContentType: model.ContentTypeUnknown,
				PathDepth:   1,
				ParentPath:  "/",
			},
		},
		{
			name: "empty url",
			url:  "",
			want: URLMetadata{
				Domain:      "",
				ContentType: model.ContentTypeUnknown,
				PathDepth:   0,
				ParentPath:  "/",
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ClassifyURL(tt.url)
			if got != tt.want {
				t.Fatalf("ClassifyURL(%q) = %#v, want %#v", tt.url, got, tt.want)
			}
		})
	}
}

func TestClassifyProductionContentShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want model.ContentType
	}{
		{
			name: "social post is an article",
			url:  "https://x.com/GitHub_Daily/status/2073708506319098344",
			want: model.ContentTypeArticle,
		},
		{
			name: "ordinary blog slug is an article",
			url:  "https://www.seangoedecke.com/good-api-design/",
			want: model.ContentTypeArticle,
		},
		{
			name: "github newsletter issue is a listing",
			url:  "https://github.com/ruanyf/weekly/blob/master/docs/issue-402.md",
			want: model.ContentTypeListing,
		},
		{
			name: "go blog slug is an article",
			url:  "https://go.dev/blog/error-syntax",
			want: model.ContentTypeArticle,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyURL(tt.url).ContentType; got != tt.want {
				t.Fatalf("ClassifyURL(%q).ContentType = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestAncestorURLsReturnsRootThroughDirectParent(t *testing.T) {
	t.Parallel()

	got := AncestorURLs("https://example.com/a/b/c/", 10)
	want := []string{
		"https://example.com/",
		"https://example.com/a/",
		"https://example.com/a/b/",
	}

	assertStringSliceEqual(t, got, want)
}

func TestAncestorURLsPreserveUserinfoAndMalformedPaths(t *testing.T) {
	t.Parallel()

	got := AncestorURLs("https://user:pass@example.com:8443/%zz/a/", 10)
	want := []string{
		"https://user:pass@example.com:8443/",
		"https://user:pass@example.com:8443/%zz/",
	}

	assertStringSliceEqual(t, got, want)
}

func TestAncestorURLsRespectsMaxDepth(t *testing.T) {
	t.Parallel()

	got := AncestorURLs("https://example.com/a/b/c/d/", 2)
	want := []string{
		"https://example.com/",
		"https://example.com/a/",
	}

	assertStringSliceEqual(t, got, want)
}

func TestAncestorURLsRejectsURLsWithoutHost(t *testing.T) {
	t.Parallel()

	got := AncestorURLs("/relative/path", 5)
	if len(got) != 0 {
		t.Fatalf("AncestorURLs() = %#v, want empty", got)
	}
}

func assertStringSliceEqual(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d; got=%#v want=%#v", len(got), len(want), got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d = %q, want %q; got=%#v want=%#v", i, got[i], want[i], got, want)
		}
	}
}
