package fetcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"webtag/internal/retry"
)

// HTTPClient 是 fetcher 包对外的通用 HTTP 客户端，封装 SSRF 防护、GET 重试和重定向校验。
type HTTPClient struct {
	client                *http.Client
	closeIdleConnections  func()
	retryAttempts         int
	retryDelay            time.Duration
	allowUnsafeTargets    bool
	lookupIP              hostLookup
	perAttemptDialTimeout time.Duration
}

// HTTPClientOptions 用于配置 HTTPClient：允许注入自定义 *http.Client、解析器、重试策略和 transport 包装器。
type HTTPClientOptions struct {
	Client             *http.Client
	RetryAttempts      int
	RetryDelay         time.Duration
	AllowUnsafeTargets bool
	LookupIP           hostLookup
	// WrapTransport, when non-nil, is called after the base transport is
	// constructed and replaces it with the returned RoundTripper. This is
	// the intended hook for instrumentation layers (e.g. metrics) that
	// must sit outside the SSRF-safe dialer without duplicating its logic.
	WrapTransport func(http.RoundTripper) http.RoundTripper
	// Package-private deterministic seams used by safedial tests. Production
	// callers cannot set them and always use net.Dialer plus the default budget.
	dialContext           dialContextFunc
	perAttemptDialTimeout time.Duration
}

// ErrUnsafeRedirect identifies redirects rejected before the next network
// hop. It preserves the unsafe-target sentinel chain so existing retry and
// observability classifiers continue to fail closed without string matching.
var ErrUnsafeRedirect = fmt.Errorf("unsafe redirect: %w", errUnsafeTarget)

// NewHTTPClient 用给定 *http.Client 构造一个最小配置的 HTTPClient；传 nil 时启用内置 SSRF-safe transport。
func NewHTTPClient(client *http.Client) *HTTPClient {
	return NewHTTPClientWithOptions(HTTPClientOptions{Client: client})
}

// NewHardenedHTTPClient returns a stock *http.Client that funnels every
// request through the SSRF pre-flight + safe-dial transport. It is the
// single convenience entry point for service-layer callers (analyzer,
// embedding) that want plain http.Client semantics without managing
// the *HTTPClient retry surface. Centralising the construction here
// means future SSRF-transport tweaks land in one place.
func NewHardenedHTTPClient() *http.Client {
	return NewHTTPClient(nil).Raw()
}

// NewHTTPClientWithOptions 按 opts 构造 HTTPClient：缺省值落回内置 SSRF-safe transport、默认重试参数和 net.DefaultResolver。
func NewHTTPClientWithOptions(opts HTTPClientOptions) *HTTPClient {
	retryAttempts := opts.RetryAttempts
	if retryAttempts <= 0 {
		retryAttempts = defaultRetryAttempts
	}
	retryDelay := opts.RetryDelay
	if retryDelay <= 0 {
		retryDelay = defaultRetryDelay
	}
	lookupIP := opts.LookupIP
	if lookupIP == nil {
		lookupIP = net.DefaultResolver
	}
	// Singleflight collapses concurrent LookupIPAddr calls for the same
	// host into a single underlying resolver call (Wave 14 L-3). The
	// pre-flight + dial-time double-check stays — singleflight only
	// dedupes simultaneous calls. Sequential calls to the same host
	// from one request still hit the resolver twice (the design has
	// always relied on the second call for TOCTOU defense), but a
	// batch fanout of N parallel requests to the same domain now
	// triggers one DNS round trip instead of N.
	lookupIP = &dedupingHostLookup{inner: lookupIP}

	httpClient := &HTTPClient{
		retryAttempts:         retryAttempts,
		retryDelay:            retryDelay,
		allowUnsafeTargets:    opts.AllowUnsafeTargets,
		lookupIP:              lookupIP,
		perAttemptDialTimeout: opts.perAttemptDialTimeout,
	}

	client := opts.Client
	if client == nil {
		// Owned-Transport path: install a SSRF-aware DialContext that
		// re-resolves and re-validates every IP just before connect, closing
		// the DNS-rebinding TOCTOU window the pre-flight lookup leaves open
		// when the dialer is allowed to resolve again on its own. Callers
		// that supply their own *http.Client (e.g. tests using server.Client())
		// fall back to the pre-flight check only — adequate for those cases
		// because their transport is wired to a known fixture.
		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		baseDialContext := dialContextFunc(dialer.DialContext)
		if opts.dialContext != nil {
			baseDialContext = opts.dialContext
		}
		transport := &http.Transport{
			DialContext:         httpClient.makeSafeDialContext(baseDialContext),
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		}
		httpClient.closeIdleConnections = transport.CloseIdleConnections
		client = &http.Client{
			Transport: transport,
			// Last-resort total budget. Every production caller already
			// installs a per-request context.WithTimeout, so this only
			// fires when a future caller forgets — better to cap a
			// hung connection at 60s than to leak a goroutine forever.
			// Kept generous (60s) so legitimate slow targets (jina /
			// arxiv) under their own ctx timeout are unaffected.
			Timeout: 60 * time.Second,
		}
	}

	// If the caller provided a WrapTransport hook, apply it now so the
	// instrumentation layer sits outside the safe dialer but inside the
	// retry / SSRF-validation logic.  Only applied on the owned-transport
	// path (client == nil above) because a caller-supplied *http.Client
	// already has its transport managed externally.
	if opts.WrapTransport != nil && opts.Client == nil && client.Transport != nil {
		client.Transport = opts.WrapTransport(client.Transport)
	}

	httpClient.client = client
	httpClient.client.CheckRedirect = httpClient.checkRedirect
	return httpClient
}

func (c *HTTPClient) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if len(via) == 0 {
		return c.validateRedirectTarget(req)
	}
	previous := via[len(via)-1]
	if previous == nil || previous.URL == nil || req == nil || req.URL == nil {
		return c.validateRedirectTarget(req)
	}

	// A redirect from authenticated TLS to plaintext can replay both
	// credentials and a 307/308 request body. Reject it before the transport
	// sees the next request, regardless of the unsafe-target opt-in.
	if strings.EqualFold(previous.URL.Scheme, "https") && strings.EqualFold(req.URL.Scheme, "http") {
		return fmt.Errorf("%w: HTTPS to HTTP downgrade rejected", ErrUnsafeRedirect)
	}
	if sameOrigin(previous.URL, req.URL) {
		return c.validateRedirectTarget(req)
	}

	// Go replays the original body for 307/308 when GetBody is set. Crossing
	// an origin with that body is not made safe merely by deleting headers.
	if req.Response != nil &&
		(req.Response.StatusCode == http.StatusTemporaryRedirect || req.Response.StatusCode == http.StatusPermanentRedirect) &&
		req.Body != nil {
		return fmt.Errorf("%w: cross-origin request body replay rejected", ErrUnsafeRedirect)
	}
	if req.Header != nil {
		stripCrossOriginSensitiveHeaders(req.Header)
	}
	return c.validateRedirectTarget(req)
}

func (c *HTTPClient) validateRedirectTarget(req *http.Request) error {
	if err := c.validateRequestTarget(req); err != nil {
		// Do not include the rejected URL or host. Redirect Locations can carry
		// signed queries and credentials, and this error may reach logs.
		return fmt.Errorf("%w: target rejected", ErrUnsafeRedirect)
	}
	return nil
}

var crossOriginSensitiveHeaders = [...]string{
	"Authorization",
	"Proxy-Authorization",
	"Proxy-Authenticate",
	"Cookie",
	"Cookie2",
	"Www-Authenticate",
	"API-Key",
	"X-API-Key",
	"X-Auth-Token",
	"X-Goog-API-Key",
	"OpenAI-Organization",
	"OpenAI-Project",
}

func stripCrossOriginSensitiveHeaders(header http.Header) {
	for _, name := range crossOriginSensitiveHeaders {
		header.Del(name)
	}
}

func sameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil || !strings.EqualFold(left.Scheme, right.Scheme) {
		return false
	}
	return strings.EqualFold(left.Hostname(), right.Hostname()) && effectivePort(left) == effectivePort(right)
}

func effectivePort(target *url.URL) string {
	if target == nil {
		return ""
	}
	if port := target.Port(); port != "" {
		return port
	}
	switch strings.ToLower(target.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// Do 执行单次请求；发出前先做一次 SSRF 预检，避免明显不安全的目标连发出去都没被拦截。
func (c *HTTPClient) Do(req *http.Request) (*http.Response, error) {
	if err := c.validateRequestTarget(req); err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	return resp, SanitizeSecurityError(err)
}

// DoWithRetry 对 GET 类幂等请求执行带退避的重试，遇到 429 / 5xx 或网络错误时退避重发。
func (c *HTTPClient) DoWithRetry(req *http.Request) (*http.Response, error) {
	if err := c.validateRequestTarget(req); err != nil {
		return nil, err
	}

	attempts := c.retryAttempts
	if attempts <= 0 {
		attempts = 1
	}
	policy := retry.NewPolicy(c.retryDelay, fetcherMaxRetryDelay)

	var lastResp *http.Response
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		resp, err := c.client.Do(req)
		if err != nil {
			err = SanitizeSecurityError(err)
			lastErr = err
			if req.Context().Err() != nil || !isRetryableRequest(req) || attempt == attempts-1 || isUnsafeTargetError(err) {
				return nil, err
			}
			if err := policy.Wait(req.Context(), policy.Delay(attempt, "")); err != nil {
				return nil, err
			}
			continue
		}

		if !isRetryableResponse(resp) || attempt == attempts-1 || !isRetryableRequest(req) {
			return resp, nil
		}

		delay := policy.Delay(attempt, resp.Header.Get("Retry-After"))
		resp.Body.Close()
		lastResp = resp
		if err := policy.Wait(req.Context(), delay); err != nil {
			return nil, err
		}
	}

	if lastResp != nil {
		return lastResp, nil
	}
	return nil, lastErr
}

// SanitizeSecurityError removes net/http's URL-bearing wrappers from outbound
// security failures while preserving stable sentinel identity. Callers that
// use Raw() must apply it before formatting or persisting a transport error.
func SanitizeSecurityError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUnsafeRedirect):
		return ErrUnsafeRedirect
	case isUnsafeTargetError(err):
		return errUnsafeTarget
	default:
		return err
	}
}

// Raw exposes a thin *http.Client that still funnels every request
// through the SSRF pre-flight + the underlying transport. Used by the
// analyzer client which wants Do semantics without the GET-only retry
// loop.
func (c *HTTPClient) Raw() *http.Client {
	if c == nil {
		return nil
	}
	return &http.Client{
		Transport:     rawHTTPClientTransport{client: c},
		CheckRedirect: c.client.CheckRedirect,
		Jar:           c.client.Jar,
		Timeout:       c.client.Timeout,
	}
}

// CloseIdleConnections releases idle sockets held by the transport created by
// this HTTPClient. Caller-supplied clients remain owned by their caller.
func (c *HTTPClient) CloseIdleConnections() {
	if c == nil || c.closeIdleConnections == nil {
		return
	}
	c.closeIdleConnections()
}

type rawHTTPClientTransport struct {
	client *HTTPClient
}

func (t rawHTTPClientTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.client == nil || t.client.client == nil || t.client.client.Transport == nil {
		return http.DefaultTransport.RoundTrip(req)
	}
	if err := t.client.validateRequestTarget(req); err != nil {
		return nil, err
	}
	return t.client.client.Transport.RoundTrip(req)
}

func (t rawHTTPClientTransport) CloseIdleConnections() {
	if t.client != nil {
		t.client.CloseIdleConnections()
	}
}

// NewRequest 构造带超时 context 的 *http.Request，返回的 cancel 必须在请求结束后调用以释放定时器。
func (c *HTTPClient) NewRequest(
	ctx context.Context,
	timeout time.Duration,
	method string,
	target string,
	body io.Reader,
) (*http.Request, context.CancelFunc, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(reqCtx, method, target, body)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return req, cancel, nil
}

// dedupingHostLookup wraps a hostLookup with singleflight so concurrent
// callers asking for the same host share one underlying DNS round
// trip. The wrapper does not cache results across the singleflight
// window — once the inner call returns, every subsequent caller does a
// fresh lookup. This keeps the dialer's TOCTOU defense intact (DNS
// rebinding between sequential calls still surfaces a fresh answer)
// while saving the redundant work when many goroutines look up the
// same host at the same instant (batch ingest fan-out, parser pool).
type dedupingHostLookup struct {
	inner hostLookup
	group singleflight.Group
}

// dedupedLookupTimeout bounds one coalesced DNS round trip. The shared lookup
// no longer inherits the leader's cancellation, so it needs its own bound.
const dedupedLookupTimeout = 15 * time.Second

func (d *dedupingHostLookup) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	// singleflight 只跑 leader 那一份闭包，并把它的 error 分发给所有等待者。
	// 调用方对 lookup 失败是刻意 fail-closed 的（包成 errUnsafeTarget，上游按
	// 不可重试的安全错误处理，直接写终态失败 / 永久取消 River job），所以绝不能
	// 让 leader 的 context.Canceled 变成别人的"目标不安全"判定。闭包因此剥离
	// 取消，调用方各自通过 DoChan 响应自己的 context。
	shared := d.group.DoChan(host, func() (any, error) {
		lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dedupedLookupTimeout)
		defer cancel()
		return d.inner.LookupIPAddr(lookupCtx, host)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case outcome := <-shared:
		if outcome.Err != nil {
			return nil, outcome.Err
		}
		addrs, _ := outcome.Val.([]net.IPAddr)
		return addrs, nil
	}
}
