// Package fetcher – Prometheus-instrumented HTTP transport.
//
// metricsRoundTripper wraps any http.RoundTripper and observes
// webtag_fetcher_duration_seconds after each round trip. Two label values
// are derived from the outgoing request:
//
//   - fetcher_type: coarse client role, supplied at construction time so
//     callers can tag traffic by purpose. Current values: "fetch" (generic
//     content fetcher), "openai" (analyzer LLM),
//     "embedding" (concept resolver vector gate). Extend as new outbound
//     clients land.
//   - host_class: eTLD+1 of the request host, capped at 32 characters,
//     used to identify the remote service without unbounded label cardinality.
//
// Usage – wrap the transport before handing the client to the fetcher stack:
//
//	client.Transport = fetcher.NewMetricsTransport(client.Transport, metrics, "fetch")
package fetcher

import (
	"net"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"webtag/internal/observability"
)

// metricsRoundTripper wraps an inner transport with Prometheus timing.
type metricsRoundTripper struct {
	inner          http.RoundTripper
	metrics        *observability.Metrics
	fetcherType    string
	fixedHostClass string
}

// NewMetricsTransport returns an http.RoundTripper that observes
// webtag_fetcher_duration_seconds after every round trip and starts an
// OTel client span around the inner round trip.
//
// Wrap order is otelhttp(metrics(inner))：
//   - 外层 otelhttp 创建 client span，inject TraceContext header；
//   - 内层 metricsRoundTripper 仍负责老的 Prometheus 直方图。
//
// span name 默认是 "HTTP <method>"；为了在 trace UI 里能按 fetcher_type
// 维度区分（fetch / openai / embedding），用
// WithSpanNameFormatter 给 span 名补上 fetcher_type 前缀。当 metrics 为
// nil 时仍然回退到只装 otelhttp（让 noop tracer 路径下也至少不破坏指标
// 链路），但调用方目前传 nil metrics 只出现在测试中，行为兼容。
func NewMetricsTransport(inner http.RoundTripper, m *observability.Metrics, fetcherType string) http.RoundTripper {
	return newMetricsTransport(inner, m, fetcherType, "")
}

// NewFixedHostMetricsTransport records a caller-supplied bounded host class
// instead of deriving it from the request URL. Use it for untrusted locators
// such as ingest images, where even eTLD+1 would be attacker-controlled and
// unbounded across requests.
func NewFixedHostMetricsTransport(
	inner http.RoundTripper,
	m *observability.Metrics,
	fetcherType string,
	fixedHostClass string,
) http.RoundTripper {
	fixedHostClass = strings.TrimSpace(fixedHostClass)
	if fixedHostClass == "" {
		fixedHostClass = "external"
	}
	return newMetricsTransport(inner, m, fetcherType, fixedHostClass)
}

func newMetricsTransport(
	inner http.RoundTripper,
	m *observability.Metrics,
	fetcherType string,
	fixedHostClass string,
) http.RoundTripper {
	if inner == nil {
		inner = http.DefaultTransport
	}
	transport := inner
	// 仅当 metrics 真正可用时挂 Prometheus 计时层；否则跳过，避免无意义
	// 的 wrap 增加 goroutine 局部分配。
	if m != nil && m.FetcherDuration != nil {
		transport = &metricsRoundTripper{
			inner:          transport,
			metrics:        m,
			fetcherType:    fetcherType,
			fixedHostClass: fixedHostClass,
		}
	}
	// otelhttp.NewTransport 永远生效：noop tracer 下创建的 span 是零开销
	// 句柄，不会真正落到 collector；OTLP 启用后自动开始记录。span name
	// formatter 让 trace 端能立刻按 fetcher_type 分组。
	ft := fetcherType
	if ft == "" {
		ft = "unknown"
	}
	return otelhttp.NewTransport(
		transport,
		otelhttp.WithSpanNameFormatter(func(_ string, req *http.Request) string {
			if req == nil {
				return "HTTP " + ft
			}
			return "HTTP " + req.Method + " " + ft
		}),
	)
}

// RoundTrip times the inner trip and records the duration metric.
func (t *metricsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.inner.RoundTrip(req)
	elapsed := time.Since(start).Seconds()

	host := t.fixedHostClass
	if host == "" && req != nil && req.URL != nil {
		host = hostClass(req.URL.Host)
	}
	ft := t.fetcherType
	if ft == "" {
		ft = "unknown"
	}
	t.metrics.FetcherDuration.WithLabelValues(ft, host).Observe(elapsed)
	return resp, err
}

// hostClass derives a short, bounded label from a URL host (host or host:port).
// It strips the port, "www." prefix, and keeps only the last two dot-delimited
// labels (eTLD+1 approximation). IP literals are returned as-is, capped at 32
// characters.
func hostClass(hostPort string) string {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		// No port, use as-is.
		host = hostPort
	}
	// Strip leading "www." to merge www and apex.
	host = strings.TrimPrefix(host, "www.")
	// Keep only the last two dot-delimited labels.
	if parts := strings.Split(host, "."); len(parts) > 2 {
		host = strings.Join(parts[len(parts)-2:], ".")
	}
	// Cap at 32 characters.
	if len(host) > 32 {
		host = host[:32]
	}
	if host == "" {
		return "unknown"
	}
	return host
}
