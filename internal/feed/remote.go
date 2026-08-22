package feed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/security"
	"webtag/internal/urlidentity"
)

const (
	maxFeedResponseBytes = 4 << 20
	feedRequestTimeout   = 20 * time.Second
	maxDiscoveredFeeds   = 20
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type ConditionalHeaders struct {
	ETag         string
	LastModified string
}

type RemoteResult struct {
	Body         []byte
	FinalURL     string
	ContentType  string
	ETag         string
	LastModified string
	NotModified  bool
}

// Remote uses the runtime's SSRF-hardened HTTP client. Limits are enforced
// after decompression, preventing a small compressed response from expanding
// into an unbounded in-memory feed.
type Remote struct {
	client HTTPDoer
	parser *Parser
}

func NewRemote(client HTTPDoer, parser *Parser) *Remote {
	if parser == nil {
		parser = NewParser()
	}
	return &Remote{client: client, parser: parser}
}

func (r *Remote) Fetch(ctx context.Context, rawURL string, conditional ConditionalHeaders) (RemoteResult, error) {
	canonical, err := ValidateURL(rawURL)
	if err != nil {
		return RemoteResult{}, err
	}
	if r == nil || r.client == nil {
		return RemoteResult{}, fmt.Errorf("feed http client is not configured")
	}
	requestCtx, cancel := context.WithTimeout(ctx, feedRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, canonical, nil)
	if err != nil {
		return RemoteResult{}, fmt.Errorf("build feed request: %w", err)
	}
	req.Header.Set("Accept", "application/atom+xml, application/rss+xml, application/rdf+xml, application/xml, text/xml, text/html;q=0.8, */*;q=0.1")
	req.Header.Set("User-Agent", "WebTag Feed Reader/1.0")
	if conditional.ETag != "" {
		req.Header.Set("If-None-Match", conditional.ETag)
	}
	if conditional.LastModified != "" {
		req.Header.Set("If-Modified-Since", conditional.LastModified)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return RemoteResult{}, fmt.Errorf("fetch feed: %w", err)
	}
	defer resp.Body.Close()
	result := RemoteResult{
		FinalURL:     canonical,
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}
	if resp.Request != nil && resp.Request.URL != nil {
		result.FinalURL = resp.Request.URL.String()
	}
	if resp.StatusCode == http.StatusNotModified {
		result.NotModified = true
		return result, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return RemoteResult{}, fmt.Errorf("feed upstream returned HTTP %d", resp.StatusCode)
	}
	body, err := readBounded(resp.Body, maxFeedResponseBytes)
	if err != nil {
		return RemoteResult{}, err
	}
	result.Body = body
	return result, nil
}

func (r *Remote) FetchAndParse(ctx context.Context, rawURL string, conditional ConditionalHeaders) (RemoteResult, model.ParsedFeed, error) {
	remote, err := r.Fetch(ctx, rawURL, conditional)
	if err != nil || remote.NotModified {
		return remote, model.ParsedFeed{}, err
	}
	parsed, err := r.parser.Parse(remote.Body, remote.FinalURL)
	if err != nil {
		return remote, model.ParsedFeed{}, err
	}
	return remote, parsed, nil
}

// Discover first attempts feed parsing, then falls back to HTML alternate
// links. Every discovered URL is resolved against the final redirect target.
func (r *Remote) Discover(ctx context.Context, rawURL string) ([]model.FeedCandidate, error) {
	remote, err := r.Fetch(ctx, rawURL, ConditionalHeaders{})
	if err != nil {
		return nil, err
	}
	if parsed, parseErr := r.parser.Parse(remote.Body, remote.FinalURL); parseErr == nil {
		return []model.FeedCandidate{directFeedCandidate(parsed, remote.FinalURL)}, nil
	} else if errors.Is(parseErr, ErrFeedItemLimitExceeded) {
		return nil, problem.NewWithCode(problem.TooLarge, "feed_item_limit", ErrFeedItemLimitExceeded.Error())
	}

	if !looksLikeHTML(remote) {
		return nil, problem.NewWithCode(problem.Invalid, "feed_not_found", "URL is neither a valid feed nor an HTML page with feed discovery links")
	}
	return discoverHTMLFeeds(remote)
}

func directFeedCandidate(parsed model.ParsedFeed, finalURL string) model.FeedCandidate {
	title := parsed.Title
	if title == "" {
		title = finalURL
	}
	return model.FeedCandidate{URL: finalURL, Title: title, Type: parsed.FeedType}
}

func looksLikeHTML(remote RemoteResult) bool {
	contentType := strings.ToLower(remote.ContentType)
	bodyPrefix := strings.ToLower(strings.TrimSpace(string(remote.Body[:min(len(remote.Body), 512)])))
	return strings.Contains(contentType, "html") || strings.Contains(bodyPrefix, "<html") || strings.HasPrefix(bodyPrefix, "<!doctype html")
}

func discoverHTMLFeeds(remote RemoteResult) ([]model.FeedCandidate, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(remote.Body)))
	if err != nil {
		return nil, problem.NewWithCode(problem.Invalid, "feed_discovery_failed", "unable to parse page for feed discovery")
	}
	base, _ := url.Parse(remote.FinalURL)
	seen := make(map[string]struct{})
	feeds := make([]model.FeedCandidate, 0)
	doc.Find(`link[href]`).EachWithBreak(func(_ int, selection *goquery.Selection) bool {
		candidate, ok := feedCandidateFromLink(selection, base)
		if !ok {
			return true
		}
		if _, duplicate := seen[candidate.URL]; duplicate {
			return true
		}
		seen[candidate.URL] = struct{}{}
		feeds = append(feeds, candidate)
		return len(feeds) < maxDiscoveredFeeds
	})
	if len(feeds) == 0 {
		return nil, problem.NewWithCode(problem.Invalid, "feed_not_found", "no RSS or Atom feeds were discovered")
	}
	return feeds, nil
}

func feedCandidateFromLink(selection *goquery.Selection, base *url.URL) (model.FeedCandidate, bool) {
	if !hasLinkRelation(selection, "alternate") || base == nil {
		return model.FeedCandidate{}, false
	}
	typeValue := normalizedMediaType(attribute(selection, "type"))
	if !isFeedMediaType(typeValue) {
		return model.FeedCandidate{}, false
	}
	href := strings.TrimSpace(attribute(selection, "href"))
	ref, err := url.Parse(href)
	if err != nil || href == "" {
		return model.FeedCandidate{}, false
	}
	candidateURL, err := ValidateURL(base.ResolveReference(ref).String())
	if err != nil {
		return model.FeedCandidate{}, false
	}
	title := cleanPlain(attribute(selection, "title"), 1024)
	if title == "" {
		title = candidateURL
	}
	feedType := "rss"
	if typeValue == "application/atom+xml" {
		feedType = "atom"
	}
	return model.FeedCandidate{URL: candidateURL, Title: title, Type: feedType}, true
}

func hasLinkRelation(selection *goquery.Selection, relation string) bool {
	for _, token := range strings.Fields(attribute(selection, "rel")) {
		if strings.EqualFold(token, relation) {
			return true
		}
	}
	return false
}

func normalizedMediaType(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if mediaType, _, err := mime.ParseMediaType(value); err == nil {
		return strings.ToLower(mediaType)
	}
	return value
}

func isFeedMediaType(value string) bool {
	switch value {
	case "application/rss+xml", "application/atom+xml", "application/rdf+xml", "text/xml", "application/xml":
		return true
	default:
		return false
	}
}

// ValidateURL keeps the feed-specific admission rules — its own error slugs, the
// CRLF guard, and the refusal to store credentials — but delegates the actual
// canonical form to urlidentity.Normalize.
//
// It used to canonicalise inline, and that local copy folded fewer cases than
// the collection path did: subscribing to the same feed with a different host
// spelling or a tracking parameter produced a second subscription that polled
// the same document. A subscription is a collection entry point; it gets the
// same identity as every other one.
func ValidateURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", problem.NewWithCode(problem.Invalid, "feed_url_required", "feed URL is required")
	}
	if len(raw) > 2048 {
		return "", problem.NewWithCode(problem.Invalid, "feed_url_too_long", "feed URL exceeds length limit")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Opaque != "" || strings.ContainsAny(raw, "\r\n\t") {
		return "", problem.NewWithCode(problem.Invalid, "invalid_feed_url", "invalid feed URL")
	}
	if parsed.User != nil {
		return "", problem.NewWithCode(problem.Invalid, "feed_credentials_unsupported", "authenticated feed URLs are not supported")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", problem.NewWithCode(problem.Invalid, "unsupported_feed_url_scheme", "feed URL must use HTTP or HTTPS")
	}
	normalized, err := urlidentity.Normalize(raw)
	if err != nil {
		return "", problem.NewWithCode(problem.Invalid, "invalid_feed_url", "invalid feed URL")
	}
	// The request uses normalized, so the SSRF check must inspect that same
	// target. In particular, normalization folds www.localhost to localhost;
	// checking the pre-normalized host would allow a loopback request through.
	normalizedURL, err := url.Parse(normalized)
	if err != nil || security.IsUnsafeHost(normalizedURL.Hostname()) {
		return "", problem.NewWithCode(problem.Invalid, "unsafe_feed_url_target", "unsafe feed URL target")
	}
	return normalized, nil
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("invalid feed response limit")
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read feed response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, problem.NewWithCode(problem.TooLarge, "feed_response_too_large", "feed response exceeds size limit")
	}
	return body, nil
}

func attribute(selection *goquery.Selection, name string) string {
	value, _ := selection.Attr(name)
	return value
}
