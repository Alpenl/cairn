package embedding

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"webtag/internal/fetcher"
	"webtag/internal/jsonx"
)

// newTestClient wires the embeddings client to a httptest server through
// fetcher's HTTP client with AllowUnsafeTargets so the loopback test server
// is reachable (the SSRF guard otherwise blocks 127.0.0.1).
func newTestClient(t *testing.T, serverURL string, opts Options) *Client {
	t.Helper()
	if opts.HTTPClient == nil {
		opts.HTTPClient = fetcher.NewHTTPClientWithOptions(fetcher.HTTPClientOptions{
			AllowUnsafeTargets: true,
		}).Raw()
	}
	if opts.Model == "" {
		opts.Model = "text-embedding-3-small"
	}
	opts.BaseURL = serverURL
	return NewClient(opts)
}

// encodeEmbeddings writes an OpenAI-shaped /embeddings response.
func encodeEmbeddings(w http.ResponseWriter, vectors map[int][]float32) {
	w.Header().Set("Content-Type", "application/json")
	type entry struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	data := make([]entry, 0, len(vectors))
	for idx, vec := range vectors {
		data = append(data, entry{Index: idx, Embedding: vec})
	}
	body, _ := jsonx.Marshal(map[string]any{"data": data})
	_, _ = w.Write(body)
}

func TestEmbedHappyPath(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %q, want /embeddings", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer embed-key" {
			t.Errorf("Authorization = %q, want Bearer embed-key", got)
		}
		encodeEmbeddings(w, map[int][]float32{
			0: {0.1, 0.2, 0.3},
			1: {0.4, 0.5, 0.6},
		})
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, Options{APIKey: "embed-key", Dimensions: 3})
	got, err := c.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(vectors) = %d, want 2", len(got))
	}
	if got[0][0] != 0.1 || got[1][2] != 0.6 {
		t.Fatalf("vectors = %v, want positional alignment", got)
	}
}

// TestEmbedPreservesInputOrder feeds the response back out of order and
// confirms the client re-sorts by index into input order.
func TestEmbedPreservesInputOrder(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately emit index 2, 0, 1 to exercise the re-sort.
		encodeEmbeddings(w, map[int][]float32{
			2: {0.2},
			0: {0.0},
			1: {0.1},
		})
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, Options{Dimensions: 1})
	got, err := c.Embed(context.Background(), []string{"x", "y", "z"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	for i := range got {
		if got[i][0] != float32(i)/10 {
			t.Fatalf("vectors[%d] = %v, want index-aligned %v", i, got[i], float32(i)/10)
		}
	}
}

func TestEmbedTimeout(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, Options{
		Dimensions:     3,
		RequestTimeout: 50 * time.Millisecond,
		RetryAttempts:  2,
		RetryDelay:     time.Millisecond,
	})
	_, err := c.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("Embed() error = nil, want timeout failure")
	}
}

func TestEmbedEmptyVector(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encodeEmbeddings(w, map[int][]float32{0: {}})
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, Options{Dimensions: 3, RetryAttempts: 1})
	_, err := c.Embed(context.Background(), []string{"a"})
	if !errors.Is(err, ErrEmptyVector) {
		t.Fatalf("Embed() error = %v, want ErrEmptyVector", err)
	}
}

func TestEmbedDimensionMismatch(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encodeEmbeddings(w, map[int][]float32{0: {0.1, 0.2}}) // 2 dims, want 3
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, Options{Dimensions: 3, RetryAttempts: 1})
	_, err := c.Embed(context.Background(), []string{"a"})
	if !errors.Is(err, ErrDimensionMismatch) {
		t.Fatalf("Embed() error = %v, want ErrDimensionMismatch", err)
	}
}

func TestEmbedCountMismatch(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encodeEmbeddings(w, map[int][]float32{0: {0.1, 0.2, 0.3}}) // 1 vec for 2 inputs
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, Options{Dimensions: 3, RetryAttempts: 1})
	_, err := c.Embed(context.Background(), []string{"a", "b"})
	if !errors.Is(err, ErrCountMismatch) {
		t.Fatalf("Embed() error = %v, want ErrCountMismatch", err)
	}
}

// TestEmbedRetriesOn5xx confirms the client retries a transient 500 and
// succeeds on the second attempt.
func TestEmbedRetriesOn5xx(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		encodeEmbeddings(w, map[int][]float32{0: {0.1, 0.2, 0.3}})
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, Options{
		Dimensions:    3,
		RetryAttempts: 3,
		RetryDelay:    time.Millisecond,
	})
	got, err := c.Embed(context.Background(), []string{"a"})
	if err != nil {
		t.Fatalf("Embed() error = %v, want success after retry", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (1 fail + 1 success)", calls.Load())
	}
	if len(got) != 1 {
		t.Fatalf("len(vectors) = %d, want 1", len(got))
	}
}

// TestEmbedDoesNotRetry4xx confirms a 400 fails fast without burning retries.
func TestEmbedDoesNotRetry4xx(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, Options{
		Dimensions:    3,
		RetryAttempts: 3,
		RetryDelay:    time.Millisecond,
	})
	_, err := c.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("Embed() error = nil, want 4xx failure")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (4xx is non-retryable)", calls.Load())
	}
}

// TestEmbedRejectsUnsafeTargetByDefault confirms the default hardened client
// blocks the loopback test server (SSRF guard) and never hits it.
func TestEmbedRejectsUnsafeTargetByDefault(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Error("server should not receive request from default hardened client")
	}))
	defer server.Close()

	// Default HTTPClient (no AllowUnsafeTargets) — must reject 127.0.0.1.
	c := NewClient(Options{
		Model:         "text-embedding-3-small",
		BaseURL:       server.URL,
		Dimensions:    3,
		RetryAttempts: 1,
	})
	_, err := c.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("Embed() error = nil, want unsafe target rejection")
	}
	if !fetcher.IsUnsafeTargetError(err) {
		t.Fatalf("Embed() error = %v, want IsUnsafeTargetError", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("calls = %d, want 0 (blocked before dial)", calls.Load())
	}
}

func TestEmbedOversizedResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Larger than the pinned MaxResponseBytes below.
		_, _ = w.Write(make([]byte, 512))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL, Options{
		Dimensions:       3,
		RetryAttempts:    1,
		MaxResponseBytes: 64,
	})
	_, err := c.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("Embed() error = nil, want oversized response rejection")
	}
}

func TestEmbedDisabledWhenNoModel(t *testing.T) {
	t.Parallel()

	c := NewClient(Options{BaseURL: "https://example.com/v1", Dimensions: 3})
	if c.Enabled() {
		t.Fatal("Enabled() = true, want false for empty model")
	}
	_, err := c.Embed(context.Background(), []string{"a"})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("Embed() error = %v, want ErrDisabled", err)
	}
}

func TestEmbedEmptyInput(t *testing.T) {
	t.Parallel()

	c := NewClient(Options{Model: "m", BaseURL: "https://example.com/v1", Dimensions: 3})
	_, err := c.Embed(context.Background(), nil)
	if !errors.Is(err, ErrEmptyInput) {
		t.Fatalf("Embed() error = %v, want ErrEmptyInput", err)
	}
}

// TestEmbedContextCanceledNotRetried confirms a canceled context fails fast.
func TestEmbedContextCanceledNotRetried(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := newTestClient(t, server.URL, Options{Dimensions: 3, RetryAttempts: 3, RetryDelay: time.Millisecond})
	_, err := c.Embed(ctx, []string{"a"})
	if err == nil {
		t.Fatal("Embed() error = nil, want canceled-context failure")
	}
	if calls.Load() != 0 {
		t.Fatalf("calls = %d, want 0 for pre-canceled context", calls.Load())
	}
}
