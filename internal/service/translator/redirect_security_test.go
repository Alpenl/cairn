package translator

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

type translatorRedirectRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn translatorRedirectRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestTranslatorHTTPSDowngradeIsSanitizedAndNotReplayed(t *testing.T) {
	t.Parallel()

	calls := 0
	providerClient := fetcher.NewHTTPClientWithOptions(fetcher.HTTPClientOptions{
		AllowUnsafeTargets: true,
		Client: &http.Client{Transport: translatorRedirectRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls > 1 {
				t.Fatal("plaintext translation redirect reached transport")
			}
			header := make(http.Header)
			header.Set("Location", "http://other.example/chat/completions?token=redirect-secret")
			return &http.Response{
				StatusCode: http.StatusPermanentRedirect,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		})},
	}).Raw()
	client := NewOpenAITranslator(Options{
		BaseURL:        "https://provider.example",
		APIKey:         "fictional-translation-key",
		Model:          "translation-test",
		HTTPClient:     providerClient,
		RequestTimeout: time.Second,
	})

	_, err := client.Translate(context.Background(), Request{
		Text:   "fictional sensitive translation input",
		Format: FormatPlain,
	})
	if err == nil || !errors.Is(err, fetcher.ErrUnsafeRedirect) || !fetcher.IsUnsafeTargetError(err) {
		t.Fatalf("Translate() error = %v, want typed unsafe redirect", err)
	}
	if calls != 1 {
		t.Fatalf("provider calls=%d, want one TLS request", calls)
	}
	for _, secret := range []string{
		"fictional-translation-key",
		"fictional sensitive translation input",
		"redirect-secret",
		"other.example",
	} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("translation error leaked %q: %v", secret, err)
		}
	}
}
