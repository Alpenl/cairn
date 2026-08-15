package feed

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"webtag/internal/httperr"
)

type remoteDoerFunc func(*http.Request) (*http.Response, error)

func (fn remoteDoerFunc) Do(request *http.Request) (*http.Response, error) { return fn(request) }

func TestRemoteDiscoveryHandlesCaseAndMIMEParameters(t *testing.T) {
	t.Parallel()
	remote := NewRemote(remoteDoerFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			Body: io.NopCloser(strings.NewReader(`<html><head>
				<link REL="Alternate stylesheet" TYPE="Application/Atom+XML; charset=utf-8" href="/atom.xml" title="Posts">
			</head></html>`)),
			Request: request,
		}, nil
	}), NewParser())
	feeds, err := remote.Discover(context.Background(), "https://example.com/blog")
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(feeds) != 1 || feeds[0].URL != "https://example.com/atom.xml" || feeds[0].Type != "atom" {
		t.Fatalf("Discover() = %#v", feeds)
	}
}

func TestRemoteDiscoverySurfacesFeedItemLimit(t *testing.T) {
	t.Parallel()
	body := `<rss><channel><title>Too many</title>` + strings.Repeat(`<item/>`, maxFeedInputItems+1) + `</channel></rss>`
	remote := NewRemote(remoteDoerFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/rss+xml"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	}), NewParser())
	_, err := remote.Discover(context.Background(), "https://example.com/feed.xml")
	carrier, ok := httperr.As(err)
	if !ok || carrier.HTTPStatus() != http.StatusRequestEntityTooLarge || carrier.HTTPMessage() != ErrFeedItemLimitExceeded.Error() {
		t.Fatalf("Discover() error = %v carrier=%v, want 413 feed item limit", err, ok)
	}
	coder, ok := carrier.(httperr.ErrorCoder)
	if !ok || coder.HTTPErrorCode() != "feed_item_limit" {
		t.Fatalf("Discover() error code = %v, want feed_item_limit", coder)
	}
}

func TestRemoteDiscoveryRejectsJSONFeedAsUnsupported(t *testing.T) {
	t.Parallel()
	body := `{"version":"https://jsonfeed.org/version/1.1","title":"JSON","items":[]}`
	remote := NewRemote(remoteDoerFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/feed+json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	}), NewParser())
	_, err := remote.Discover(context.Background(), "https://example.com/feed.json")
	carrier, ok := httperr.As(err)
	if !ok || carrier.HTTPStatus() != http.StatusUnprocessableEntity {
		t.Fatalf("Discover() error = %v carrier=%v, want 422", err, ok)
	}
	coder, ok := carrier.(httperr.ErrorCoder)
	if !ok || coder.HTTPErrorCode() != "feed_not_found" {
		t.Fatalf("Discover() error code = %v, want feed_not_found", coder)
	}
}

func TestRemoteFetchSendsConditionalHeadersAndHandles304(t *testing.T) {
	t.Parallel()
	remote := NewRemote(remoteDoerFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("If-None-Match"); got != `"v1"` {
			t.Fatalf("If-None-Match = %q", got)
		}
		if got := request.Header.Get("If-Modified-Since"); got == "" {
			t.Fatal("If-Modified-Since is empty")
		}
		return &http.Response{StatusCode: http.StatusNotModified, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	}), NewParser())
	result, err := remote.Fetch(context.Background(), "https://example.com/feed", ConditionalHeaders{ETag: `"v1"`, LastModified: "Mon, 01 Jan 2024 00:00:00 GMT"})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !result.NotModified {
		t.Fatal("NotModified = false")
	}
}

func TestRemoteFetchAndParseReturnsRemoteMetadataOnParseError(t *testing.T) {
	t.Parallel()
	remote := NewRemote(remoteDoerFunc(func(request *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("ETag", `"broken"`)
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader("not a feed")), Request: request}, nil
	}), NewParser())
	result, _, err := remote.FetchAndParse(context.Background(), "https://example.com/feed", ConditionalHeaders{})
	if err == nil {
		t.Fatal("FetchAndParse() error = nil")
	}
	if result.ETag != `"broken"` || result.FinalURL != "https://example.com/feed" {
		t.Fatalf("remote result lost on parse failure: %#v", result)
	}
}

func TestValidateURLRejectsCredentialsAndPrivateTargets(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"http://127.0.0.1/feed", "http://www.localhost/feed", "https://user:pass@example.com/feed", "file:///tmp/feed"} {
		if _, err := ValidateURL(raw); err == nil {
			t.Fatalf("ValidateURL(%q) error = nil", raw)
		}
	}
}

func TestRemoteFetchRejectsNormalizedLoopbackBeforeRequest(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(response, `<rss><channel><title>private</title></channel></rss>`)
	}))
	t.Cleanup(server.Close)

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, serverURL.Host)
	}
	t.Cleanup(transport.CloseIdleConnections)

	remote := NewRemote(&http.Client{Transport: transport}, NewParser())
	_, err = remote.Fetch(t.Context(), "http://www.localhost:"+serverURL.Port()+"/feed", ConditionalHeaders{})
	carrier, ok := httperr.As(err)
	if !ok || carrier.HTTPStatus() != http.StatusUnprocessableEntity {
		t.Fatalf("Fetch(www.localhost) error = %v, want 422 unsafe target", err)
	}
	coder, ok := carrier.(httperr.ErrorCoder)
	if !ok || coder.HTTPErrorCode() != "unsafe_feed_url_target" {
		t.Fatalf("Fetch(www.localhost) error code = %v, want unsafe_feed_url_target", coder)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("loopback server received %d requests, want zero", got)
	}
}

func TestValidateURLCanonicalizesEquivalentFeedURLs(t *testing.T) {
	t.Parallel()
	// The expected forms are whatever urlidentity.Normalize produces: feed URLs
	// are collection identities like any other, so the root path is "/" and a
	// leading www folds away. A subscription and a saved link for the same
	// address must not disagree about what that address is.
	tests := map[string]string{
		"https://EXAMPLE.com:443/#section":      "https://example.com/",
		"http://Example.COM:80?format=rss#x":    "http://example.com/?format=rss",
		"https://EXAMPLE.com:8443/feed":         "https://example.com:8443/feed",
		"https://www.example.com/feed/":         "https://example.com/feed",
		"https://example.com/feed?utm_source=x": "https://example.com/feed",
		"https://example.com/feed?b=2&a=1":      "https://example.com/feed?a=1&b=2",
	}
	for raw, want := range tests {
		got, err := ValidateURL(raw)
		if err != nil {
			t.Fatalf("ValidateURL(%q) error = %v", raw, err)
		}
		if got != want {
			t.Errorf("ValidateURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
