package fetcher

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"webtag/internal/observability"
)

func TestFixedHostMetricsTransportDoesNotLabelUntrustedImageHosts(t *testing.T) {
	t.Parallel()

	metrics := observability.NewMetrics()
	transport := NewFixedHostMetricsTransport(
		roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		}),
		metrics,
		"vision",
		"untrusted_image",
	)

	for _, rawURL := range []string{
		"https://signed.attacker-one.example/image.png?token=first-secret",
		"https://cdn.attacker-two.example/photo.jpg?token=second-secret",
	} {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip() error = %v", err)
		}
		resp.Body.Close()
	}

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	if !strings.Contains(body, `webtag_fetcher_duration_seconds_count{fetcher_type="vision",host_class="untrusted_image"} 2`) {
		t.Fatalf("metrics body missing fixed vision host class:\n%s", body)
	}
	for _, forbidden := range []string{"attacker-one", "attacker-two", "first-secret", "second-secret"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("metrics body leaked %q:\n%s", forbidden, body)
		}
	}
}
