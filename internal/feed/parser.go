// Package feed owns remote feed parsing, discovery, and normalization. It is
// deliberately independent from persistence and HTTP handlers so every input
// format shares one sanitization and identity policy.
package feed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
	"github.com/microcosm-cc/bluemonday"
	"github.com/mmcdole/gofeed"

	"webtag/internal/model"
)

const (
	maxStoredItemHTML = 512 << 10
	maxStoredItemText = 512 << 10
	maxStoredSummary  = 16 << 10
	maxParsedItems    = 1000
	maxFeedInputItems = 2000
)

// ErrFeedItemLimitExceeded reports a feed document that would require the
// upstream parser to allocate an unsafe number of item objects.
var ErrFeedItemLimitExceeded = errors.New("feed contains more than 2000 items")

// ErrUnsupportedFeedDocument reports a non-XML document such as JSON Feed.
// WebTag's first RSS release intentionally supports only RSS, Atom, and RDF.
var ErrUnsupportedFeedDocument = errors.New("feed document must be RSS, Atom, or RDF XML")

// ErrMalformedFeedDocument reports XML that the strict preflight cannot parse.
// It must not be handed to gofeed's deliberately lenient XML pull parser.
var ErrMalformedFeedDocument = errors.New("feed document contains malformed XML")

type parseFeedDocumentFunc func(string) (*gofeed.Feed, error)

// Parser accepts RSS 2.0, Atom, and RSS 1.0/RDF through gofeed and projects
// every item into the storage model after sanitizing publisher-supplied HTML.
type Parser struct {
	policy        *bluemonday.Policy
	parseDocument parseFeedDocumentFunc
}

func NewParser() *Parser {
	return &Parser{
		policy: bluemonday.UGCPolicy(),
		parseDocument: func(document string) (*gofeed.Feed, error) {
			return gofeed.NewParser().ParseString(document)
		},
	}
}

func (p *Parser) Parse(body []byte, sourceURL string) (model.ParsedFeed, error) {
	if p == nil {
		p = NewParser()
	}
	if err := validateFeedDocumentPrefix(body); err != nil {
		return model.ParsedFeed{}, err
	}
	if err := preflightFeedItemCount(body); err != nil {
		return model.ParsedFeed{}, err
	}
	// gofeed.Parser lazily initializes format translators and is not safe for
	// concurrent Parse calls. Feed refreshes run concurrently, so keep only the
	// immutable sanitizer on Parser and allocate the small gofeed parser per
	// document.
	parseDocument := p.parseDocument
	if parseDocument == nil {
		parseDocument = func(document string) (*gofeed.Feed, error) {
			return gofeed.NewParser().ParseString(document)
		}
	}
	parsed, err := parseDocument(string(body))
	if err != nil {
		return model.ParsedFeed{}, fmt.Errorf("parse rss or atom document: %w", err)
	}
	if parsed == nil || strings.TrimSpace(parsed.Title) == "" && len(parsed.Items) == 0 {
		return model.ParsedFeed{}, fmt.Errorf("document does not contain a feed")
	}

	itemCapacity := min(len(parsed.Items), maxParsedItems)
	out := model.ParsedFeed{
		Title:       cleanPlain(parsed.Title, 1024),
		Description: cleanPlain(parsed.Description, maxStoredSummary),
		SiteURL:     resolveURL(sourceURL, parsed.Link),
		FeedType:    normalizeFeedType(parsed.FeedType),
		Items:       make([]model.FeedItem, 0, itemCapacity),
	}
	seenItems := make(map[string]struct{}, itemCapacity)
	for _, item := range parsed.Items {
		if item == nil {
			continue
		}
		normalized := p.normalizeItem(item, sourceURL)
		if normalized.Title == "" && normalized.URL == "" {
			continue
		}
		if _, duplicate := seenItems[normalized.ExternalID]; duplicate {
			continue
		}
		seenItems[normalized.ExternalID] = struct{}{}
		out.Items = append(out.Items, normalized)
		if len(out.Items) == maxParsedItems {
			break
		}
	}
	return out, nil
}

func validateFeedDocumentPrefix(body []byte) error {
	trimmed := bytes.TrimSpace(body)
	trimmed = bytes.TrimPrefix(trimmed, []byte{0xef, 0xbb, 0xbf})
	trimmed = bytes.TrimSpace(trimmed)
	if len(trimmed) == 0 || trimmed[0] != '<' {
		return ErrUnsupportedFeedDocument
	}
	return nil
}

type feedDocumentKind uint8

const (
	feedDocumentUnknown feedDocumentKind = iota
	feedDocumentRSS
	feedDocumentAtom
	feedDocumentRDF
)

type feedDocumentRoot struct {
	kind feedDocumentKind
}

// preflightFeedItemCount streams over the XML before gofeed builds its RSS or
// Atom intermediate objects. Strict XML errors are rejected here because
// gofeed's lenient parser may otherwise accept them after the item-count scan
// has stopped. Unknown roots, notably HTML discovery pages, return immediately
// without applying the feed limit.
func preflightFeedItemCount(body []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var root feedDocumentRoot
	rootSeen := false
	depth := 0
	itemCount := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrMalformedFeedDocument, err)
		}
		switch element := token.(type) {
		case xml.StartElement:
			if !rootSeen {
				root = classifyFeedDocumentRoot(element.Name)
				if root.kind == feedDocumentUnknown {
					return nil
				}
				rootSeen = true
				depth = 1
				continue
			}
			if isFeedItemElement(root, element.Name, depth) {
				itemCount++
				if itemCount > maxFeedInputItems {
					return ErrFeedItemLimitExceeded
				}
			}
			depth++
		case xml.EndElement:
			if rootSeen {
				depth--
				if depth == 0 {
					return nil
				}
			}
		}
	}
}

func classifyFeedDocumentRoot(name xml.Name) feedDocumentRoot {
	switch {
	case strings.EqualFold(name.Local, "rss"):
		return feedDocumentRoot{kind: feedDocumentRSS}
	case strings.EqualFold(name.Local, "feed"):
		return feedDocumentRoot{kind: feedDocumentAtom}
	case strings.EqualFold(name.Local, "rdf"):
		return feedDocumentRoot{kind: feedDocumentRDF}
	default:
		return feedDocumentRoot{}
	}
}

func isFeedItemElement(root feedDocumentRoot, name xml.Name, depth int) bool {
	switch root.kind {
	case feedDocumentRSS:
		return (depth == 1 || depth == 2) && strings.EqualFold(name.Local, "item")
	case feedDocumentAtom:
		return depth == 1 && strings.EqualFold(name.Local, "entry")
	case feedDocumentRDF:
		return depth == 1 && strings.EqualFold(name.Local, "item")
	default:
		return false
	}
}

func (p *Parser) normalizeItem(item *gofeed.Item, sourceURL string) model.FeedItem {
	itemURL := resolveURL(sourceURL, item.Link)
	rawHTML := strings.TrimSpace(item.Content)
	if rawHTML == "" {
		rawHTML = strings.TrimSpace(item.Description)
	}
	contentBase := itemURL
	if contentBase == "" {
		contentBase = sourceURL
	}
	cleanHTML := truncateUTF8(p.sanitizeHTML(rawHTML, contentBase), maxStoredItemHTML)
	plain := truncateUTF8(htmlToText(cleanHTML), maxStoredItemText)
	if plain == "" {
		plain = cleanPlain(rawHTML, maxStoredItemText)
	}

	summaryText := htmlToText(p.policy.Sanitize(strings.TrimSpace(item.Description)))
	if summaryText == "" && plain != "" {
		summaryText = plain
	}
	summaryText = truncateUTF8(summaryText, maxStoredSummary)

	published := item.PublishedParsed
	if published == nil {
		published = item.UpdatedParsed
	}
	author := ""
	if item.Author != nil {
		author = cleanPlain(item.Author.Name, 512)
	} else if len(item.Authors) > 0 && item.Authors[0] != nil {
		author = cleanPlain(item.Authors[0].Name, 512)
	}

	externalID := strings.TrimSpace(item.GUID)
	if externalID == "" {
		externalID = itemURL
	}
	if externalID == "" {
		publishedText := ""
		if published != nil {
			publishedText = published.UTC().Format(time.RFC3339Nano)
		}
		sum := sha256.Sum256([]byte(item.Title + "\x00" + publishedText + "\x00" + plain))
		externalID = "sha256:" + hex.EncodeToString(sum[:])
	}

	return model.FeedItem{
		ExternalID:  truncateUTF8(externalID, 2048),
		Title:       cleanPlain(item.Title, 2048),
		URL:         itemURL,
		Author:      optionalString(author),
		Summary:     optionalString(summaryText),
		Content:     optionalString(plain),
		ContentHTML: optionalString(cleanHTML),
		PublishedAt: cloneTime(published),
	}
}

func normalizeFeedType(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "atom":
		return "atom"
	case "rss":
		return "rss"
	default:
		return "rss"
	}
}

func resolveURL(baseRaw, refRaw string) string {
	refRaw = strings.TrimSpace(refRaw)
	if refRaw == "" {
		return ""
	}
	ref, err := url.Parse(refRaw)
	if err != nil {
		return ""
	}
	if !ref.IsAbs() {
		base, baseErr := url.Parse(baseRaw)
		if baseErr != nil {
			return ""
		}
		ref = base.ResolveReference(ref)
	}
	canonical, err := ValidateURL(ref.String())
	if err != nil {
		return ""
	}
	return canonical
}

func (p *Parser) sanitizeHTML(raw, baseURL string) string {
	sanitized := strings.TrimSpace(p.policy.Sanitize(raw))
	if sanitized == "" {
		return ""
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(sanitized))
	if err != nil {
		return ""
	}
	for _, attributeName := range []string{"href", "src"} {
		doc.Find("[" + attributeName + "]").Each(func(_ int, selection *goquery.Selection) {
			value, _ := selection.Attr(attributeName)
			resolved := resolveURL(baseURL, value)
			if resolved == "" {
				selection.RemoveAttr(attributeName)
				return
			}
			selection.SetAttr(attributeName, resolved)
		})
	}
	body := doc.Find("body").First()
	if body.Length() == 0 {
		return ""
	}
	html, err := body.Html()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(html)
}

func htmlToText(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(raw))
	if err != nil {
		return cleanPlain(raw, maxStoredItemText)
	}
	return strings.Join(strings.Fields(doc.Text()), " ")
}

func cleanPlain(raw string, maxBytes int) string {
	return truncateUTF8(strings.Join(strings.Fields(strings.TrimSpace(raw)), " "), maxBytes)
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return strings.TrimSpace(value[:end])
}

func optionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := value.UTC()
	return &copy
}
