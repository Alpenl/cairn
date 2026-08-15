package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"webtag/internal/config"
)

func TestRuntimeHTTPClientsKeepFetcherSafeWhenAIUnsafeTargetsEnabled(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	httpClients := newRuntimeHTTPClientOwner()
	t.Cleanup(func() { _ = httpClients.Stop(context.Background()) })
	stack := httpClients.buildFetcherStack(config.Config{
		Fetcher: config.FetcherConfig{
			RetryAttempts: 2,
			RetryDelayMS:  25,
		},
		Analyzer: config.AnalyzerConfig{
			AllowUnsafeTargets: true,
		},
	}, nil)
	fetchClient, analyzerClient, visionClient := stack.fetchClient, stack.analyzerClient, stack.visionClient

	fetchReq, fetchCancel, err := fetchClient.NewRequest(context.Background(), time.Second, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("fetch NewRequest() error = %v", err)
	}
	defer fetchCancel()

	_, err = fetchClient.DoWithRetry(fetchReq)
	if err == nil {
		t.Fatal("fetch DoWithRetry() error = nil, want unsafe target rejection")
	}

	analyzerReq, analyzerCancel, err := analyzerClient.NewRequest(context.Background(), time.Second, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("analyzer NewRequest() error = %v", err)
	}
	defer analyzerCancel()

	resp, err := analyzerClient.Raw().Do(analyzerReq)
	if err != nil {
		t.Fatalf("analyzer Raw().Do() error = %v, want success with explicit AI unsafe target override", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	visionReq, visionCancel, err := visionClient.NewRequest(context.Background(), time.Second, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("vision NewRequest() error = %v", err)
	}
	defer visionCancel()
	if _, err := visionClient.Do(visionReq); err == nil {
		t.Fatal("vision Do() error = nil, want unsafe target rejection despite AI override")
	}
}

// TestBuildRuntimeReturnsErrorOnInvalidDatabaseURL covers the
// fail-fast path in BuildRuntime — a malformed DSN must surface as a
// constructor error before any other dependency is built. The path was
// previously at 0% coverage so a regression in error wrapping (e.g.
// returning a half-built Runtime alongside the error) would not have
// been caught.
func TestBuildRuntimeReturnsErrorOnInvalidDatabaseURL(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		DatabaseURL: "://broken-dsn",
	}

	runtime, err := BuildRuntime(context.Background(), cfg)
	if err == nil {
		t.Fatal("BuildRuntime returned nil error for malformed DSN")
	}
	if runtime != nil {
		t.Fatalf("BuildRuntime returned non-nil runtime alongside error: %+v", runtime)
	}
}

// TestRuntimeStartCloseAreNilSafe pins the defensive zero/nil guards
// on Runtime.Start and Runtime.Close so a caller wiring partial
// state never panics. These guards also protect a non-nil Runtime
// whose start/close closures were never installed.
func TestRuntimeStartCloseAreNilSafe(t *testing.T) {
	t.Parallel()

	var nilRuntime *Runtime
	if err := nilRuntime.Start(context.Background()); err != nil {
		t.Fatalf("nil Runtime Start = %v, want nil", err)
	}
	if err := nilRuntime.Close(context.Background()); err != nil {
		t.Fatalf("nil Runtime Close = %v, want nil", err)
	}

	emptyRuntime := &Runtime{}
	if err := emptyRuntime.Start(context.Background()); err != nil {
		t.Fatalf("empty Runtime Start = %v, want nil", err)
	}
	if err := emptyRuntime.Close(context.Background()); err != nil {
		t.Fatalf("empty Runtime Close = %v, want nil", err)
	}

	wantErr := errors.New("start failed")
	withFunc := &Runtime{
		start: func(context.Context) error { return wantErr },
		close: func(context.Context) error { return wantErr },
	}
	if err := withFunc.Start(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Start propagation = %v, want %v", err, wantErr)
	}
	if err := withFunc.Close(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Close propagation = %v, want %v", err, wantErr)
	}
}
