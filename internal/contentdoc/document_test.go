package contentdoc

import (
	"strings"
	"testing"

	"webtag/internal/model"
)

func TestFromHTMLPreservesStructureAndRemovesExecutableContent(t *testing.T) {
	t.Parallel()

	got, err := FromHTML(`<article onclick="steal()">
		<h1>Guide</h1><p>First <strong>paragraph</strong>.</p>
		<div hidden>hidden attribute secret</div>
		<div aria-hidden="true">aria hidden secret</div>
		<div style="display: none !important">inline style secret</div>
		<ul><li>One</li><li>Two</li></ul>
		<pre><code class="language-go">fmt.Println(&quot;ok&quot;)</code></pre>
		<p><a href="/docs">Docs</a> <a href="javascript:alert(1)">unsafe</a></p>
		<script>alert("must not survive")</script>
	</article>`, "https://example.com/article")
	if err != nil {
		t.Fatalf("FromHTML() error = %v", err)
	}
	if got.Format != model.ContentFormatMarkdown || got.Document == nil {
		t.Fatalf("FromHTML() = %#v, want markdown document", got)
	}
	for _, want := range []string{"# Guide", "- One", "- Two", "```", "[Docs](https://example.com/docs)"} {
		if !strings.Contains(*got.Document, want) {
			t.Errorf("document = %q, want %q", *got.Document, want)
		}
	}
	for _, unsafe := range []string{
		"<script", "alert(\"must not survive\")", "javascript:", "onclick",
		"hidden attribute secret", "aria hidden secret", "inline style secret",
	} {
		if strings.Contains(*got.Document, unsafe) || strings.Contains(got.Text, unsafe) {
			t.Errorf("normalized content contains unsafe %q: %#v", unsafe, got)
		}
	}
	if !strings.Contains(got.Text, "Guide\n\nFirst paragraph.") || !strings.Contains(got.Text, "One\nTwo") {
		t.Errorf("text = %q, want semantic block boundaries", got.Text)
	}
}

func TestFromMarkdownDropsRawHTMLAndUnsafeLinks(t *testing.T) {
	t.Parallel()

	got, err := FromMarkdown("# Heading\n\n- item\n\n<script>alert(1)</script>\n\n[x](javascript:alert(2))", "https://example.com")
	if err != nil {
		t.Fatalf("FromMarkdown() error = %v", err)
	}
	if got.Document == nil || !strings.Contains(*got.Document, "# Heading") || !strings.Contains(*got.Document, "- item") {
		t.Fatalf("document = %#v, want preserved Markdown structure", got.Document)
	}
	if strings.Contains(*got.Document, "script") || strings.Contains(*got.Document, "javascript:") || strings.Contains(got.Text, "alert") {
		t.Fatalf("unsafe Markdown survived: %#v", got)
	}
}

func TestFromHTMLSelectsSpecificReadableContainer(t *testing.T) {
	t.Parallel()

	got, err := FromHTML(`<html><body>
		<nav><h1>Repository navigation</h1><a href="/search">Search everything</a></nav>
		<article class="markdown-body"><h1>Weekly 402</h1><p>Actual issue body.</p><ul><li>Tool one</li></ul></article>
		<aside>Trending repositories and account controls</aside>
	</body></html>`, "https://github.com/example/repo/blob/main/issue.md")
	if err != nil {
		t.Fatalf("FromHTML() error = %v", err)
	}
	if got.Document == nil || !strings.Contains(*got.Document, "# Weekly 402") {
		t.Fatalf("document = %#v, want markdown-body content", got.Document)
	}
	for _, noise := range []string{"Repository navigation", "Search everything", "Trending repositories"} {
		if strings.Contains(got.Text, noise) || strings.Contains(*got.Document, noise) {
			t.Errorf("selected document contains page chrome %q: %#v", noise, got)
		}
	}
}

func TestFromHTMLSelectsLongestXArticle(t *testing.T) {
	t.Parallel()

	got, err := FromHTML(`<html><body>
		<nav>Home Explore Notifications Messages Bookmarks</nav>
		<main>
			<article><p>Main post has a complete description of the project, its workflow, monitoring, and backtesting features.</p><a href="/project">Project</a></article>
			<article><p>Short reply.</p></article>
			<article><p>Another reply.</p></article>
			<section>Discover more posts and trending topics.</section>
		</main>
	</body></html>`, "https://x.com/example/status/1")
	if err != nil {
		t.Fatalf("FromHTML() error = %v", err)
	}
	if !strings.Contains(got.Text, "Main post") {
		t.Fatalf("text = %q, want main X post", got.Text)
	}
	for _, noise := range []string{"Home Explore", "Short reply", "Another reply", "Discover more"} {
		if strings.Contains(got.Text, noise) {
			t.Errorf("selected X article contains %q: %q", noise, got.Text)
		}
	}
}

func TestFromHTMLKeepsMainForGenericListings(t *testing.T) {
	t.Parallel()

	got, err := FromHTML(`<html><body><nav>Site navigation</nav><main>
		<article><h2>First card</h2><p>First summary.</p></article>
		<article><h2>Second card</h2><p>Second summary.</p></article>
	</main></body></html>`, "https://news.example.com/")
	if err != nil {
		t.Fatalf("FromHTML() error = %v", err)
	}
	if !strings.Contains(got.Text, "First card") || !strings.Contains(got.Text, "Second card") {
		t.Fatalf("text = %q, want the whole generic main listing", got.Text)
	}
	if strings.Contains(got.Text, "Site navigation") {
		t.Fatalf("text = %q, must omit navigation outside main", got.Text)
	}
}

func TestFromHTMLRemovesSemanticCommentRegions(t *testing.T) {
	t.Parallel()

	got, err := FromHTML(`<html><body><main>
		<article><h1>Release analysis</h1><p>The release reduced median latency by 30 percent.</p></article>
		<section class="commentary"><h2>Author commentary</h2><p>This paragraph is part of the article and must remain.</p></section>
		<section id="comments" aria-label="Reader comments">
			<article itemprop="comment"><b>reply-author</b><p>This reader reply must be removed.</p></article>
		</section>
	</main></body></html>`, "https://example.com/release")
	if err != nil {
		t.Fatalf("FromHTML() error = %v", err)
	}
	for _, want := range []string{"Release analysis", "reduced median latency", "Author commentary", "must remain"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("text = %q, want article text %q", got.Text, want)
		}
	}
	for _, unwanted := range []string{"reply-author", "reader reply"} {
		if strings.Contains(got.Text, unwanted) || (got.Document != nil && strings.Contains(*got.Document, unwanted)) {
			t.Errorf("normalized content contains comment %q: %#v", unwanted, got)
		}
	}
}
