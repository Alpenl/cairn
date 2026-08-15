package feed

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mmcdole/gofeed"
)

func TestParserSupportsRSSAtomAndRSS1(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		document string
		wantType string
	}{
		{
			name: "rss2",
			document: `<?xml version="1.0"?><rss version="2.0"><channel><title>RSS News</title><link>https://example.com/</link>
				<item><guid>one</guid><title>First</title><link>/posts/1#fragment</link><description><![CDATA[<p>Hello</p>]]></description></item>
			</channel></rss>`,
			wantType: "rss",
		},
		{
			name: "atom",
			document: `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>Atom News</title>
				<link href="https://example.com/"/><entry><id>two</id><title>Second</title><link href="https://example.com/posts/2"/><content type="html">&lt;p&gt;World&lt;/p&gt;</content></entry>
			</feed>`,
			wantType: "atom",
		},
		{
			name: "rss1",
			document: `<?xml version="1.0"?><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/">
				<channel rdf:about="https://example.com/feed"><title>RDF News</title><link>https://example.com/</link><description>News</description></channel>
				<item rdf:about="https://example.com/posts/3"><title>Third</title><link>https://example.com/posts/3</link><description>RDF body</description></item>
			</rdf:RDF>`,
			wantType: "rss",
		},
	}
	parser := NewParser()
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := parser.Parse([]byte(test.document), "https://example.com/feed.xml")
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if parsed.FeedType != test.wantType {
				t.Fatalf("FeedType = %q, want %q", parsed.FeedType, test.wantType)
			}
			if len(parsed.Items) != 1 {
				t.Fatalf("len(Items) = %d, want 1", len(parsed.Items))
			}
			if strings.Contains(parsed.Items[0].URL, "#") {
				t.Fatalf("item URL retained fragment: %q", parsed.Items[0].URL)
			}
		})
	}
}

func TestParserBoundsItemProcessingAndDeduplicatesExternalIDs(t *testing.T) {
	t.Parallel()
	var document strings.Builder
	document.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>Many</title>`)
	for index := 0; index < maxParsedItems+100; index++ {
		_, _ = fmt.Fprintf(&document, `<item><guid>id-%04d</guid><title>%d</title></item>`, index, index)
	}
	document.WriteString(`<item><guid>id-0000</guid><title>duplicate</title></item></channel></rss>`)
	parsed, err := NewParser().Parse([]byte(document.String()), "https://example.com/feed.xml")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(parsed.Items) != maxParsedItems {
		t.Fatalf("len(Items) = %d, want %d", len(parsed.Items), maxParsedItems)
	}
	if parsed.Items[0].ExternalID != "id-0000" || parsed.Items[len(parsed.Items)-1].ExternalID != "id-0999" {
		t.Fatalf("bounded item identities first=%q last=%q", parsed.Items[0].ExternalID, parsed.Items[len(parsed.Items)-1].ExternalID)
	}
}

func TestParserRejectsNearLimitFeedBeforeGofeedAllocation(t *testing.T) {
	t.Parallel()
	const item = `<item><guid>x</guid><title>x</title></item>`
	prefix := `<?xml version="1.0"?><rss version="2.0"><channel><title>Many</title>`
	suffix := `</channel></rss>`
	itemCount := (maxFeedResponseBytes - len(prefix) - len(suffix) - 1024) / len(item)
	document := prefix + strings.Repeat(item, itemCount) + suffix
	if itemCount < 50_000 || len(document) < maxFeedResponseBytes-(64<<10) || len(document) >= maxFeedResponseBytes {
		t.Fatalf("large feed fixture items=%d bytes=%d", itemCount, len(document))
	}

	assertParserRejectedBeforeGofeed(t, []byte(document), ErrFeedItemLimitExceeded)
}

func TestParserPreflightClosesGofeedXMLCountingBypasses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		prefix string
		item   string
		suffix string
	}{
		{name: "uppercase rss", prefix: `<RSS><CHANNEL>`, item: `<ITEM/>`, suffix: `</CHANNEL></RSS>`},
		{name: "uppercase atom", prefix: `<FEED>`, item: `<ENTRY/>`, suffix: `</FEED>`},
		{name: "uppercase rdf", prefix: `<RDF>`, item: `<ITEM/>`, suffix: `</RDF>`},
		{name: "rss root direct item", prefix: `<rss>`, item: `<item/>`, suffix: `</rss>`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			document := test.prefix + strings.Repeat(test.item, maxFeedInputItems+1) + test.suffix
			assertParserRejectedBeforeGofeed(t, []byte(document), ErrFeedItemLimitExceeded)
		})
	}
}

func TestParserRejectsNearLimitJSONFeedBeforeGofeedAllocation(t *testing.T) {
	t.Parallel()
	const item = `{"id":"x","content_text":"x"},`
	prefix := `{"version":"https://jsonfeed.org/version/1.1","title":"Many","items":[`
	suffix := `{"id":"last","content_text":"x"}]}`
	itemCount := (maxFeedResponseBytes - len(prefix) - len(suffix) - 1024) / len(item)
	document := prefix + strings.Repeat(item, itemCount) + suffix
	if itemCount < 50_000 || len(document) < maxFeedResponseBytes-(64<<10) || len(document) >= maxFeedResponseBytes {
		t.Fatalf("large JSON Feed fixture items=%d bytes=%d", itemCount, len(document))
	}
	assertParserRejectedBeforeGofeed(t, []byte(document), ErrUnsupportedFeedDocument)
}

func TestFeedDocumentPrefixAllowsUTF8BOMAndWhitespace(t *testing.T) {
	t.Parallel()
	if err := validateFeedDocumentPrefix([]byte(" \n\xef\xbb\xbf\t<rss/>")); err != nil {
		t.Fatalf("validateFeedDocumentPrefix() error = %v", err)
	}
}

func TestFeedItemPreflightRecognizesNamespacesAndIgnoresHTML(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prefix  string
		item    string
		suffix  string
		wantErr bool
	}{
		{
			name:    "namespaced rss",
			prefix:  `<rss xmlns="urn:example:rss"><channel>`,
			item:    `<item/>`,
			suffix:  `</channel></rss>`,
			wantErr: true,
		},
		{
			name:    "prefixed atom",
			prefix:  `<atom:feed xmlns:atom="http://www.w3.org/2005/Atom">`,
			item:    `<atom:entry/>`,
			suffix:  `</atom:feed>`,
			wantErr: true,
		},
		{
			name:    "rdf rss1",
			prefix:  `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/">`,
			item:    `<item/>`,
			suffix:  `</rdf:RDF>`,
			wantErr: true,
		},
		{
			name:    "rss item namespace mismatch",
			prefix:  `<rss><channel>`,
			item:    `<item xmlns="urn:untrusted"/>`,
			suffix:  `</channel></rss>`,
			wantErr: true,
		},
		{
			name:    "atom root and entry namespace mismatch",
			prefix:  `<feed xmlns="urn:untrusted-root">`,
			item:    `<entry xmlns="urn:untrusted-entry"/>`,
			suffix:  `</feed>`,
			wantErr: true,
		},
		{
			name:   "html discovery page",
			prefix: `<html><body>`,
			item:   `<entry/>`,
			suffix: `</body></html>`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			document := test.prefix + strings.Repeat(test.item, maxFeedInputItems+1) + test.suffix
			err := preflightFeedItemCount([]byte(document))
			if got := errors.Is(err, ErrFeedItemLimitExceeded); got != test.wantErr {
				t.Fatalf("preflightFeedItemCount() error = %v, limit=%v want %v", err, got, test.wantErr)
			}
		})
	}
}

func TestFeedItemPreflightRejectsLenientXMLBypassBeforeConfiguredParser(t *testing.T) {
	t.Parallel()
	document := `<rss><channel><title>&bogus;</title>` + strings.Repeat(`<item/>`, maxFeedInputItems+1) + `</channel></rss>`
	assertParserRejectedBeforeGofeed(t, []byte(document), ErrMalformedFeedDocument)
}

func assertParserRejectedBeforeGofeed(t *testing.T, document []byte, want error) {
	t.Helper()
	parser := NewParser()
	parseCalled := false
	parser.parseDocument = func(string) (*gofeed.Feed, error) {
		parseCalled = true
		return nil, errors.New("gofeed should not be called")
	}
	_, err := parser.Parse(document, "https://example.com/feed.xml")
	if !errors.Is(err, want) {
		t.Fatalf("Parse() error = %v, want %v", err, want)
	}
	if parseCalled {
		t.Fatal("gofeed parser was called after preflight rejection")
	}
}

func TestParserSanitizesStoredHTMLAndUnsafeURLs(t *testing.T) {
	t.Parallel()
	document := `<?xml version="1.0"?><rss version="2.0"><channel><title>Safe</title><link>https://example.com/</link>
		<item><guid>x</guid><title>Entry</title><link>https://user:secret@example.com/post</link>
		<description><![CDATA[
			<script>alert(1)</script><p>Hello <a href="javascript:alert(1)">bad</a>
			<a href="/good">good</a><img src="http://127.0.0.1/private"><img src="data:image/png;base64,AAAA"></p>
		]]></description></item></channel></rss>`
	parsed, err := NewParser().Parse([]byte(document), "https://example.com/feed.xml")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	item := parsed.Items[0]
	if item.URL != "" {
		t.Fatalf("credential-bearing item URL = %q, want empty", item.URL)
	}
	if item.ContentHTML == nil {
		t.Fatal("ContentHTML is nil")
	}
	html := *item.ContentHTML
	for _, forbidden := range []string{"<script", "javascript:", "127.0.0.1", "data:image"} {
		if strings.Contains(strings.ToLower(html), forbidden) {
			t.Fatalf("sanitized HTML contains %q: %s", forbidden, html)
		}
	}
	if !strings.Contains(html, `href="https://example.com/good"`) {
		t.Fatalf("relative safe link was not resolved: %s", html)
	}
	if item.Content == nil || !strings.Contains(*item.Content, "Hello") {
		t.Fatalf("plain content = %v, want readable text", item.Content)
	}
}

func TestParserPreservesDocumentOrderForUndatedItems(t *testing.T) {
	t.Parallel()
	document := `<?xml version="1.0"?><rss version="2.0"><channel><title>Order</title>
		<item><guid>z-first</guid><title>First</title></item>
		<item><guid>a-second</guid><title>Second</title></item>
	</channel></rss>`
	parsed, err := NewParser().Parse([]byte(document), "https://example.com/feed.xml")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(parsed.Items) != 2 || parsed.Items[0].ExternalID != "z-first" || parsed.Items[1].ExternalID != "a-second" {
		t.Fatalf("undated order = %#v", parsed.Items)
	}
}

func TestParserTreatsPublisherDocumentOrderAsTheSyncHead(t *testing.T) {
	t.Parallel()
	document := `<?xml version="1.0"?><rss version="2.0"><channel><title>Order</title>
		<item><guid>declared-first</guid><title>First</title><pubDate>Mon, 01 Jan 2024 00:00:00 GMT</pubDate></item>
		<item><guid>newer-date-second</guid><title>Second</title><pubDate>Mon, 01 Jan 2025 00:00:00 GMT</pubDate></item>
	</channel></rss>`
	parsed, err := NewParser().Parse([]byte(document), "https://example.com/feed.xml")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(parsed.Items) != 2 || parsed.Items[0].ExternalID != "declared-first" || parsed.Items[1].ExternalID != "newer-date-second" {
		t.Fatalf("document order = %#v", parsed.Items)
	}
}
