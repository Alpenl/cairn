// Package contentdoc normalizes captured source documents into a canonical
// plain-text projection plus safe Markdown for reading.
package contentdoc

import (
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"golang.org/x/net/html"

	"webtag/internal/model"
)

var (
	languageClassPattern = regexp.MustCompile(`^language-[A-Za-z0-9_+.-]{1,64}$`)
	spaceBeforeNewline   = regexp.MustCompile(`[\t ]+\n`)
	excessBlankLines     = regexp.MustCompile(`\n{3,}`)
	styleSpaceReplacer   = strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "")
)

var removedElements = map[string]struct{}{
	"applet": {}, "audio": {}, "base": {}, "button": {}, "canvas": {},
	"embed": {}, "form": {}, "frame": {}, "frameset": {}, "head": {},
	"iframe": {}, "input": {}, "link": {}, "math": {}, "meta": {},
	"noscript": {}, "object": {}, "script": {}, "select": {}, "style": {},
	"svg": {}, "template": {}, "textarea": {}, "video": {},
}

var discussionTokens = map[string]struct{}{
	"comment": {}, "comments": {}, "comment-list": {}, "commentlist": {},
	"comment-thread": {}, "comment-section": {}, "comments-area": {},
	"reply": {}, "replies": {}, "reply-list": {}, "replies-list": {},
	"disqus_thread": {}, "giscus": {}, "utterances": {},
}

var blockElements = map[string]struct{}{
	"address": {}, "article": {}, "aside": {}, "blockquote": {}, "body": {},
	"dd": {}, "details": {}, "div": {}, "dl": {}, "dt": {}, "figcaption": {},
	"figure": {}, "footer": {}, "h1": {}, "h2": {}, "h3": {}, "h4": {},
	"h5": {}, "h6": {}, "header": {}, "hr": {}, "main": {}, "nav": {},
	"ol": {}, "p": {}, "pre": {}, "section": {}, "summary": {}, "table": {},
	"tbody": {}, "td": {}, "tfoot": {}, "th": {}, "thead": {}, "tr": {},
	"ul": {},
}

// Plain creates a legacy-compatible content snapshot while preserving useful
// line and paragraph boundaries instead of collapsing the entire document.
func Plain(value string) model.SavedContent {
	return model.SavedContent{
		Text:   normalizePlain(value),
		Format: model.ContentFormatPlain,
	}
}

// FromHTML converts untrusted HTML into safe Markdown and a canonical plain
// projection. Executable/embedded/form nodes and unsafe URLs are removed
// before conversion.
func FromHTML(value string, baseURL string) (model.SavedContent, error) {
	doc, err := html.Parse(strings.NewReader(value))
	if err != nil {
		return model.SavedContent{}, fmt.Errorf("parse content html: %w", err)
	}
	base := parseBaseURL(baseURL)
	pruneDiscussionTree(doc)
	if readable := selectReadableRoot(doc, base); readable != nil {
		doc = cloneAsDocument(readable)
	}
	sanitizeTree(doc, base)
	return fromSanitizedHTML(doc, baseURL)
}

// FromMarkdown parses GFM without enabling raw HTML, then runs the rendered
// tree through the same HTML sanitizer used for browser captures. This keeps
// headings/lists/tables/code while dropping embedded executable markup.
func FromMarkdown(value string, baseURL string) (model.SavedContent, error) {
	var rendered bytes.Buffer
	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	if err := md.Convert([]byte(value), &rendered); err != nil {
		return model.SavedContent{}, fmt.Errorf("parse content markdown: %w", err)
	}
	return FromHTML(rendered.String(), baseURL)
}

func fromSanitizedHTML(doc *html.Node, baseURL string) (model.SavedContent, error) {
	conv := converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(),
		strikethrough.NewStrikethroughPlugin(),
		table.NewTablePlugin(
			table.WithCellPaddingBehavior(table.CellPaddingBehaviorMinimal),
			table.WithHeaderPromotion(true),
		),
	))
	markdownBytes, err := conv.ConvertNode(doc, converter.WithDomain(baseURL))
	if err != nil {
		return model.SavedContent{}, fmt.Errorf("convert content html to markdown: %w", err)
	}
	markdown := strings.TrimSpace(string(markdownBytes))
	text := extractPlain(doc)
	if text == "" {
		return model.SavedContent{}, nil
	}
	if markdown == "" {
		return Plain(text), nil
	}
	return model.SavedContent{
		Text:     text,
		Document: stringPointer(markdown),
		Format:   model.ContentFormatMarkdown,
	}, nil
}

func sanitizeTree(node *html.Node, base *url.URL) {
	for child := node.FirstChild; child != nil; {
		next := child.NextSibling
		if child.Type == html.CommentNode {
			node.RemoveChild(child)
			child = next
			continue
		}
		if child.Type == html.ElementNode {
			name := strings.ToLower(child.Data)
			_, dangerous := removedElements[name]
			if dangerous || hiddenElement(child.Attr) || discussionElement(child.Attr) {
				node.RemoveChild(child)
				child = next
				continue
			}
			child.Attr = safeAttributes(name, child.Attr, base)
		}
		sanitizeTree(child, base)
		child = next
	}
}

func selectReadableRoot(doc *html.Node, base *url.URL) *html.Node {
	if best := selectV2EXReadableRoot(doc, base); best != nil {
		return best
	}

	for _, className := range []string{"markdown-body", "post-content", "article-content"} {
		if best := longestMatchingElement(doc, func(node *html.Node) bool {
			return hasClass(node, className)
		}); best != nil {
			return best
		}
	}

	// X lays the conversation out as several sibling articles. The first/main
	// post is normally much longer than individual replies; choosing the
	// longest article removes navigation, replies, and discovery rails without
	// applying that lossy heuristic to generic listing pages.
	if base != nil {
		host := strings.ToLower(base.Hostname())
		if host == "x.com" || host == "twitter.com" || strings.HasSuffix(host, ".x.com") || strings.HasSuffix(host, ".twitter.com") {
			if best := longestMatchingElement(doc, tagMatcher("article")); best != nil {
				return best
			}
		}
	}

	if best := longestMatchingElement(doc, tagMatcher("main")); best != nil {
		return best
	}
	if best := longestMatchingElement(doc, func(node *html.Node) bool {
		return attrEquals(node, "role", "main")
	}); best != nil {
		return best
	}

	articles := matchingElements(doc, tagMatcher("article"))
	if len(articles) == 1 {
		return articles[0]
	}

	for _, id := range []string{"content", "main"} {
		if best := longestMatchingElement(doc, func(node *html.Node) bool {
			return attrEquals(node, "id", id)
		}); best != nil {
			return best
		}
	}
	return nil
}

func selectV2EXReadableRoot(doc *html.Node, base *url.URL) *html.Node {
	if !isV2EXTopicURL(base) {
		return nil
	}
	return longestMatchingElement(doc, func(node *html.Node) bool {
		return hasClass(node, "topic_content")
	})
}

func tagMatcher(tag string) func(*html.Node) bool {
	return func(node *html.Node) bool {
		return node.Type == html.ElementNode && strings.EqualFold(node.Data, tag)
	}
}

func matchingElements(root *html.Node, match func(*html.Node) bool) []*html.Node {
	var matches []*html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if match(node) {
			matches = append(matches, node)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return matches
}

func longestMatchingElement(root *html.Node, match func(*html.Node) bool) *html.Node {
	var (
		best      *html.Node
		bestChars int
	)
	for _, candidate := range matchingElements(root, match) {
		chars := visibleTextChars(candidate)
		if chars > bestChars {
			best = candidate
			bestChars = chars
		}
	}
	return best
}

func visibleTextChars(node *html.Node) int {
	if node.Type == html.ElementNode {
		if _, dangerous := removedElements[strings.ToLower(node.Data)]; dangerous || hiddenElement(node.Attr) || discussionElement(node.Attr) {
			return 0
		}
	}
	if node.Type == html.TextNode {
		return utf8.RuneCountInString(strings.TrimSpace(node.Data))
	}
	total := 0
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		total += visibleTextChars(child)
	}
	return total
}

func pruneDiscussionTree(node *html.Node) {
	for child := node.FirstChild; child != nil; {
		next := child.NextSibling
		if child.Type == html.ElementNode && discussionElement(child.Attr) {
			node.RemoveChild(child)
			child = next
			continue
		}
		pruneDiscussionTree(child)
		child = next
	}
}

func discussionElement(attrs []html.Attribute) bool {
	for _, attr := range attrs {
		name := strings.ToLower(strings.TrimSpace(attr.Key))
		value := strings.ToLower(strings.TrimSpace(attr.Val))
		switch name {
		case "id", "data-testid":
			if _, ok := discussionTokens[value]; ok {
				return true
			}
		case "class", "itemprop":
			for _, token := range strings.Fields(value) {
				if _, ok := discussionTokens[token]; ok {
					return true
				}
			}
		case "itemtype":
			for _, itemType := range strings.Fields(value) {
				if strings.HasSuffix(strings.TrimRight(itemType, "/"), "/comment") {
					return true
				}
			}
		}
	}
	return false
}

func isV2EXTopicURL(base *url.URL) bool {
	if base == nil || !strings.HasPrefix(base.EscapedPath(), "/t/") {
		return false
	}
	host := strings.ToLower(base.Hostname())
	return host == "v2ex.com" || strings.HasSuffix(host, ".v2ex.com")
}

func hasClass(node *html.Node, className string) bool {
	if node.Type != html.ElementNode {
		return false
	}
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, "class") {
			for _, class := range strings.Fields(attr.Val) {
				if class == className {
					return true
				}
			}
		}
	}
	return false
}

func attrEquals(node *html.Node, name string, value string) bool {
	if node.Type != html.ElementNode {
		return false
	}
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, name) && strings.EqualFold(strings.TrimSpace(attr.Val), value) {
			return true
		}
	}
	return false
}

func cloneAsDocument(source *html.Node) *html.Node {
	doc := &html.Node{Type: html.DocumentNode}
	doc.AppendChild(cloneNode(source))
	return doc
}

func cloneNode(source *html.Node) *html.Node {
	clone := &html.Node{
		Type:      source.Type,
		DataAtom:  source.DataAtom,
		Data:      source.Data,
		Namespace: source.Namespace,
		Attr:      append([]html.Attribute(nil), source.Attr...),
	}
	for child := source.FirstChild; child != nil; child = child.NextSibling {
		clone.AppendChild(cloneNode(child))
	}
	return clone
}

func hiddenElement(attrs []html.Attribute) bool {
	for _, attr := range attrs {
		name := strings.ToLower(strings.TrimSpace(attr.Key))
		value := strings.ToLower(strings.TrimSpace(attr.Val))
		switch name {
		case "hidden":
			return true
		case "aria-hidden":
			if value == "true" {
				return true
			}
		case "style":
			compact := styleSpaceReplacer.Replace(value)
			if strings.Contains(compact, "display:none") ||
				strings.Contains(compact, "visibility:hidden") ||
				strings.Contains(compact, "content-visibility:hidden") {
				return true
			}
		}
	}
	return false
}

func safeAttributes(tag string, attrs []html.Attribute, base *url.URL) []html.Attribute { //nolint:gocyclo // HTML 属性白名单，分支即允许的属性集
	out := make([]html.Attribute, 0, 3)
	for _, attr := range attrs {
		name := strings.ToLower(strings.TrimSpace(attr.Key))
		value := strings.TrimSpace(attr.Val)
		switch {
		case tag == "a" && name == "href":
			if safe := safeURL(value, base, false); safe != "" {
				out = append(out, html.Attribute{Key: "href", Val: safe})
			}
		case tag == "img" && name == "src":
			if safe := safeURL(value, base, true); safe != "" {
				out = append(out, html.Attribute{Key: "src", Val: safe})
			}
		case tag == "img" && (name == "alt" || name == "title"):
			out = append(out, html.Attribute{Key: name, Val: value})
		case tag == "a" && name == "title":
			out = append(out, html.Attribute{Key: name, Val: value})
		case tag == "code" && name == "class" && languageClassPattern.MatchString(value):
			out = append(out, html.Attribute{Key: name, Val: value})
		case tag == "ol" && name == "start":
			out = append(out, html.Attribute{Key: name, Val: value})
		case (tag == "td" || tag == "th") && (name == "colspan" || name == "rowspan"):
			out = append(out, html.Attribute{Key: name, Val: value})
		}
	}
	return out
}

func safeURL(raw string, base *url.URL, image bool) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if !parsed.IsAbs() {
		if base == nil {
			if strings.HasPrefix(raw, "#") {
				return raw
			}
			return ""
		}
		parsed = base.ResolveReference(parsed)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if image {
		if scheme != "http" && scheme != "https" {
			return ""
		}
	} else if scheme != "" && scheme != "http" && scheme != "https" && scheme != "mailto" && scheme != "tel" {
		return ""
	}
	if parsed.User != nil {
		parsed.User = nil
	}
	return parsed.String()
}

func parseBaseURL(raw string) *url.URL {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil
	}
	return parsed
}

func extractPlain(doc *html.Node) string {
	var out bytes.Buffer
	appendPlain(&out, doc, false)
	value := strings.ReplaceAll(out.String(), "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = spaceBeforeNewline.ReplaceAllString(value, "\n")
	value = excessBlankLines.ReplaceAllString(value, "\n\n")
	return strings.TrimSpace(value)
}

func appendPlain(out *bytes.Buffer, node *html.Node, inPre bool) {
	if node.Type == html.TextNode {
		if inPre {
			out.WriteString(node.Data)
			return
		}
		appendCollapsedText(out, node.Data)
		return
	}

	name := ""
	if node.Type == html.ElementNode {
		name = strings.ToLower(node.Data)
	}
	if name == "br" {
		ensureNewlines(out, 1)
		return
	}
	_, block := blockElements[name]
	if block {
		ensureNewlines(out, 2)
	}
	if name == "li" {
		ensureNewlines(out, 1)
	}

	childPre := inPre || name == "pre"
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		appendPlain(out, child, childPre)
	}

	if name == "li" {
		ensureNewlines(out, 1)
	} else if block {
		ensureNewlines(out, 2)
	}
}

func appendCollapsedText(out *bytes.Buffer, value string) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return
	}
	leadingSpace := startsWithSpace(value)
	trailingSpace := endsWithSpace(value)
	if leadingSpace && out.Len() > 0 && !lastIsSpace(out) {
		out.WriteByte(' ')
	}
	for idx, field := range fields {
		if idx > 0 && !lastIsSpace(out) {
			out.WriteByte(' ')
		}
		out.WriteString(field)
	}
	if trailingSpace && !lastIsSpace(out) {
		out.WriteByte(' ')
	}
}

func ensureNewlines(out *bytes.Buffer, count int) {
	value := out.Bytes()
	end := len(value)
	for end > 0 && (value[end-1] == ' ' || value[end-1] == '\t') {
		end--
	}
	if end != len(value) {
		out.Truncate(end)
		value = out.Bytes()
	}
	existing := 0
	for idx := len(value) - 1; idx >= 0 && value[idx] == '\n'; idx-- {
		existing++
	}
	for existing < count {
		out.WriteByte('\n')
		existing++
	}
}

func startsWithSpace(value string) bool {
	for _, r := range value {
		return unicode.IsSpace(r)
	}
	return false
}

func endsWithSpace(value string) bool {
	if value == "" {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(value)
	return unicode.IsSpace(r)
}

func lastIsSpace(out *bytes.Buffer) bool {
	value := out.Bytes()
	if len(value) == 0 {
		return false
	}
	r, _ := utf8.DecodeLastRune(value)
	return unicode.IsSpace(r)
}

func normalizePlain(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = spaceBeforeNewline.ReplaceAllString(value, "\n")
	value = excessBlankLines.ReplaceAllString(value, "\n\n")
	return strings.TrimSpace(value)
}

func stringPointer(value string) *string {
	copy := value
	return &copy
}
