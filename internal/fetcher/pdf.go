package fetcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	nurl "net/url"
	"path"
	"strings"
	"sync"
	"time"

	"webtag/internal/errsafe"
	"webtag/internal/observability"
)

const (
	defaultPDFTimeout               = 20 * time.Second
	defaultPDFMaxBytes              = 20 << 20
	defaultPDFMaxChars              = 20000
	defaultPDFMaxPages              = 500
	defaultPDFMaxObjects            = 100000
	defaultPDFParseTimeout          = 5 * time.Second
	defaultPDFMaxMemoryBytes int64  = 512 << 20
	defaultPDFMaxCPUSeconds  uint64 = 4

	// pdfPoolMaxCap caps the buffer capacity that may re-enter the pool. Large
	// PDFs (tens of MB) would otherwise inflate steady-state memory because
	// sync.Pool only releases entries opportunistically.
	pdfPoolMaxCap = 32 << 20
)

// pdfInputBufPool recycles the buffer used to read the raw PDF response body.
// PARSE_CONCURRENCY can drive several 20MB allocations per second; pooling
// roughly caps memory pressure to (active workers * 20MB) instead of growing
// with throughput.
var pdfInputBufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// putPDFBuf returns buf to pool only when its capacity stays under the cap
// to avoid retaining oversized allocations across requests.
func putPDFBuf(pool *sync.Pool, buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	if buf.Cap() > pdfPoolMaxCap {
		return
	}
	buf.Reset()
	pool.Put(buf)
}

// PDFFetcher 抓取 .pdf 链接并提取纯文本正文，受输入、结构、输出、CPU 与内存硬预算保护。
type PDFFetcher struct {
	client         *HTTPClient
	Timeout        time.Duration
	MaxBytes       int64
	MaxChars       int
	MaxPages       int
	MaxObjects     int
	ParseTimeout   time.Duration
	MaxMemoryBytes int64
	MaxCPUSeconds  uint64
	metrics        *observability.Metrics
}

// NewPDFFetcher 构造默认配置（20s 超时 / 20MiB 体积上限 / 20000 字符正文上限）的 PDF 抓取器。
func NewPDFFetcher(client *HTTPClient) *PDFFetcher {
	return newPDFFetcherWithMetrics(client, nil)
}

// newPDFFetcherWithMetrics constructs a PDF fetcher that records only bounded
// isolated-parser outcome and resource-limit labels.
func newPDFFetcherWithMetrics(client *HTTPClient, metrics *observability.Metrics) *PDFFetcher {
	return &PDFFetcher{
		client:         ensureHTTPClient(client),
		Timeout:        defaultPDFTimeout,
		MaxBytes:       defaultPDFMaxBytes,
		MaxChars:       defaultPDFMaxChars,
		MaxPages:       defaultPDFMaxPages,
		MaxObjects:     defaultPDFMaxObjects,
		ParseTimeout:   defaultPDFParseTimeout,
		MaxMemoryBytes: defaultPDFMaxMemoryBytes,
		MaxCPUSeconds:  defaultPDFMaxCPUSeconds,
		metrics:        metrics,
	}
}

// CanHandle 当 URL 去掉 query/fragment 后以 .pdf 结尾时返回 true，仅做路径嗅探不发请求。
func (f *PDFFetcher) CanHandle(url string) bool {
	base := url
	if cut := strings.IndexAny(base, "?#"); cut >= 0 {
		base = base[:cut]
	}
	return strings.HasSuffix(strings.ToLower(base), ".pdf")
}

// Fetch 下载 PDF 二进制流到 pooled buffer，再在隔离 helper 中按 MaxChars 硬预算提取纯文本。
func (f *PDFFetcher) Fetch(ctx context.Context, url string) (Content, error) {
	req, cancel, err := f.client.NewRequest(ctx, f.Timeout, http.MethodGet, url, nil)
	if err != nil {
		return Content{}, &FetchError{URL: url, Reason: "build request failed", Err: err}
	}
	defer cancel()

	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "application/pdf,application/octet-stream")

	resp, err := f.client.DoWithRetry(req)
	if err != nil {
		return Content{}, &FetchError{URL: url, Reason: "request failed", Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Content{}, &FetchError{URL: url, Reason: fmt.Sprintf("HTTP %d", resp.StatusCode), Err: errsafe.ErrUpstreamHTTP}
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "pdf") && !strings.Contains(contentType, "octet-stream") {
		return Content{}, &FetchError{URL: url, Reason: "response content-type is not PDF", Err: errsafe.ErrContentType}
	}

	inputBuf := pdfInputBufPool.Get().(*bytes.Buffer)
	inputBuf.Reset()
	defer putPDFBuf(&pdfInputBufPool, inputBuf)

	if _, err := io.Copy(inputBuf, io.LimitReader(resp.Body, f.MaxBytes+1)); err != nil {
		return Content{}, &FetchError{URL: url, Reason: "read PDF failed", Err: err}
	}
	if int64(inputBuf.Len()) > f.MaxBytes {
		return Content{}, &FetchError{URL: url, Reason: "PDF exceeds size limit", Err: errsafe.ErrResponseTooLarge}
	}

	body, err := parsePDFIsolated(req.Context(), inputBuf.Bytes(), pdfParseBudget{
		maxInputBytes:  f.MaxBytes,
		maxChars:       f.MaxChars,
		maxPages:       f.MaxPages,
		maxObjects:     f.MaxObjects,
		wallTime:       f.ParseTimeout,
		maxMemoryBytes: f.MaxMemoryBytes,
		maxCPUSeconds:  f.MaxCPUSeconds,
	})
	f.recordParseOutcome(err)
	if err != nil {
		reason := "open PDF failed"
		if errors.Is(err, errPDFResourceBudget) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			reason = "PDF parsing rejected by resource budget"
		}
		return Content{}, &FetchError{Reason: reason, Err: err}
	}
	if body == "" {
		return Content{}, &FetchError{Reason: "PDF text is empty"}
	}

	return Content{
		URL:         url,
		Title:       pdfTitleFromURL(url),
		Body:        body,
		Metadata:    nil,
		FetcherType: "pdf",
	}, nil
}

func (f *PDFFetcher) recordParseOutcome(err error) {
	if f == nil || f.metrics == nil || f.metrics.PDFParseOutcomesTotal == nil {
		return
	}
	outcome, limit := pdfParseMetricLabels(err)
	f.metrics.PDFParseOutcomesTotal.WithLabelValues(string(outcome), string(limit)).Inc()
}

func pdfTitleFromURL(rawURL string) string {
	parsedURL, err := nurl.Parse(rawURL)
	if err != nil {
		return ""
	}

	base := path.Base(parsedURL.Path)
	if base == "." || base == "/" {
		return ""
	}

	base = strings.TrimSuffix(base, ".pdf")
	base = strings.TrimSuffix(base, ".PDF")
	return normalizeSpace(strings.NewReplacer("-", " ", "_", " ").Replace(base))
}
