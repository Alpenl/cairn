package analyzer

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"webtag/internal/fetcher"
)

func TestAnalyzerDoesNotRetryCredentialedHTTPSDowngrade(t *testing.T) {
	t.Parallel()

	providerCalls := 0
	providerClient := fetcher.NewHTTPClientWithOptions(fetcher.HTTPClientOptions{
		AllowUnsafeTargets: true,
		Client: &http.Client{Transport: visionRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			providerCalls++
			if providerCalls > 1 {
				t.Fatalf("plaintext redirect reached transport with auth=%q", req.Header.Get("Authorization"))
			}
			header := make(http.Header)
			header.Set("Location", "http://provider.example/chat/completions")
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     header,
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Request:    req,
			}, nil
		})},
	}).Raw()
	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL: "https://provider.example", APIKey: "fictional-key", Model: "test",
		HTTPClient: providerClient, EmptyResponseRetries: 3,
	})

	_, err := a.Analyze(context.Background(), AnalyzeRequest{Content: fetcher.Content{
		URL: "https://example.com/post", Title: "Title", Body: "fictional sensitive body",
	}})
	if err == nil || !fetcher.IsUnsafeTargetError(err) {
		t.Fatalf("Analyze() error = %v, want non-retryable redirect security error", err)
	}
	if providerCalls != 1 {
		t.Fatalf("provider calls=%d, want only initial TLS request", providerCalls)
	}
	if strings.Contains(err.Error(), "fictional-key") || strings.Contains(err.Error(), "fictional sensitive body") {
		t.Fatalf("redirect error leaked credentials or body: %v", err)
	}
}
