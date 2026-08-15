package urlidentity_test

import (
	"errors"
	"strings"
	"testing"

	"webtag/internal/urlidentity"
)

// TestNormalizeRules is the executable statement of the frozen normalisation
// contract. Every case names the rule it pins so that a future edit that
// "simplifies" one rule fails with the rule's own name in the output.
func TestNormalizeRules(t *testing.T) {
	cases := []struct {
		rule string
		in   string
		want string
	}{
		// Scheme and host casing.
		{"scheme lowercased", "HTTPS://example.com/a", "https://example.com/a"},
		{"host lowercased", "https://EXAMPLE.COM/a", "https://example.com/a"},
		{"host case does not touch path case", "https://EXAMPLE.com/AbC", "https://example.com/AbC"},

		// Trailing host dot.
		{"trailing host dot removed", "https://example.com./a", "https://example.com/a"},
		{"trailing host dot with www", "https://www.example.com./a", "https://example.com/a"},

		// Leading www folding.
		{"leading www folded", "https://www.example.com/a", "https://example.com/a"},
		{"only one www label folded", "https://www.www.example.com/a", "https://www.example.com/a"},
		{"www inside host untouched", "https://a.www.example.com/", "https://a.www.example.com/"},
		{"host starting with www but no dot untouched", "https://wwwexample.com/", "https://wwwexample.com/"},

		// Default ports.
		{"http default port removed", "http://example.com:80/a", "http://example.com/a"},
		{"https default port removed", "https://example.com:443/a", "https://example.com/a"},
		{"http non-default port kept", "http://example.com:8080/a", "http://example.com:8080/a"},
		{"https non-default port kept", "https://example.com:8443/a", "https://example.com:8443/a"},
		{"http 443 is not a default port", "http://example.com:443/a", "http://example.com:443/a"},
		{"https 80 is not a default port", "https://example.com:80/a", "https://example.com:80/a"},

		// Fragment.
		{"fragment removed", "https://example.com/a#section", "https://example.com/a"},
		{"empty fragment removed", "https://example.com/a#", "https://example.com/a"},
		{"fragment removed before query ordering", "https://example.com/a?b=2&a=1#x", "https://example.com/a?a=1&b=2"},

		// Path.
		{"empty path becomes root", "https://example.com", "https://example.com/"},
		{"root path kept", "https://example.com/", "https://example.com/"},
		{"dot segment resolved", "https://example.com/a/./b", "https://example.com/a/b"},
		{"dotdot segment resolved", "https://example.com/a/b/../c", "https://example.com/a/c"},
		{"dotdot cannot escape root", "https://example.com/../../a", "https://example.com/a"},
		{"dotdot down to root stays root", "https://example.com/a/..", "https://example.com/"},
		{"repeated slashes collapsed", "https://example.com//a///b", "https://example.com/a/b"},
		{"trailing slash removed on non-root", "https://example.com/a/", "https://example.com/a"},
		{"trailing dot segment removed", "https://example.com/a/.", "https://example.com/a"},
		{"encoded dotdot is not a traversal", "https://example.com/a/%2E%2E/b", "https://example.com/a/%2E%2E/b"},
		{"encoded slash stays inside its segment", "https://example.com/a%2Fb", "https://example.com/a%2Fb"},

		// Tracking parameters.
		{"utm_ prefix dropped", "https://example.com/a?utm_source=x", "https://example.com/a"},
		{"utm_ prefix dropped case-insensitively", "https://example.com/a?UTM_Source=x", "https://example.com/a"},
		{"every utm_ variant dropped", "https://example.com/a?utm_medium=m&utm_campaign=c&utm_term=t", "https://example.com/a"},
		{"fbclid dropped", "https://example.com/a?fbclid=x", "https://example.com/a"},
		{"gclid dropped", "https://example.com/a?GCLID=x", "https://example.com/a"},
		{"repeated tracking keys all dropped", "https://example.com/a?utm_source=x&utm_source=y&id=1", "https://example.com/a?id=1"},
		{"encoded tracking key dropped", "https://example.com/a?utm%5Fsource=x&id=1", "https://example.com/a?id=1"},
		{"utm is not a prefix match without underscore", "https://example.com/a?utm=1", "https://example.com/a?utm=1"},
		{"utmx is not a tracking key", "https://example.com/a?utmx=1", "https://example.com/a?utmx=1"},
		{"fbclid substring is not a tracking key", "https://example.com/a?xfbclid=1", "https://example.com/a?xfbclid=1"},
		{"tracking removal can empty the query", "https://example.com/a?utm_source=x&fbclid=y", "https://example.com/a"},

		// Business query parameters.
		{"query pairs ordered by key", "https://example.com/a?b=2&a=1", "https://example.com/a?a=1&b=2"},
		{"query order is not identity", "https://example.com/a?a=1&b=2", "https://example.com/a?a=1&b=2"},
		{"equal keys ordered by value", "https://example.com/a?k=b&k=a", "https://example.com/a?k=a&k=b"},
		{"repeated identical pairs preserved", "https://example.com/a?k=1&k=1", "https://example.com/a?k=1&k=1"},
		{"empty value preserved", "https://example.com/a?k=", "https://example.com/a?k="},
		{"valueless key preserved", "https://example.com/a?k", "https://example.com/a?k"},
		{"valueless sorts before empty value", "https://example.com/a?k=&k", "https://example.com/a?k&k="},
		{"valueless and empty value converge on one order", "https://example.com/a?k&k=", "https://example.com/a?k&k="},
		{"encoded value preserved verbatim", "https://example.com/a?q=%E4%B8%AD%E6%96%87", "https://example.com/a?q=%E4%B8%AD%E6%96%87"},
		{"encoded value case preserved", "https://example.com/a?q=%e4%b8%ad", "https://example.com/a?q=%e4%b8%ad"},
		{"plus in value preserved", "https://example.com/a?q=a+b", "https://example.com/a?q=a+b"},
		{"empty query segments dropped", "https://example.com/a?&k=1&", "https://example.com/a?k=1"},
		{"bare question mark dropped", "https://example.com/a?", "https://example.com/a"},
		{"uppercase key sorts before lowercase key", "https://example.com/a?b=1&B=2", "https://example.com/a?B=2&b=1"},

		// Combinations across rules.
		{
			rule: "all rules at once",
			in:   "HTTPS://WWW.Example.COM.:443//blog/./posts/../2024/?utm_source=news&z=26&a=1#top",
			want: "https://example.com/blog/2024?a=1&z=26",
		},
		{
			rule: "combination keeps non-default port and business params",
			in:   "HTTP://WWW.Example.com:8080/a//b/?fbclid=1&b=2&a=1#x",
			want: "http://example.com:8080/a/b?a=1&b=2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.rule, func(t *testing.T) {
			got, err := urlidentity.Normalize(tc.in)
			if err != nil {
				t.Fatalf("Normalize(%q) error = %v, want nil", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeNeverMergesDistinctIdentities is the counterweight to the
// equivalence table: every pair below differs only in a dimension that the
// contract declares load-bearing, so a normaliser that got sloppy and merged
// them would be caught here rather than by a user finding two pages fused
// into one record.
func TestNormalizeNeverMergesDistinctIdentities(t *testing.T) {
	pairs := []struct {
		rule string
		a    string
		b    string
	}{
		{"http and https are different identities", "http://example.com/a", "https://example.com/a"},
		{"http and https stay apart after full normalisation", "HTTP://WWW.example.com/a/", "HTTPS://www.Example.com/a"},
		{"non-default ports stay apart", "https://example.com:8080/a", "https://example.com:8443/a"},
		{"a non-default port never merges with the default", "https://example.com:8443/a", "https://example.com/a"},
		{"http 80 and https 443 collapse to different origins", "http://example.com:80/a", "https://example.com:443/a"},
		{"business parameter values stay apart", "https://example.com/a?id=1", "https://example.com/a?id=2"},
		{"a dropped business parameter would be a merge", "https://example.com/a?id=1", "https://example.com/a"},
		{"repeat count of a business parameter matters", "https://example.com/a?k=1", "https://example.com/a?k=1&k=1"},
		{"empty value differs from absent parameter", "https://example.com/a?k=", "https://example.com/a"},
		{"valueless key differs from empty value", "https://example.com/a?k", "https://example.com/a?k="},
		{"different hosts stay apart", "https://example.com/a", "https://example.org/a"},
		{"subdomains stay apart", "https://example.com/a", "https://blog.example.com/a"},
		{"paths stay apart", "https://example.com/a", "https://example.com/b"},
		{"encoded slash differs from a real separator", "https://example.com/a%2Fb", "https://example.com/a/b"},
		{"userinfo is part of the identity", "https://user@example.com/a", "https://example.com/a"},
	}

	for _, tc := range pairs {
		t.Run(tc.rule, func(t *testing.T) {
			first, err := urlidentity.Normalize(tc.a)
			if err != nil {
				t.Fatalf("Normalize(%q) error = %v", tc.a, err)
			}
			second, err := urlidentity.Normalize(tc.b)
			if err != nil {
				t.Fatalf("Normalize(%q) error = %v", tc.b, err)
			}
			if first == second {
				t.Fatalf("Normalize(%q) and Normalize(%q) both = %q, want distinct identities", tc.a, tc.b, first)
			}
		})
	}
}

// TestNormalizeIsIdempotent guards the property the persistence layer relies
// on: a stored identity fed back through the contract must not drift, or a
// re-submitted record would stop matching the row it created.
func TestNormalizeIsIdempotent(t *testing.T) {
	inputs := []string{
		"HTTPS://WWW.Example.COM.:443//blog/./posts/../2024/?utm_source=news&z=26&a=1#top",
		"http://example.com:8080/a/%2E%2E/b?k&k=&q=a+b",
		"https://example.com",
		"https://user:pass@example.com:8443/x/",
		"https://[2001:db8::1]:8443/a/",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			once, err := urlidentity.Normalize(in)
			if err != nil {
				t.Fatalf("Normalize(%q) error = %v", in, err)
			}
			twice, err := urlidentity.Normalize(once)
			if err != nil {
				t.Fatalf("Normalize(%q) error = %v", once, err)
			}
			if once != twice {
				t.Fatalf("Normalize is not idempotent: %q -> %q -> %q", in, once, twice)
			}
		})
	}
}

func TestNormalizeIPv6HostKeepsBrackets(t *testing.T) {
	got, err := urlidentity.Normalize("HTTPS://[2001:DB8::1]:443/a/")
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if got != "https://[2001:db8::1]/a" {
		t.Fatalf("Normalize() = %q, want %q", got, "https://[2001:db8::1]/a")
	}
}

// TestNormalizeRejects pins the exact error each invalid input reports, so the
// HTTP layer's 422 code mapping cannot silently change category.
func TestNormalizeRejects(t *testing.T) {
	cases := []struct {
		rule string
		in   string
		want error
	}{
		{"empty string", "", urlidentity.ErrEmpty},
		{"whitespace only", "   \t\n ", urlidentity.ErrEmpty},
		{"relative path", "/foo/bar", urlidentity.ErrNotAbsolute},
		{"schemeless host", "example.com/a", urlidentity.ErrNotAbsolute},
		{"protocol relative", "//example.com/a", urlidentity.ErrNotAbsolute},
		{"scheme without host", "https://", urlidentity.ErrNotAbsolute},
		{"scheme with empty authority and path", "http:///a", urlidentity.ErrNotAbsolute},
		{"javascript scheme", "javascript:alert(1)", urlidentity.ErrNotAbsolute},
		{"data scheme", "data:text/html;base64,PHNjcmlwdD4=", urlidentity.ErrNotAbsolute},
		{"file scheme", "file:///etc/passwd", urlidentity.ErrNotAbsolute},
		{"mailto scheme", "mailto:a@example.com", urlidentity.ErrNotAbsolute},
		{"javascript scheme with authority", "javascript://example.com/%0aalert(1)", urlidentity.ErrUnsupportedScheme},
		{"ftp scheme", "ftp://example.com/a", urlidentity.ErrUnsupportedScheme},
		{"file scheme with host", "file://example.com/a", urlidentity.ErrUnsupportedScheme},
		{"ws scheme", "ws://example.com/a", urlidentity.ErrUnsupportedScheme},
		{"host is only a dot", "https://./a", urlidentity.ErrNotAbsolute},
		{"unparseable url", "https://exa mple.com/a", urlidentity.ErrNotAbsolute},
		{"control character", "https://example.com/\x7f", urlidentity.ErrNotAbsolute},
	}

	for _, tc := range cases {
		t.Run(tc.rule, func(t *testing.T) {
			got, err := urlidentity.Normalize(tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Normalize(%q) error = %v, want %v", tc.in, err, tc.want)
			}
			if got != "" {
				t.Fatalf("Normalize(%q) = %q, want empty string on failure", tc.in, got)
			}
		})
	}
}

// TestNormalizeTrimsSurroundingWhitespace covers the clipboard-paste shape
// that reaches the API with a trailing newline.
func TestNormalizeTrimsSurroundingWhitespace(t *testing.T) {
	got, err := urlidentity.Normalize("  https://example.com/a\n")
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if got != "https://example.com/a" {
		t.Fatalf("Normalize() = %q, want %q", got, "https://example.com/a")
	}
}

// TestNormalizeDoesNotGrowUnboundedly is a cheap guard that normalisation is a
// rewrite rather than an expansion: identity strings are persisted and indexed,
// so a rule that appended instead of replacing would silently inflate storage.
func TestNormalizeDoesNotGrowUnboundedly(t *testing.T) {
	in := "https://www.example.com/" + strings.Repeat("a/", 200) + "?b=1&a=2"
	got, err := urlidentity.Normalize(in)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if len(got) > len(in) {
		t.Fatalf("Normalize() grew the url from %d to %d bytes", len(in), len(got))
	}
}
