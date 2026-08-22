package fetcher

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestHTTPClientLimitsRedirects(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	defer server.Close()
	client := NewHTTPClientWithOptions(HTTPClientOptions{
		Client: server.Client(), allowUnsafeTargets: true,
	})
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/loop", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	response, err := client.Do(request)
	if response != nil {
		response.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("Do() error = %v, want redirect limit", err)
	}
}

type lookupIPFunc func(context.Context, string) ([]net.IPAddr, error)

func (fn lookupIPFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return fn(ctx, host)
}

func TestHTTPClientDoWithRetryWaitsBeforeRetryingRetryableResponses(t *testing.T) {
	t.Parallel()

	start := time.Now()
	callTimes := make([]time.Time, 0, 2)

	client := NewHTTPClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			callTimes = append(callTimes, time.Now())
			if len(callTimes) == 1 {
				return &http.Response{
					StatusCode: http.StatusBadGateway,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		}),
	})

	req, cancel, err := client.NewRequest(context.Background(), time.Second, http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	defer cancel()

	resp, err := client.DoWithRetry(req)
	if err != nil {
		t.Fatalf("DoWithRetry() error = %v", err)
	}
	defer resp.Body.Close()

	if len(callTimes) != 2 {
		t.Fatalf("call count = %d, want 2", len(callTimes))
	}
	if callTimes[1].Sub(callTimes[0]) < 10*time.Millisecond {
		t.Fatalf("retry delay = %v, want at least 10ms", callTimes[1].Sub(callTimes[0]))
	}
	if time.Since(start) < 10*time.Millisecond {
		t.Fatalf("elapsed = %v, want visible retry delay", time.Since(start))
	}
}

func TestHTTPClientDoWithRetryHonorsRetryAfterHeader(t *testing.T) {
	t.Parallel()

	callTimes := make([]time.Time, 0, 2)

	client := NewHTTPClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			callTimes = append(callTimes, time.Now())
			if len(callTimes) == 1 {
				header := make(http.Header)
				header.Set("Retry-After", "1")
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     header,
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		}),
	})

	req, cancel, err := client.NewRequest(context.Background(), 2*time.Second, http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	defer cancel()

	resp, err := client.DoWithRetry(req)
	if err != nil {
		t.Fatalf("DoWithRetry() error = %v", err)
	}
	defer resp.Body.Close()

	if len(callTimes) != 2 {
		t.Fatalf("call count = %d, want 2", len(callTimes))
	}
	// Retry-After: 1 second, jittered +/-20% by the shared retry policy, so expect a
	// delay in [800ms, 1200ms). Threshold sits below the lower bound to
	// stay robust against scheduler noise but still well above the
	// fallback ~25ms exponential backoff so a regression that ignores
	// Retry-After entirely fails this assertion. Wave 11 M2 added the
	// jitter to defang thundering herds when N workers all see the same
	// 429 + Retry-After at the same wall-clock instant.
	if callTimes[1].Sub(callTimes[0]) < 700*time.Millisecond {
		t.Fatalf("retry delay = %v, want Retry-After-driven delay", callTimes[1].Sub(callTimes[0]))
	}
}

func TestHTTPClientDoWithRetryStopsWhenContextIsCancelledDuringBackoff(t *testing.T) {
	t.Parallel()

	client := NewHTTPClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}),
	})
	client.retryDelay = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	req, reqCancel, err := client.NewRequest(ctx, time.Second, http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	defer reqCancel()

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err = client.DoWithRetry(req)
	if err == nil {
		t.Fatal("DoWithRetry() error = nil, want context cancellation")
	}
	if time.Since(start) >= 150*time.Millisecond {
		t.Fatalf("elapsed = %v, want cancellation to interrupt backoff", time.Since(start))
	}
}

func TestNewHTTPClientWithOptionsAppliesRetrySettings(t *testing.T) {
	t.Parallel()

	client := NewHTTPClientWithOptions(HTTPClientOptions{
		RetryAttempts: 4,
		RetryDelay:    123 * time.Millisecond,
	})

	if client.retryAttempts != 4 {
		t.Fatalf("retryAttempts = %d, want 4", client.retryAttempts)
	}
	if client.retryDelay != 123*time.Millisecond {
		t.Fatalf("retryDelay = %v, want 123ms", client.retryDelay)
	}
}

func TestHTTPClientOwnsOnlyItsConstructedTransportCleanup(t *testing.T) {
	t.Parallel()

	owned := NewHTTPClientWithOptions(HTTPClientOptions{})
	if owned.closeIdleConnections == nil {
		t.Fatal("owned HTTP client did not register transport cleanup")
	}
	owned.CloseIdleConnections()

	external := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	})}
	borrowed := NewHTTPClientWithOptions(HTTPClientOptions{Client: external})
	if borrowed.closeIdleConnections != nil {
		t.Fatal("caller-supplied HTTP client was incorrectly claimed by runtime cleanup")
	}
	borrowed.CloseIdleConnections()

	closeCalls := 0
	probe := &HTTPClient{
		client:               &http.Client{},
		closeIdleConnections: func() { closeCalls++ },
	}
	probe.CloseIdleConnections()
	probe.Raw().CloseIdleConnections()
	if closeCalls != 2 {
		t.Fatalf("owned transport close calls = %d, want direct and Raw delegation", closeCalls)
	}
}

func TestHTTPClientRejectsUnsafeLoopbackTargetsByDefault(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not receive request for blocked loopback target")
	}))
	defer server.Close()

	client := NewHTTPClient(server.Client())
	req, cancel, err := client.NewRequest(context.Background(), time.Second, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	defer cancel()

	_, err = client.DoWithRetry(req)
	if err == nil {
		t.Fatal("DoWithRetry() error = nil, want unsafe target rejection")
	}
	if !strings.Contains(err.Error(), "unsafe target host") {
		t.Fatalf("DoWithRetry() error = %v, want unsafe target host error", err)
	}
}

func TestHTTPClientDoRejectsUnsafeLoopbackTargetsByDefault(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not receive request for blocked loopback target")
	}))
	defer server.Close()

	client := NewHTTPClient(server.Client())
	req, cancel, err := client.NewRequest(context.Background(), time.Second, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	defer cancel()

	_, err = client.Do(req)
	if err == nil {
		t.Fatal("Do() error = nil, want unsafe target rejection")
	}
	if !strings.Contains(err.Error(), "unsafe target host") {
		t.Fatalf("Do() error = %v, want unsafe target host error", err)
	}
}

func TestHTTPClientAllowsUnsafeTargetsWhenExplicitlyEnabled(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	client := NewHTTPClientWithOptions(HTTPClientOptions{
		Client:             server.Client(),
		allowUnsafeTargets: true,
	})
	req, cancel, err := client.NewRequest(context.Background(), time.Second, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	defer cancel()

	resp, err := client.DoWithRetry(req)
	if err != nil {
		t.Fatalf("DoWithRetry() error = %v, want success with unsafe targets explicitly enabled", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHTTPClientRejectsHostsResolvingToUnsafeIPs(t *testing.T) {
	t.Parallel()

	client := NewHTTPClientWithOptions(HTTPClientOptions{
		Client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				t.Fatal("transport should not run for blocked resolved target")
				return nil, nil
			}),
		},
		LookupIP: lookupIPFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}),
	})

	req, cancel, err := client.NewRequest(context.Background(), time.Second, http.MethodGet, "https://safe.example/path", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	defer cancel()

	_, err = client.DoWithRetry(req)
	if err == nil {
		t.Fatal("DoWithRetry() error = nil, want unsafe resolved target rejection")
	}
	if !strings.Contains(err.Error(), "unsafe target host") {
		t.Fatalf("DoWithRetry() error = %v, want unsafe target host error", err)
	}
}

func TestHTTPClientFailsClosedOnDNSError(t *testing.T) {
	t.Parallel()

	dnsErr := errors.New("nxdomain")
	client := NewHTTPClientWithOptions(HTTPClientOptions{
		Client: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("transport should not run when DNS lookup fails")
				return nil, nil
			}),
		},
		LookupIP: lookupIPFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return nil, dnsErr
		}),
	})

	req, cancel, err := client.NewRequest(context.Background(), time.Second, http.MethodGet, "https://safe.example/path", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	defer cancel()

	_, err = client.Do(req)
	if err == nil {
		t.Fatal("Do() error = nil, want fail-closed DNS rejection")
	}
	if !strings.Contains(err.Error(), "unsafe target host") {
		t.Fatalf("Do() error = %v, want unsafe target host error", err)
	}
	if !strings.Contains(err.Error(), "dns lookup") {
		t.Fatalf("Do() error = %v, want DNS lookup mention so the operator can debug", err)
	}
}

func TestHTTPClientDialerRejectsLateUnsafeIPSwap(t *testing.T) {
	t.Parallel()

	// Simulate a DNS rebinding scenario: the pre-flight LookupIPAddr is invoked
	// twice — first by validateRequestTarget (returns a safe address so the
	// initial check passes), then again from inside the safe DialContext (now
	// returns a loopback address). The dialer must reject the swapped reply
	// and never connect to the loopback IP.
	calls := 0
	client := NewHTTPClientWithOptions(HTTPClientOptions{
		LookupIP: lookupIPFunc(func(context.Context, string) ([]net.IPAddr, error) {
			calls++
			if calls == 1 {
				return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
			}
			return []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, nil
		}),
	})

	req, cancel, err := client.NewRequest(context.Background(), time.Second, http.MethodGet, "https://safe.example/path", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	defer cancel()

	_, err = client.Do(req)
	if err == nil {
		t.Fatal("Do() error = nil, want unsafe-after-swap rejection")
	}
	if !strings.Contains(err.Error(), "unsafe target host") {
		t.Fatalf("Do() error = %v, want unsafe target host error", err)
	}
	if calls < 2 {
		t.Fatalf("LookupIP calls = %d, want at least 2 (pre-flight + dialer)", calls)
	}
}

func TestHTTPClientRejectsRedirectsToUnsafeTargets(t *testing.T) {
	t.Parallel()

	var calls int
	client := NewHTTPClientWithOptions(HTTPClientOptions{
		Client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				switch calls {
				case 1:
					header := make(http.Header)
					header.Set("Location", "http://127.0.0.1/private")
					return &http.Response{
						StatusCode: http.StatusFound,
						Header:     header,
						Body:       io.NopCloser(strings.NewReader("")),
						Request:    req,
					}, nil
				default:
					t.Fatalf("unexpected redirected request to %s", req.URL.String())
					return nil, nil
				}
			}),
		},
		// Stub LookupIP so the fail-closed pre-flight resolver does not reject
		// the synthetic safe.example hostname before the redirect arrives.
		LookupIP: lookupIPFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}),
	})

	req, cancel, err := client.NewRequest(context.Background(), time.Second, http.MethodGet, "https://safe.example/path", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	defer cancel()

	_, err = client.DoWithRetry(req)
	if err == nil {
		t.Fatal("DoWithRetry() error = nil, want unsafe redirect rejection")
	}
	if !strings.Contains(err.Error(), "unsafe target host") {
		t.Fatalf("DoWithRetry() error = %v, want unsafe target host error", err)
	}
	if calls != 1 {
		t.Fatalf("transport calls = %d, want 1 before redirect was blocked", calls)
	}
}

func TestHTTPClientRejectsHTTPSDowngradeBeforeReplayingCredentialsOrBody(t *testing.T) {
	t.Parallel()

	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			calls := 0
			client := NewHTTPClientWithOptions(HTTPClientOptions{
				allowUnsafeTargets: true,
				Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					if calls > 1 {
						t.Fatalf("plaintext redirect reached transport with auth=%q", req.Header.Get("Authorization"))
					}
					header := make(http.Header)
					header.Set("Location", "http://provider.example/chat/completions")
					return &http.Response{
						StatusCode: status,
						Header:     header,
						Body:       io.NopCloser(strings.NewReader("")),
						Request:    req,
					}, nil
				})},
			})
			req, err := http.NewRequest(http.MethodPost, "https://provider.example/chat/completions", bytes.NewBufferString("sensitive-body"))
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			req.Header.Set("Authorization", "Bearer fictional-key")
			_, err = client.Do(req)
			if err == nil || !IsUnsafeTargetError(err) {
				t.Fatalf("Do() error = %v, want non-retryable security rejection", err)
			}
			if calls != 1 {
				t.Fatalf("transport calls=%d, want only TLS request", calls)
			}
		})
	}
}

func TestHTTPClientCrossOriginRedirectStripsSensitiveHeaders(t *testing.T) {
	t.Parallel()

	calls := 0
	client := NewHTTPClientWithOptions(HTTPClientOptions{
		allowUnsafeTargets: true,
		Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				header := make(http.Header)
				header.Set("Location", "https://other.example/next")
				return &http.Response{StatusCode: http.StatusFound, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			}
			for _, name := range []string{
				"Authorization", "Proxy-Authorization", "Proxy-Authenticate",
				"Cookie", "Cookie2", "Www-Authenticate",
				"API-Key", "X-API-Key",
			} {
				if value := req.Header.Get(name); value != "" {
					t.Fatalf("redirect retained %s=%q", name, value)
				}
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
		})},
	})
	req, err := http.NewRequest(http.MethodGet, "https://provider.example/start", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer fictional-key")
	req.Header.Set("Proxy-Authorization", "Basic fictional-proxy")
	req.Header.Set("Proxy-Authenticate", "Basic fictional-proxy-challenge")
	req.Header.Set("Cookie", "session=fictional")
	req.Header.Set("Cookie2", "legacy=fictional")
	req.Header.Set("Www-Authenticate", "Bearer fictional-challenge")
	req.Header.Set("API-Key", "fictional-api-key")
	req.Header.Set("X-API-Key", "fictional-x-api-key")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	resp.Body.Close()
	if calls != 2 {
		t.Fatalf("transport calls=%d, want 2", calls)
	}
}

func TestHTTPClientRejectsCredentialedAndNonHTTPRedirectTargets(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		location string
	}{
		{name: "credentials", location: "https://user:query-secret@other.example/private"},
		{name: "scheme", location: "file:///etc/passwd"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			client := NewHTTPClientWithOptions(HTTPClientOptions{
				allowUnsafeTargets: true,
				Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					if calls > 1 {
						t.Fatal("rejected redirect target reached transport")
					}
					header := make(http.Header)
					header.Set("Location", test.location)
					return &http.Response{StatusCode: http.StatusFound, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				})},
			})
			req, err := http.NewRequest(http.MethodGet, "https://provider.example/start", nil)
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			_, err = client.Do(req)
			if err == nil || !errors.Is(err, ErrUnsafeRedirect) || !IsUnsafeTargetError(err) {
				t.Fatalf("Do() error = %v, want typed unsafe redirect", err)
			}
			if strings.Contains(err.Error(), "query-secret") || strings.Contains(err.Error(), "other.example") {
				t.Fatalf("redirect error leaked target details: %v", err)
			}
			if calls != 1 {
				t.Fatalf("transport calls=%d, want initial hop only", calls)
			}
		})
	}
}

func TestHTTPClientAllowsSameOriginHTTPSRedirect(t *testing.T) {
	t.Parallel()

	calls := 0
	client := NewHTTPClientWithOptions(HTTPClientOptions{
		allowUnsafeTargets: true,
		Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				header := make(http.Header)
				header.Set("Location", "https://provider.example/next")
				return &http.Response{StatusCode: http.StatusFound, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			}
			if got := req.Header.Get("Authorization"); got != "Bearer fictional-key" {
				t.Fatalf("same-origin redirect Authorization=%q, want retained bearer", got)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
		})},
	})
	req, err := http.NewRequest(http.MethodGet, "https://provider.example/start", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer fictional-key")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	resp.Body.Close()
	if calls != 2 {
		t.Fatalf("transport calls=%d, want 2", calls)
	}
}

func TestHTTPClientRejectsDowngradeAfterAllowedRedirectHop(t *testing.T) {
	t.Parallel()

	calls := 0
	client := NewHTTPClientWithOptions(HTTPClientOptions{
		allowUnsafeTargets: true,
		Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			header := make(http.Header)
			switch calls {
			case 1:
				header.Set("Location", "https://other.example/next")
			case 2:
				if req.Header.Get("Authorization") != "" {
					t.Fatal("cross-origin intermediate hop retained Authorization")
				}
				header.Set("Location", "http://other.example/plaintext")
			default:
				t.Fatal("plaintext hop reached transport")
			}
			return &http.Response{StatusCode: http.StatusFound, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		})},
	})
	req, err := http.NewRequest(http.MethodGet, "https://provider.example/start", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer fictional-key")
	_, err = client.Do(req)
	if err == nil || !IsUnsafeTargetError(err) {
		t.Fatalf("Do() error = %v, want downgrade rejection", err)
	}
	if calls != 2 {
		t.Fatalf("transport calls=%d, want two HTTPS hops", calls)
	}
}

func TestHTTPClientAllowsExplicitInitialHTTP(t *testing.T) {
	t.Parallel()

	calls := 0
	client := NewHTTPClientWithOptions(HTTPClientOptions{
		allowUnsafeTargets: true,
		Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.URL.Scheme != "http" {
				t.Fatalf("request scheme=%q, want explicit http", req.URL.Scheme)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
		})},
	})
	req, err := http.NewRequest(http.MethodPost, "http://provider.example/start", bytes.NewBufferString("body"))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	resp.Body.Close()
	if calls != 1 {
		t.Fatalf("transport calls=%d, want 1", calls)
	}
}

func TestHTTPClientRejectsCrossOrigin307BodyReplay(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			calls := 0
			client := NewHTTPClientWithOptions(HTTPClientOptions{
				allowUnsafeTargets: true,
				Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					if calls > 1 {
						t.Fatal("cross-origin body replay reached transport")
					}
					header := make(http.Header)
					header.Set("Location", "https://other.example/next")
					return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				})},
			})
			req, err := http.NewRequest(http.MethodPost, "https://provider.example/start", bytes.NewBufferString("sensitive-body"))
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			_, err = client.Do(req)
			if err == nil || !IsUnsafeTargetError(err) {
				t.Fatalf("Do() error = %v, want cross-origin body rejection", err)
			}
			if calls != 1 {
				t.Fatalf("transport calls=%d, want 1", calls)
			}
		})
	}
}

// TestHTTPClientDialerEnforcesPerAttemptTimeoutOnBlackholedIPs guards the
// safedial happy-eyeballs fix: the loop must cap each individual IP dial at
// defaultPerAttemptDialTimeout so a black-holed first address (e.g. an IPv6
// AAAA on a network without IPv6 transit) cannot park the request behind
// the kernel's 30s SYN retransmit budget. Pre-fix, dialer.Timeout (30s)
// swallowed the entire ctx budget on the first dead IP and the IPv4
// fallback was effectively dead code.
//
// A deterministic test dialer blocks until each injected attempt context
// expires. This avoids relying on TEST-NET addresses, which are correctly
// rejected by the strict special-use policy and would make this test pass
// before reaching the dial loop.
func TestHTTPClientDialerEnforcesPerAttemptTimeoutOnBlackholedIPs(t *testing.T) {
	t.Parallel()

	dialCalls := 0
	client := NewHTTPClientWithOptions(HTTPClientOptions{
		LookupIP: lookupIPFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{
				{IP: net.ParseIP("8.8.8.8")},
				{IP: net.ParseIP("1.1.1.1")},
			}, nil
		}),
		dialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialCalls++
			<-ctx.Done()
			return nil, ctx.Err()
		},
		perAttemptDialTimeout: 10 * time.Millisecond,
	})

	req, cancel, err := client.NewRequest(context.Background(), time.Second, http.MethodGet, "https://safe.example/path", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	defer cancel()

	start := time.Now()
	_, err = client.Do(req)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Do() error = nil, want dial failure on black-holed addresses")
	}
	if dialCalls != 2 {
		t.Fatalf("dial calls=%d, want both validated addresses", dialCalls)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Do() elapsed = %v, want two short per-attempt budgets", elapsed)
	}
	t.Logf("blackhole dial elapsed = %v err = %v", elapsed, err)
}

// TestOwnedTransportConnectionPoolDefaults pins the owned-transport
// connection-pool settings so a future refactor cannot silently drop
// back to Go's stdlib defaults (MaxIdleConnsPerHost=2), which would
// throttle parallel fetches against the same host (e.g. the GitHub /
// Jina / arxiv fan-out the parse pipeline relies on). The values
// themselves were tuned in client.go; this test only asserts they
// stay non-default and inside a sane range so the design intent
// survives a transport rewrite.
func TestOwnedTransportConnectionPoolDefaults(t *testing.T) {
	t.Parallel()

	// Owned-transport path: nil opts.Client triggers the branch in
	// NewHTTPClientWithOptions that constructs the *http.Transport
	// with the SSRF-aware DialContext and the connection-pool knobs.
	c := NewHTTPClientWithOptions(HTTPClientOptions{}).Raw()
	tr, ok := unwrapTransport(c.Transport).(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport on owned client, got %T", c.Transport)
	}

	if tr.MaxIdleConnsPerHost <= 2 {
		t.Errorf("MaxIdleConnsPerHost = %d; default is 2 — must be raised so parallel "+
			"fetches against the same host (GitHub/Jina/arxiv) reuse idle conns",
			tr.MaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns <= 0 {
		t.Errorf("MaxIdleConns = %d; want > 0 so the pool keeps connections alive across hosts",
			tr.MaxIdleConns)
	}
	if tr.IdleConnTimeout <= 0 || tr.IdleConnTimeout > 5*time.Minute {
		t.Errorf("IdleConnTimeout = %v; want > 0 and <= 5min to bound leaked idle sockets",
			tr.IdleConnTimeout)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Errorf("ForceAttemptHTTP2 = false; want true so eligible upstreams multiplex over h2")
	}
}

// unwrapTransport peels the SSRF wrapper installed by Raw() so the
// test can reach the underlying *http.Transport whose pool knobs we
// care about. Returns the input unchanged when there is no wrapper.
func unwrapTransport(rt http.RoundTripper) http.RoundTripper {
	if w, ok := rt.(rawHTTPClientTransport); ok {
		if w.client != nil && w.client.client != nil {
			return w.client.client.Transport
		}
	}
	return rt
}
