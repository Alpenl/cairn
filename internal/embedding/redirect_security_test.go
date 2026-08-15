package embedding

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"webtag/internal/fetcher"
)

type embeddingRedirectRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn embeddingRedirectRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestEmbeddingHTTPSDowngradeIsSanitizedAndNotRetried(t *testing.T) {
	t.Parallel()

	calls := 0
	providerClient := fetcher.NewHTTPClientWithOptions(fetcher.HTTPClientOptions{
		AllowUnsafeTargets: true,
		Client: &http.Client{Transport: embeddingRedirectRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls > 1 {
				t.Fatal("plaintext embedding redirect reached transport")
			}
			header := make(http.Header)
			header.Set("Location", "http://other.example/embeddings?token=redirect-secret")
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		})},
	}).Raw()
	client := NewClient(Options{
		BaseURL:        "https://provider.example",
		APIKey:         "fictional-embedding-key",
		Model:          "embedding-test",
		Dimensions:     1,
		HTTPClient:     providerClient,
		RequestTimeout: time.Second,
		RetryAttempts:  3,
		RetryDelay:     time.Millisecond,
	})

	_, err := client.Embed(context.Background(), []string{"fictional sensitive embedding input"})
	if err == nil || !errors.Is(err, fetcher.ErrUnsafeRedirect) || !fetcher.IsUnsafeTargetError(err) {
		t.Fatalf("Embed() error = %v, want typed unsafe redirect", err)
	}
	if calls != 1 {
		t.Fatalf("provider calls=%d, want one TLS request", calls)
	}
	for _, secret := range []string{
		"fictional-embedding-key",
		"fictional sensitive embedding input",
		"redirect-secret",
		"other.example",
	} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("embedding error leaked %q: %v", secret, err)
		}
	}
}
