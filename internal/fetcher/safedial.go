package fetcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"webtag/internal/errsafe"
	"webtag/internal/security"
)

// defaultPerAttemptDialTimeout caps each individual IP dial inside the
// safedial fallback loop. Without it, a hostname that resolves to an
// IPv6 black-holed address (e.g. r.jina.ai / html.duckduckgo.com under
// some Anycast carriers from networks without working IPv6 transit)
// would park the request behind net.Dialer.Timeout (30s) before the
// loop ever reached the working IPv4 address — long enough for every
// fetcher's own ctx deadline (Jina 20s, DDG 10s) to fire first, so the
// IPv4 fallback was effectively dead code.
//
// 3s is empirically enough to complete a TCP handshake to any reachable
// public host while being short enough that two stale IPs (worst-case
// dual-stack misconfiguration) still leave room under the tightest
// fetcher budget (search: 10s).
const defaultPerAttemptDialTimeout = 3 * time.Second

// hostLookup is the small subset of net.Resolver the SSRF guards
// depend on. Carved out so tests can inject a deterministic resolver
// without touching the global net.DefaultResolver.
type hostLookup interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

// errUnsafeTarget is the package-internal sentinel for SSRF-protection
// failures. It wraps errsafe.ErrUnsafeTarget so callers (and the
// classifier) can use errors.Is at either level — IsUnsafeTargetError
// keeps matching as before, while errsafe.ClassifyError now resolves the
// category through the sentinel chain instead of substring matching.
var errUnsafeTarget = fmt.Errorf("unsafe target host: %w", errsafe.ErrUnsafeTarget)

// makeSafeDialContext returns a DialContext that validates every resolved IP
// before connecting and dials the validated address explicitly. This closes
// the TOCTOU window where validateRequestTarget could see a safe DNS answer
// while the kernel's resolver in net.Dialer.DialContext later sees a different
// (unsafe) one.
func (c *HTTPClient) makeSafeDialContext(dial dialContextFunc) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if c == nil || c.allowUnsafeTargets {
			return dial(ctx, network, address)
		}

		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}

		// IP literal: validate inline, then hand off to the dialer using the
		// already-validated address so no DNS resolution happens at all.
		if ip := net.ParseIP(host); ip != nil {
			if security.IsUnsafeHost(ip.String()) {
				return nil, fmt.Errorf("%w: %s", errUnsafeTarget, ip.String())
			}
			return dial(ctx, network, address)
		}

		addrs, lookupErr := c.lookupIP.LookupIPAddr(ctx, host)
		if lookupErr != nil {
			// Fail-closed on resolution error so transient NXDOMAIN /
			// timeouts cannot bypass IP-level checks. The caller will see
			// a wrapped errUnsafeTarget rather than a benign DNS error.
			// 这里**只**包裹 errUnsafeTarget，DNS 错误用 %v 渲染为字符串：
			// 上游 SSRF 分类靠 errors.Is(err, errUnsafeTarget) 判定，
			// 同时 wrap 解析错误会让分类被 DNS 子错误污染。
			return nil, fmt.Errorf("%w: dns lookup %s: %v", errUnsafeTarget, host, lookupErr) //nolint:errorlint // reason: 故意只 wrap sentinel，保留 SSRF 分类的纯粹性
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("%w: no addresses for %s", errUnsafeTarget, host)
		}

		var lastErr error
		for _, addr := range addrs {
			if security.IsUnsafeHost(addr.IP.String()) {
				return nil, fmt.Errorf("%w: resolved %s -> %s", errUnsafeTarget, host, addr.IP.String())
			}
			ipPort := net.JoinHostPort(addr.IP.String(), port)

			// Per-attempt deadline so a single black-holed IP cannot
			// burn the caller's whole ctx budget. ctx still bounds the
			// loop overall — context.WithTimeout takes the min of its
			// parent deadline and the new one, so a tight outer ctx
			// (e.g. fetcher with 10s timeout) is honored unchanged.
			attemptTimeout := c.perAttemptDialTimeout
			if attemptTimeout <= 0 {
				attemptTimeout = defaultPerAttemptDialTimeout
			}
			attemptCtx, attemptCancel := context.WithTimeout(ctx, attemptTimeout)
			conn, dialErr := dial(attemptCtx, network, ipPort)
			attemptCancel()
			if dialErr == nil {
				return conn, nil
			}
			// If the parent ctx itself expired we are done — the next
			// attempt would just hit the same error and trying it would
			// only mask the real cause from the caller.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("dial %s: no usable address", host)
		}
		return nil, lastErr
	}
}

// validateRequestTarget runs the pre-flight SSRF check on req. The
// dialer-side check (makeSafeDialContext) is what closes the TOCTOU
// window, but this pre-flight stays so requests for an obviously
// unsafe host fail immediately instead of after a connect attempt.
//
// Wave 14 L-3 considered removing the pre-flight LookupIPAddr to avoid
// the duplicate DNS lookup that fires on every cold connection (and
// every redirect). The duplication was kept on purpose: when a caller
// supplies a custom http.Transport, makeSafeDialContext is bypassed
// entirely and this pre-flight becomes the only IP-level gate. Tests
// (TestHTTPClientRejectsHostsResolvingToUnsafeIPs,
// TestHTTPClientFailsClosedOnDNSError) pin that defense-in-depth path.
// The ~1ms per cold connection is the price.
func (c *HTTPClient) validateRequestTarget(req *http.Request) error {
	if req == nil || req.URL == nil {
		return nil
	}
	if req.URL.User != nil {
		return fmt.Errorf("%w: URL credentials are not allowed", errUnsafeTarget)
	}
	switch strings.ToLower(req.URL.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("%w: non-HTTP scheme rejected", errUnsafeTarget)
	}
	if c == nil || c.allowUnsafeTargets {
		return nil
	}

	host := strings.TrimSpace(req.URL.Hostname())
	if host == "" {
		return fmt.Errorf("%w: missing target host", errUnsafeTarget)
	}
	if security.IsUnsafeHost(host) {
		return fmt.Errorf("%w: %s", errUnsafeTarget, host)
	}
	// IP literals were already covered by IsUnsafeHost above; only hostnames
	// need the resolver pre-flight. Fail-closed so transient resolver errors
	// (NXDOMAIN, timeouts, refused) cannot silently bypass the IP check —
	// the actual dial would have hit the same error anyway, and the caller
	// gets a clearer "unsafe" signal than a generic transport failure.
	if c.lookupIP != nil && net.ParseIP(host) == nil {
		addrs, err := c.lookupIP.LookupIPAddr(req.Context(), host)
		if err != nil {
			// 同 makeSafeDialContext：只 wrap errUnsafeTarget，DNS 错误以
			// 文本形式拼接，避免污染 errors.Is 的 SSRF 分类匹配。
			return fmt.Errorf("%w: dns lookup %s: %v", errUnsafeTarget, host, err) //nolint:errorlint // reason: 故意只 wrap sentinel，保留 SSRF 分类的纯粹性
		}
		if len(addrs) == 0 {
			return fmt.Errorf("%w: no addresses for %s", errUnsafeTarget, host)
		}
		for _, addr := range addrs {
			if security.IsUnsafeHost(addr.IP.String()) {
				return fmt.Errorf("%w: resolved %s -> %s", errUnsafeTarget, host, addr.IP.String())
			}
		}
	}
	return nil
}

func isUnsafeTargetError(err error) bool {
	return errors.Is(err, errUnsafeTarget)
}

// IsUnsafeTargetError reports whether err originated from the SSRF
// allow-list — both pre-flight and dialer-time guards wrap the same
// sentinel so service / analyzer code can short-circuit retries when
// they see this category.
func IsUnsafeTargetError(err error) bool {
	return isUnsafeTargetError(err)
}
