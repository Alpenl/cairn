package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"webtag/internal/config"
	"webtag/internal/fetcher"
	"webtag/internal/service/analyzer"
	"webtag/internal/service/translator"
)

func TestRuntimeHTTPClientOwnerStopClosesTransportCreatedByNew(t *testing.T) {
	states := make(chan http.ConnState, 8)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		states <- state
	}
	server.Start()
	t.Cleanup(server.Close)

	owner := newRuntimeHTTPClientOwner()
	client := owner.New(fetcher.HTTPClientOptions{AllowUnsafeTargets: true})
	response, err := client.Raw().Get(server.URL)
	if err != nil {
		t.Fatalf("owned client GET error = %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
	waitForHTTPConnectionState(t, states, http.StateIdle)

	if err := owner.Stop(t.Context()); err != nil {
		t.Fatalf("owner Stop() error = %v", err)
	}
	waitForHTTPConnectionState(t, states, http.StateClosed)
}

func TestBuildRuntimeCreatesEveryOutboundTransportWithAcquiredOwner(t *testing.T) {
	_, owner := newRuntimeHTTPMarkerOwner()
	probe := newProductionRuntimeBuildProbe()
	options := probe.options()
	options.newHTTPClientOwner = func() *runtimeHTTPClientOwner { return owner }
	cfg := productionRuntimeMatrixConfig()

	runtime, err := buildRuntimeWithOptions(t.Context(), cfg, options)
	if err != nil {
		t.Fatalf("buildRuntimeWithOptions() error = %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = runtime.Close(cleanupCtx)
	})

	owner.mu.Lock()
	created := len(owner.clients)
	owner.mu.Unlock()
	// Fetch, vision, analyzer/translator, and embedding are four independently
	// configured transports. Missing any one means a production handoff used a
	// different owner even if the local helper test still passed.
	if created != 4 {
		t.Fatalf("acquired Runtime HTTP owner created %d transports, want 4", created)
	}
}

func TestBuildFetcherStackManagerUsesRuntimeOwnedHTTPClient(t *testing.T) {
	t.Parallel()

	marker, owner := newRuntimeHTTPMarkerOwner()
	cfg := runtimeHTTPMarkerConfig()
	stack := owner.buildFetcherStack(cfg, nil)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _ = stack.manager.FetchLight(ctx, runtimeHTTPMarkerBaseURL+"/article")
	marker.assertSaw(t, http.MethodGet, runtimeHTTPMarkerBaseURL+"/article")
}

func TestBuildAnalyzerUsesRuntimeOwnedHTTPClient(t *testing.T) {
	t.Parallel()

	marker, owner := newRuntimeHTTPMarkerOwner()
	cfg := runtimeHTTPMarkerConfig()
	stack := owner.buildFetcherStack(cfg, nil)
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
	stack := owner.buildFetcherStack(cfg, nil)
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
	_, analyzerClient := owner.newRuntimeHTTPClients(cfg, nil)
	client := buildTranslator(cfg, analyzerClient)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _ = client.Translate(ctx, translator.Request{Text: "hello world", Format: translator.FormatPlain})
	marker.assertSaw(t, http.MethodPost, runtimeHTTPMarkerBaseURL+"/chat/completions")
}

func TestBuildEmbeddingClientUsesRuntimeOwnedHTTPClient(t *testing.T) {
	t.Parallel()

	marker, owner := newRuntimeHTTPMarkerOwner()
	cfg := runtimeHTTPMarkerConfig()
	client := owner.buildEmbeddingClient(cfg, nil)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _ = client.Embed(ctx, []string{"hello"})
	marker.assertSaw(t, http.MethodPost, runtimeHTTPMarkerBaseURL+"/embeddings")
}

func TestRuntimeHTTPClientOwnerClosesEveryTransportInReverseOrderOnce(t *testing.T) {
	t.Parallel()

	events := make([]string, 0, 3)
	owner := newRuntimeHTTPClientOwner()
	owner.register(idleConnectionCloseProbe{name: "fetch", events: &events})
	owner.register(idleConnectionCloseProbe{name: "analyzer", events: &events})
	owner.register(idleConnectionCloseProbe{name: "embedding", events: &events})

	if err := owner.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := owner.Stop(t.Context()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	want := []string{"embedding", "analyzer", "fetch"}
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

	if err := owner.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop() error = %v, want context.Canceled", err)
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
			BaseURL:                 runtimeHTTPMarkerBaseURL,
			Model:                   "rf7-analyzer",
			RetryAttempts:           1,
			RetryDelayMS:            1,
			RequestTimeoutMS:        500,
			DisableStructuredOutput: true,
		},
		Embedding: config.EmbeddingConfig{
			BaseURL:          runtimeHTTPMarkerBaseURL,
			Model:            "rf7-embedding",
			Dimensions:       1,
			RetryAttempts:    1,
			RetryDelayMS:     1,
			RequestTimeoutMS: 500,
		},
	}
}

func waitForHTTPConnectionState(t *testing.T, states <-chan http.ConnState, want http.ConnState) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case state := <-states:
			if state == want {
				return
			}
		case <-timer.C:
			t.Fatalf("HTTP connection did not reach state %s", want)
		}
	}
}
