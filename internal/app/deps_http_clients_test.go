package app

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"webtag/internal/config"
	"webtag/internal/fetcher"
	"webtag/internal/service/analyzer"
	"webtag/internal/service/translator"
)

func TestBuildFetcherStackManagerUsesRuntimeOwnedHTTPClient(t *testing.T) {
	t.Parallel()

	marker, owner := newRuntimeHTTPMarkerOwner()
	cfg := runtimeHTTPMarkerConfig()
	stack := owner.buildFetcherStack(cfg)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _ = stack.manager.Fetch(ctx, runtimeHTTPMarkerBaseURL+"/article")
	marker.assertSaw(t, http.MethodGet, runtimeHTTPMarkerBaseURL+"/article")
}

func TestBuildAnalyzerUsesRuntimeOwnedHTTPClient(t *testing.T) {
	t.Parallel()

	marker, owner := newRuntimeHTTPMarkerOwner()
	cfg := runtimeHTTPMarkerConfig()
	stack := owner.buildFetcherStack(cfg)
	client := buildAnalyzer(cfg, stack.analyzerClient, stack.visionClient, nil)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _ = client.Analyze(ctx, analyzer.AnalyzeRequest{
		Content:              fetcher.Content{URL: "https://example.com/article", Title: "Title", Body: "Body"},
		SystemPromptOverride: "Return JSON.",
	})
	marker.assertSaw(t, http.MethodPost, runtimeHTTPMarkerBaseURL+"/chat/completions")
}

func TestBuildAnalyzerVisionUsesDedicatedRuntimeOwnedHTTPClient(t *testing.T) {
	t.Parallel()

	marker, owner := newRuntimeHTTPMarkerOwner()
	cfg := runtimeHTTPMarkerConfig()
	stack := owner.buildFetcherStack(cfg)
	client := buildAnalyzer(cfg, stack.analyzerClient, stack.visionClient, nil)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _ = client.Analyze(ctx, analyzer.AnalyzeRequest{
		Content: fetcher.Content{
			URL:       "https://example.com/article",
			Title:     "Title",
			Body:      "Body",
			ImageURLs: []string{runtimeHTTPMarkerBaseURL + "/image.png?signed=secret"},
		},
		SystemPromptOverride: "Return JSON.",
	})
	marker.assertSaw(t, http.MethodGet, runtimeHTTPMarkerBaseURL+"/image.png?signed=secret")
}

func TestBuildTranslatorUsesRuntimeOwnedHTTPClient(t *testing.T) {
	t.Parallel()

	marker, owner := newRuntimeHTTPMarkerOwner()
	cfg := runtimeHTTPMarkerConfig()
	_, analyzerClient := owner.newRuntimeHTTPClients(cfg)
	client := buildTranslator(cfg, analyzerClient)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _ = client.Translate(ctx, translator.Request{Text: "hello world", Format: translator.FormatPlain})
	marker.assertSaw(t, http.MethodPost, runtimeHTTPMarkerBaseURL+"/chat/completions")
}

func TestRuntimeHTTPClientOwnerClosesEveryTransportInReverseOrderOnce(t *testing.T) {
	t.Parallel()

	events := make([]string, 0, 3)
	owner := newRuntimeHTTPClientOwner()
	owner.register(idleConnectionCloseProbe{name: "fetch", events: &events})
	owner.register(idleConnectionCloseProbe{name: "analyzer", events: &events})
	owner.register(idleConnectionCloseProbe{name: "vision", events: &events})

	if err := owner.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := owner.Stop(t.Context()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	want := []string{"vision", "analyzer", "fetch"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("close events = %v, want %v", events, want)
	}
	if err := owner.Stop(t.Context()); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("close events after idempotent Stop = %v, want unchanged %v", events, want)
	}
}

func TestRuntimeHTTPClientOwnerClosesTransportsEvenAfterDeadline(t *testing.T) {
	t.Parallel()

	closed := false
	owner := newRuntimeHTTPClientOwner()
	owner.register(idleConnectionCloserFunc(func() { closed = true }))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := owner.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if !closed {
		t.Fatal("Stop() skipped transport cleanup after its context ended")
	}
}

func TestRuntimeHTTPClientOwnerRejectsRegistrationAfterStop(t *testing.T) {
	t.Parallel()

	owner := newRuntimeHTTPClientOwner()
	if err := owner.Stop(t.Context()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	closed := false
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("register after Stop did not panic")
		}
		if !closed {
			t.Fatal("register after Stop did not release the rejected transport")
		}
	}()
	owner.register(idleConnectionCloserFunc(func() { closed = true }))
}

type idleConnectionCloseProbe struct {
	name   string
	events *[]string
}

func (p idleConnectionCloseProbe) CloseIdleConnections() {
	*p.events = append(*p.events, p.name)
}

type idleConnectionCloserFunc func()

func (f idleConnectionCloserFunc) CloseIdleConnections() {
	f()
}

const runtimeHTTPMarkerBaseURL = "http://8.8.8.8"

var errRuntimeHTTPMarker = errors.New("runtime HTTP owner marker transport reached")

type runtimeHTTPMarkerRequest struct {
	method string
	url    string
}

type runtimeHTTPMarkerTransport struct {
	mu       sync.Mutex
	requests []runtimeHTTPMarkerRequest
}

func (m *runtimeHTTPMarkerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	m.mu.Lock()
	m.requests = append(m.requests, runtimeHTTPMarkerRequest{method: request.Method, url: request.URL.String()})
	m.mu.Unlock()
	return nil, errRuntimeHTTPMarker
}

func (m *runtimeHTTPMarkerTransport) assertSaw(t *testing.T, method, url string) {
	t.Helper()
	m.mu.Lock()
	requests := append([]runtimeHTTPMarkerRequest(nil), m.requests...)
	m.mu.Unlock()
	for _, request := range requests {
		if request.method == method && request.url == url {
			return
		}
	}
	t.Fatalf("runtime-owned transport requests = %v, want %s %s", requests, method, url)
}

func newRuntimeHTTPMarkerOwner() (*runtimeHTTPMarkerTransport, *runtimeHTTPClientOwner) {
	marker := &runtimeHTTPMarkerTransport{}
	owner := newRuntimeHTTPClientOwner()
	owner.newClient = func(options fetcher.HTTPClientOptions) *fetcher.HTTPClient {
		options.Client = &http.Client{Transport: marker}
		return fetcher.NewHTTPClientWithOptions(options)
	}
	return marker, owner
}

func runtimeHTTPMarkerConfig() config.Config {
	return config.Config{
		Fetcher: config.FetcherConfig{RetryAttempts: 1, RetryDelayMS: 1},
		Analyzer: config.AnalyzerConfig{
			BaseURL:          runtimeHTTPMarkerBaseURL,
			Model:            "rf7-analyzer",
			RetryAttempts:    1,
			RetryDelayMS:     1,
			RequestTimeoutMS: 500,
		},
	}
}
