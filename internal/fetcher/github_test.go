package fetcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func TestGitHubFetcherReadmeTruncationPreservesUTF8(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/example/project":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"full_name":"example/project","description":"desc","language":"Go","stargazers_count":1,"forks_count":1,"topics":["parser"]}`))
		case "/repos/example/project/readme":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("你好世界再见"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	fetcher := NewGitHubFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}), "")
	fetcher.APIBaseURL = server.URL
	fetcher.ReadmeMaxChars = 4

	got, err := fetcher.Fetch(context.Background(), "https://github.com/example/project")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !strings.Contains(got.Body, "你好世界") {
		t.Fatalf("Body = %q, want rune-safe README truncation", got.Body)
	}
	if !utf8.ValidString(got.Body) {
		t.Fatalf("Body = %q, want valid UTF-8", got.Body)
	}
}

func TestGitHubFetcherRetriesTransientRepositoryFailures(t *testing.T) {
	t.Parallel()

	var repoCalls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/example/project":
			if repoCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"full_name":"example/project","description":"desc","language":"Go","stargazers_count":1,"forks_count":1,"topics":["parser"]}`))
		case "/repos/example/project/readme":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("Recovered README"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	fetcher := NewGitHubFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}), "")
	fetcher.APIBaseURL = server.URL

	got, err := fetcher.Fetch(context.Background(), "https://github.com/example/project")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if repoCalls.Load() != 2 {
		t.Fatalf("repo HTTP calls = %d, want 2", repoCalls.Load())
	}
	if !strings.Contains(got.Body, "Recovered README") {
		t.Fatalf("Body = %q, want recovered README", got.Body)
	}
}

func TestGitHubFetcherRejectsUnexpectedRepositoryContentType(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/example/project":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(`{"full_name":"example/project"}`))
		case "/repos/example/project/readme":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("README"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	fetcher := NewGitHubFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}), "")
	fetcher.APIBaseURL = server.URL

	_, err := fetcher.Fetch(context.Background(), "https://github.com/example/project")
	if err == nil {
		t.Fatal("Fetch() error = nil, want repository content-type rejection")
	}
	if !strings.Contains(err.Error(), "content-type") {
		t.Fatalf("Fetch() error = %v, want content-type error", err)
	}
}

func TestGitHubFetcherRejectsOversizedRepositoryResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/example/project":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(strings.Repeat("A", 256)))
		case "/repos/example/project/readme":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("README"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	fetcher := NewGitHubFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}), "")
	fetcher.APIBaseURL = server.URL
	fetcher.RepositoryMaxBytes = 32

	_, err := fetcher.Fetch(context.Background(), "https://github.com/example/project")
	if err == nil {
		t.Fatal("Fetch() error = nil, want oversized repository response rejection")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("Fetch() error = %v, want oversized response error", err)
	}
}

func TestGitHubFetcherSkipsUnsupportedReadmeContentType(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/example/project":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"full_name":"example/project","description":"desc","language":"Go","stargazers_count":1,"forks_count":1,"topics":["parser"]}`))
		case "/repos/example/project/readme":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("binary-readme"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	fetcher := NewGitHubFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}), "")
	fetcher.APIBaseURL = server.URL

	got, err := fetcher.Fetch(context.Background(), "https://github.com/example/project")
	if err != nil {
		t.Fatalf("Fetch() error = %v, want success with README skipped", err)
	}
	if strings.Contains(got.Body, "## README") {
		t.Fatalf("Body = %q, want unsupported README content omitted", got.Body)
	}
	if !strings.Contains(got.Body, "desc") {
		t.Fatalf("Body = %q, want repository metadata retained", got.Body)
	}
}
