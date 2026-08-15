package fetcher

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"webtag/internal/errsafe"
	"webtag/internal/observability"
)

func minimalTextPDF(text string) []byte {
	return minimalTextPDFWithPageCount(text, 1)
}

func minimalTextPDFWithPageCount(text string, pageCount int) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, 6)
	writeObject := func(number int, body string) {
		offsets[number] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", number, body)
	}
	writeObject(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObject(2, fmt.Sprintf("<< /Type /Pages /Kids [3 0 R] /Count %d >>", pageCount))
	writeObject(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>")
	writeObject(4, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	stream := "BT /F1 12 Tf 72 720 Td (" + text + ") Tj ET"
	writeObject(5, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	xref := out.Len()
	out.WriteString("xref\n0 6\n0000000000 65535 f \n")
	for object := 1; object <= 5; object++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[object])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return out.Bytes()
}

func compressedTextPDF(text string) []byte {
	var content bytes.Buffer
	zw := zlib.NewWriter(&content)
	_, _ = fmt.Fprintf(zw, "BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
	_ = zw.Close()

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, 6)
	writeObject := func(number int, body []byte) {
		offsets[number] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", number)
		out.Write(body)
		out.WriteString("\nendobj\n")
	}
	writeObject(1, []byte("<< /Type /Catalog /Pages 2 0 R >>"))
	writeObject(2, []byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"))
	writeObject(3, []byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"))
	writeObject(4, []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"))
	stream := fmt.Sprintf("<< /Length %d /Filter /FlateDecode >>\nstream\n", content.Len())
	streamBody := append([]byte(stream), content.Bytes()...)
	streamBody = append(streamBody, []byte("\nendstream")...)
	writeObject(5, streamBody)
	xref := out.Len()
	out.WriteString("xref\n0 6\n0000000000 65535 f \n")
	for object := 1; object <= 5; object++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[object])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return out.Bytes()
}

func TestPDFFetcherRejectsUnexpectedContentType(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>not a pdf</body></html>"))
	}))
	defer server.Close()

	fetcher := NewPDFFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))

	_, err := fetcher.Fetch(context.Background(), server.URL+"/report.pdf")
	if err == nil {
		t.Fatal("Fetch() error = nil, want content-type rejection")
	}
	if !strings.Contains(err.Error(), "content-type") {
		t.Fatalf("Fetch() error = %v, want content-type error", err)
	}
}

func TestPDFFetcherRejectsOversizedResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte(strings.Repeat("A", 64)))
	}))
	defer server.Close()

	fetcher := NewPDFFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	fetcher.MaxBytes = 16

	_, err := fetcher.Fetch(context.Background(), server.URL+"/report.pdf")
	if err == nil {
		t.Fatal("Fetch() error = nil, want oversized PDF rejection")
	}
	if !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("Fetch() error = %v, want size limit error", err)
	}
}

func TestPDFFetcherRejectsHugeDeclaredObjectBudgetWithoutLeakingURL(t *testing.T) {
	t.Parallel()

	payload := []byte("%PDF-1.4\ntrailer << /Size 999999999 >>\nstartxref\n0\n%%EOF\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	fetcher := NewPDFFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	sensitiveURL := server.URL + "/report.pdf?signed=query-secret"
	content, err := fetcher.Fetch(context.Background(), sensitiveURL)
	if err == nil {
		t.Fatal("Fetch() error = nil, want object-budget rejection")
	}
	if content.Body != "" {
		t.Fatalf("Fetch() body = %q, want empty so no partial body can be persisted", content.Body)
	}
	if !strings.Contains(err.Error(), "resource budget") {
		t.Fatalf("Fetch() error = %v, want stable resource-budget error", err)
	}
	if strings.Contains(err.Error(), "query-secret") {
		t.Fatalf("Fetch() error leaked URL query: %v", err)
	}
	outcome, limit := pdfParseMetricLabels(err)
	if outcome != pdfParseOutcomeLimit || limit != pdfParseLimitObjects {
		t.Fatalf("Fetch() labels = (%q, %q), want (%q, %q)", outcome, limit, pdfParseOutcomeLimit, pdfParseLimitObjects)
	}
}

func TestPDFFetcherRecordsBoundedObjectLimitMetric(t *testing.T) {
	t.Parallel()

	payload := []byte("%PDF-1.4\ntrailer << /Size 999999999 >>\nstartxref\n0\n%%EOF\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	metrics := observability.NewMetrics()
	fetcher := newPDFFetcherWithMetrics(
		NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}),
		metrics,
	)

	_, err := fetcher.Fetch(context.Background(), server.URL+"/private.pdf?signed=metric-secret")
	if err == nil {
		t.Fatal("Fetch() error = nil, want object budget rejection")
	}
	if got := testutil.ToFloat64(metrics.PDFParseOutcomesTotal.WithLabelValues("limit", "objects")); got != 1 {
		t.Fatalf("PDF parse limit/objects metric = %v, want 1", got)
	}
}

func TestPDFFetcherRecordsStableOutcomeMetricMatrix(t *testing.T) {
	t.Parallel()

	metrics := observability.NewMetrics()
	fetcher := newPDFFetcherWithMetrics(nil, metrics)
	cases := []struct {
		outcome string
		limit   string
		err     error
	}{
		{outcome: "success", limit: "none", err: nil},
		{outcome: "limit", limit: "output", err: newPDFLimitError(pdfParseLimitOutput)},
		{outcome: "crash", limit: "none", err: &pdfParseError{outcome: pdfParseOutcomeCrash, limit: pdfParseLimitNone, cause: errPDFResourceBudget}},
		{outcome: "timeout", limit: "wall_time", err: &pdfParseError{outcome: pdfParseOutcomeTimeout, limit: pdfParseLimitWallTime, cause: context.DeadlineExceeded}},
	}
	for _, tc := range cases {
		fetcher.recordParseOutcome(tc.err)
		if got := testutil.ToFloat64(metrics.PDFParseOutcomesTotal.WithLabelValues(tc.outcome, tc.limit)); got != 1 {
			t.Fatalf("PDF parse metric (%q, %q) = %v, want 1", tc.outcome, tc.limit, got)
		}
	}
}

func TestParsePDFIsolatedParsesValidBoundaryDocument(t *testing.T) {
	t.Parallel()

	body, err := parsePDFIsolated(context.Background(), minimalTextPDF("Hello"), pdfParseBudget{
		maxInputBytes: 1 << 20, maxChars: 100, maxPages: 1, maxObjects: 6,
		wallTime: 3 * time.Second, maxMemoryBytes: defaultPDFMaxMemoryBytes,
	})
	if err != nil {
		t.Fatalf("parsePDFIsolated() error = %v", err)
	}
	if !strings.Contains(body, "Hello") {
		t.Fatalf("parsePDFIsolated() body=%q, want extracted text", body)
	}
}

func TestParsePDFIsolatedRejectsDeclaredPageCountAboveBudget(t *testing.T) {
	t.Parallel()

	// The xref remains internally consistent while the page tree declares a
	// hostile count. The fixture stays below 1 KiB and needs no giant file.
	payload := minimalTextPDFWithPageCount("Page budget", 1_000_000)
	_, err := parsePDFIsolated(context.Background(), payload, pdfParseBudget{
		maxInputBytes: 1 << 20, maxChars: 100, maxPages: 1, maxObjects: 6,
		wallTime: 3 * time.Second, maxMemoryBytes: defaultPDFMaxMemoryBytes,
	})
	if !errors.Is(err, errPDFResourceBudget) {
		t.Fatalf("parsePDFIsolated() error = %v, want resource-budget rejection", err)
	}
	outcome, limit := pdfParseMetricLabels(err)
	if outcome != pdfParseOutcomeLimit || limit != pdfParseLimitPages {
		t.Fatalf("parsePDFIsolated() labels = (%q, %q), want (%q, %q)", outcome, limit, pdfParseOutcomeLimit, pdfParseLimitPages)
	}
}

func TestParsePDFIsolatedRejectsMalformedStructureWithinFixedBudget(t *testing.T) {
	t.Parallel()

	const sentinel = "malformed-structure-must-not-leak"
	payload := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Secret (" + sentinel + ") >>\nendobj\n" +
		"xref\n0 999\ntruncated")
	started := time.Now()
	body, err := parsePDFIsolated(context.Background(), payload, pdfParseBudget{
		maxInputBytes: 1 << 20, maxChars: 100, maxPages: 10, maxObjects: 1000,
		wallTime: 2 * time.Second, maxMemoryBytes: defaultPDFMaxMemoryBytes,
		maxCPUSeconds: 1,
	})
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("parsePDFIsolated() elapsed = %s, want < 2s", elapsed)
	}
	if body != "" {
		t.Fatalf("parsePDFIsolated() body = %q, want empty", body)
	}
	if err == nil || !errors.Is(err, errsafe.ErrParse) {
		t.Fatalf("parsePDFIsolated() error = %v, want parse failure", err)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("parsePDFIsolated() error leaked malformed input: %v", err)
	}
	outcome, limit := pdfParseMetricLabels(err)
	if outcome != pdfParseOutcomeParseError || limit != pdfParseLimitNone {
		t.Fatalf("parsePDFIsolated() labels = (%q, %q), want (%q, %q)", outcome, limit, pdfParseOutcomeParseError, pdfParseLimitNone)
	}
}

func TestParsePDFIsolatedRejectsCompressedOutputBombWithoutPartialText(t *testing.T) {
	t.Parallel()

	payload := compressedTextPDF(strings.Repeat("A", 8<<10))
	if len(payload) >= 1<<10 {
		t.Fatalf("compressed fixture size = %d bytes, want < 1 KiB", len(payload))
	}
	started := time.Now()
	body, err := parsePDFIsolated(context.Background(), payload, pdfParseBudget{
		maxInputBytes: 1 << 20, maxChars: 128, maxPages: 1, maxObjects: 6,
		wallTime: 2 * time.Second, maxMemoryBytes: defaultPDFMaxMemoryBytes,
	})
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("parsePDFIsolated() elapsed = %s, want < 2s", elapsed)
	}
	if body != "" {
		t.Fatalf("parsePDFIsolated() returned partial body of %d bytes", len(body))
	}
	if !errors.Is(err, errPDFResourceBudget) {
		t.Fatalf("parsePDFIsolated() error = %v, want resource-budget rejection", err)
	}
	outcome, limit := pdfParseMetricLabels(err)
	if outcome != pdfParseOutcomeLimit || limit != pdfParseLimitOutput {
		t.Fatalf("parsePDFIsolated() labels = (%q, %q), want (%q, %q)", outcome, limit, pdfParseOutcomeLimit, pdfParseLimitOutput)
	}
}

func TestParsePDFIsolatedHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := parsePDFIsolated(ctx, minimalTextPDF("Cancelled"), pdfParseBudget{
		maxInputBytes: 1 << 20, maxChars: 100, maxPages: 1, maxObjects: 6,
		wallTime: time.Second, maxMemoryBytes: defaultPDFMaxMemoryBytes,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("parsePDFIsolated() error = %v, want context.Canceled", err)
	}
}

func TestParsePDFIsolatedTerminatesBlockedHelperAtWallTime(t *testing.T) {
	started := time.Now()
	body, err := parsePDFIsolated(context.Background(), minimalTextPDF("never parsed"), pdfParseBudget{
		maxInputBytes: 1 << 20, maxChars: 100, maxPages: 1, maxObjects: 6,
		wallTime: 150 * time.Millisecond, maxMemoryBytes: defaultPDFMaxMemoryBytes,
		maxCPUSeconds: 5, testMode: "block",
	})
	elapsed := time.Since(started)
	if elapsed >= 2*time.Second {
		t.Fatalf("parsePDFIsolated() elapsed = %s, want strict < 2s wall bound", elapsed)
	}
	if body != "" {
		t.Fatalf("parsePDFIsolated() body = %q, want no partial text", body)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parsePDFIsolated() error = %v, want context deadline", err)
	}
	outcome, limit := pdfParseMetricLabels(err)
	if outcome != pdfParseOutcomeTimeout || limit != pdfParseLimitWallTime {
		t.Fatalf("parsePDFIsolated() labels = (%q, %q), want (%q, %q)", outcome, limit, pdfParseOutcomeTimeout, pdfParseLimitWallTime)
	}
}

func TestParsePDFIsolatedClassifiesHelperCrashWithoutLeakingInput(t *testing.T) {
	const sentinel = "hostile-body-must-not-leak"
	started := time.Now()
	body, err := parsePDFIsolated(context.Background(), minimalTextPDF(sentinel), pdfParseBudget{
		maxInputBytes: 1 << 20, maxChars: 100, maxPages: 1, maxObjects: 6,
		wallTime: 2 * time.Second, maxMemoryBytes: defaultPDFMaxMemoryBytes,
		maxCPUSeconds: 1, testMode: "crash",
	})
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("parsePDFIsolated() elapsed = %s, want < 2s", elapsed)
	}
	if body != "" {
		t.Fatalf("parsePDFIsolated() body = %q, want empty", body)
	}
	if !errors.Is(err, errPDFResourceBudget) {
		t.Fatalf("parsePDFIsolated() error = %v, want stable resource failure", err)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("parsePDFIsolated() error leaked PDF text: %v", err)
	}
	outcome, limit := pdfParseMetricLabels(err)
	if outcome != pdfParseOutcomeCrash || limit != pdfParseLimitNone {
		t.Fatalf("parsePDFIsolated() labels = (%q, %q), want (%q, %q)", outcome, limit, pdfParseOutcomeCrash, pdfParseLimitNone)
	}
}

func TestParsePDFIsolatedEnforcesHelperCPULimit(t *testing.T) {
	started := time.Now()
	body, err := parsePDFIsolated(context.Background(), minimalTextPDF("CPU budget"), pdfParseBudget{
		maxInputBytes: 1 << 20, maxChars: 100, maxPages: 1, maxObjects: 6,
		wallTime: 5 * time.Second, maxMemoryBytes: defaultPDFMaxMemoryBytes,
		maxCPUSeconds: 2, testMode: "cpu",
	})
	if elapsed := time.Since(started); elapsed >= 4*time.Second {
		t.Fatalf("parsePDFIsolated() elapsed = %s, want CPU limit before 4s", elapsed)
	}
	if body != "" {
		t.Fatalf("parsePDFIsolated() body = %q, want empty", body)
	}
	if !errors.Is(err, errPDFResourceBudget) {
		t.Fatalf("parsePDFIsolated() error = %v, want resource-budget rejection", err)
	}
	outcome, limit := pdfParseMetricLabels(err)
	if outcome != pdfParseOutcomeLimit || limit != pdfParseLimitCPU {
		t.Fatalf("parsePDFIsolated() labels = (%q, %q), want (%q, %q)", outcome, limit, pdfParseOutcomeLimit, pdfParseLimitCPU)
	}
}

func TestParsePDFIsolatedContainsMemoryExhaustionWithinRSSLimit(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("race runtime reserves a large shadow address space; release helper enforces RLIMIT_AS")
	}

	const memoryBudget = int64(512 << 20)
	started := time.Now()
	body, err := parsePDFIsolated(context.Background(), minimalTextPDF("memory budget"), pdfParseBudget{
		maxInputBytes: 1 << 20, maxChars: 100, maxPages: 1, maxObjects: 6,
		wallTime: 4 * time.Second, maxMemoryBytes: memoryBudget,
		maxCPUSeconds: 3, testMode: "memory",
	})
	if elapsed := time.Since(started); elapsed >= 4*time.Second {
		t.Fatalf("parsePDFIsolated() elapsed = %s, want memory confinement before wall timeout", elapsed)
	}
	if body != "" {
		t.Fatalf("parsePDFIsolated() body = %q, want empty", body)
	}
	var parseErr *pdfParseError
	if !errors.As(err, &parseErr) || parseErr.outcome != pdfParseOutcomeCrash {
		t.Fatalf("parsePDFIsolated() error = %v, want contained helper crash", err)
	}
	if parseErr.maxRSSBytes <= 0 || parseErr.maxRSSBytes > memoryBudget {
		t.Fatalf("helper MaxRSS = %d bytes, want within (0, %d]", parseErr.maxRSSBytes, memoryBudget)
	}
}

func TestParsePDFIsolatedCancellationCleansUpHelperProcessTree(t *testing.T) {
	pids := t.TempDir() + "/child.pid"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	type result struct {
		body string
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		body, err := parsePDFIsolated(ctx, minimalTextPDF("process tree"), pdfParseBudget{
			maxInputBytes: 1 << 20, maxChars: 100, maxPages: 1, maxObjects: 6,
			wallTime: 5 * time.Second, maxMemoryBytes: defaultPDFMaxMemoryBytes,
			maxCPUSeconds: 4, testMode: "spawn-child", testArtifactPath: pids,
		})
		resultCh <- result{body: body, err: err}
	}()

	childPID := waitForPDFHelperChildPID(t, pids, 2*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	if !processExists(childPID) {
		t.Fatalf("helper child pid %d exited before cancellation precondition", childPID)
	}
	cancel()

	select {
	case got := <-resultCh:
		if got.body != "" {
			t.Fatalf("parsePDFIsolated() body = %q, want empty", got.body)
		}
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("parsePDFIsolated() error = %v, want context.Canceled", got.err)
		}
		outcome, limit := pdfParseMetricLabels(got.err)
		if outcome != pdfParseOutcomeCanceled || limit != pdfParseLimitNone {
			t.Fatalf("parsePDFIsolated() labels = (%q, %q), want (%q, %q)", outcome, limit, pdfParseOutcomeCanceled, pdfParseLimitNone)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parsePDFIsolated() did not return within 2s after cancellation")
	}

	deadline := time.Now().Add(2 * time.Second)
	for processExists(childPID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(childPID) {
		t.Fatalf("helper child pid %d survived parent cancellation", childPID)
	}
}

func TestParsePDFIsolatedRemovesHelperScratchFilesAfterTimeout(t *testing.T) {
	tempRoot := t.TempDir()
	_, err := parsePDFIsolated(context.Background(), minimalTextPDF("temporary files"), pdfParseBudget{
		maxInputBytes: 1 << 20, maxChars: 100, maxPages: 1, maxObjects: 6,
		wallTime: 150 * time.Millisecond, maxMemoryBytes: defaultPDFMaxMemoryBytes,
		maxCPUSeconds: 4, testMode: "temp-block", testTempRoot: tempRoot,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parsePDFIsolated() error = %v, want deadline", err)
	}
	entries, readErr := os.ReadDir(tempRoot)
	if readErr != nil {
		t.Fatalf("ReadDir(helper temp root): %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("helper temp root retained %d entries after timeout: %v", len(entries), entries)
	}
}

func TestBlockedPDFHelperDoesNotBlockAnotherParseWorker(t *testing.T) {
	readyPath := t.TempDir() + "/ready"
	blockedCtx, cancelBlocked := context.WithCancel(context.Background())
	t.Cleanup(cancelBlocked)
	type result struct{ err error }
	blocked := make(chan result, 1)
	go func() {
		_, err := parsePDFIsolated(blockedCtx, minimalTextPDF("blocked"), pdfParseBudget{
			maxInputBytes: 1 << 20, maxChars: 100, maxPages: 1, maxObjects: 6,
			wallTime: 5 * time.Second, maxMemoryBytes: defaultPDFMaxMemoryBytes,
			maxCPUSeconds: 4, testMode: "block-ready", testArtifactPath: readyPath,
		})
		blocked <- result{err: err}
	}()
	waitForPDFHelperArtifact(t, readyPath, 2*time.Second)

	started := time.Now()
	body, err := parsePDFIsolated(context.Background(), minimalTextPDF("independent worker"), pdfParseBudget{
		maxInputBytes: 1 << 20, maxChars: 100, maxPages: 1, maxObjects: 6,
		wallTime: 2 * time.Second, maxMemoryBytes: defaultPDFMaxMemoryBytes,
		maxCPUSeconds: 1,
	})
	if err != nil {
		t.Fatalf("independent parse error = %v", err)
	}
	if !strings.Contains(body, "independent worker") {
		t.Fatalf("independent parse body = %q, want extracted text", body)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("independent parse elapsed = %s, want < 2s while hostile helper is blocked", elapsed)
	}
	cancelBlocked()

	select {
	case got := <-blocked:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("blocked parse error = %v, want cancellation", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked parse did not terminate within strict deadline")
	}
}

func waitForPDFHelperChildPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	data := waitForPDFHelperArtifact(t, path, timeout)
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if parseErr != nil || pid <= 0 {
		t.Fatalf("invalid helper child PID %q: %v", data, parseErr)
	}
	return pid
}

func waitForPDFHelperArtifact(t *testing.T, path string, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			return data
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read helper child PID: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper did not publish artifact within %s", timeout)
	return nil
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func TestPDFFetcherRetriesTransientHTTPFailures(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4\nnot-a-real-pdf"))
	}))
	defer server.Close()

	fetcher := NewPDFFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))

	_, err := fetcher.Fetch(context.Background(), server.URL+"/report.pdf")
	if err == nil {
		t.Fatal("Fetch() error = nil, want PDF decode error after retry")
	}
	if calls.Load() != 2 {
		t.Fatalf("HTTP calls = %d, want 2", calls.Load())
	}
	if !strings.Contains(err.Error(), "open PDF failed") {
		t.Fatalf("Fetch() error = %v, want PDF decode failure after retry", err)
	}
}

func TestPutPDFBufResetsBeforePutting(t *testing.T) {
	t.Parallel()

	pool := &sync.Pool{New: func() any { return new(bytes.Buffer) }}

	buf := pool.Get().(*bytes.Buffer)
	buf.WriteString("payload")
	putPDFBuf(pool, buf)

	// sync.Pool offers no "same instance back" guarantee — entries can be
	// discarded by GC between Put and Get. Verify the load-bearing
	// invariant directly: the buffer we just handed back was Reset
	// before being released, so any future Get would observe Len()==0.
	if buf.Len() != 0 {
		t.Fatalf("putPDFBuf did not Reset buffer before Put; len=%d", buf.Len())
	}
}

func TestPutPDFBufDropsOversizedBuffers(t *testing.T) {
	t.Parallel()

	pool := &sync.Pool{New: func() any { return new(bytes.Buffer) }}

	huge := bytes.NewBuffer(make([]byte, 0, pdfPoolMaxCap+1))
	huge.Write(make([]byte, 16))
	putPDFBuf(pool, huge)

	got := pool.Get().(*bytes.Buffer)
	if got == huge {
		t.Fatal("oversized buffer was put back into the pool; want discard")
	}
	if got.Cap() > pdfPoolMaxCap {
		t.Fatalf("freshly allocated pool buffer has cap=%d, want <= %d", got.Cap(), pdfPoolMaxCap)
	}
}

func TestPutPDFBufNilSafe(t *testing.T) {
	t.Parallel()

	pool := &sync.Pool{New: func() any { return new(bytes.Buffer) }}
	putPDFBuf(pool, nil) // must not panic
}
