package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"webtag/internal/observability"
)

func TestRequestIDPreservesIncomingHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestID(nil))
	router.GET("/ping", func(c *gin.Context) {
		c.String(http.StatusOK, c.GetString(RequestIDContextKey))
	})

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set(RequestIDHeader, "req-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if rec.Header().Get(RequestIDHeader) != "req-123" {
		t.Fatalf("X-Request-ID = %q, want %q", rec.Header().Get(RequestIDHeader), "req-123")
	}

	if rec.Body.String() != "req-123" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "req-123")
	}
}

func TestRequestIDGeneratesHeaderWhenMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestID(nil))
	router.GET("/ping", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if rec.Header().Get(RequestIDHeader) == "" {
		t.Fatal("expected generated X-Request-ID header")
	}
}

func TestJSONErrorUsesStandardEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/boom", func(c *gin.Context) {
		JSONError(c, http.StatusNotFound, "link not found")
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	var body struct {
		Error struct {
			Code      int    `json:"code"`
			ErrorCode string `json:"error_code"`
			Message   string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal() returned error: %v", err)
	}

	if body.Error.Code != http.StatusNotFound || body.Error.Message != "link not found" {
		t.Fatalf("unexpected body: %+v", body)
	}
	// Wave 6 M3：旧签名 JSONError(c, status, message) 自动获得 default_<status>
	// 兜底 slug，前端永远能拿到一个非空 error_code（"default_xxx" 一看就
	// 知道是兜底而非业务定义的稳定 slug，便于后续治理）。
	if body.Error.ErrorCode != "default_404" {
		t.Fatalf("error_code = %q, want \"default_404\" fallback slug", body.Error.ErrorCode)
	}
}

// TestJSONErrorWithSlugWritesSlugField 锁定 Wave 6 M3：JSONErrorWithSlug 在
// ErrorDetail 中写出机器可读 slug，且保留原有 code/message 字段不动，确保
// 向后兼容老客户端的同时让新客户端可以按 error_code 分支。
func TestJSONErrorWithSlugWritesSlugField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/cooldown", func(c *gin.Context) {
		JSONErrorWithSlug(c, http.StatusTooManyRequests, ErrCodeCooldownActive, "请稍后重试")
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cooldown", nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}

	var body struct {
		Error struct {
			Code      int    `json:"code"`
			ErrorCode string `json:"error_code"`
			Message   string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal() returned error: %v", err)
	}

	if body.Error.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want %d", body.Error.Code, http.StatusTooManyRequests)
	}
	if body.Error.ErrorCode != ErrCodeCooldownActive {
		t.Fatalf("error_code = %q, want %q", body.Error.ErrorCode, ErrCodeCooldownActive)
	}
	if body.Error.Message != "请稍后重试" {
		t.Fatalf("message = %q, want %q", body.Error.Message, "请稍后重试")
	}
}

// TestJSONErrorWithSlugFallsBackOnEmptySlug 守护一个边界：调用方传入空
// slug 时，envelope 仍然能给出有意义的 error_code（兜底为 default_<status>），
// 而不是写出空字符串导致 omitempty 把 error_code 字段整个吃掉。
func TestJSONErrorWithSlugFallsBackOnEmptySlug(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/boom", func(c *gin.Context) {
		JSONErrorWithSlug(c, http.StatusBadGateway, "", "upstream broken")
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	var body struct {
		Error struct {
			ErrorCode string `json:"error_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal() returned error: %v", err)
	}
	if body.Error.ErrorCode != "default_502" {
		t.Fatalf("error_code = %q, want \"default_502\" fallback", body.Error.ErrorCode)
	}
}

func TestRequestIDInjectsLoggerIntoRequestContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := slog.New(slog.NewTextHandler(&noopWriter{}, nil))
	router := gin.New()
	router.Use(RequestID(logger))
	router.GET("/ping", func(c *gin.Context) {
		contextLogger := observability.FromContext(c.Request.Context())
		if contextLogger == nil {
			t.Fatal("expected request logger in context")
		}
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set(RequestIDHeader, "req-ctx")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestAccessLogWritesStructuredEntry(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuffer, nil))
	router := gin.New()
	router.Use(RequestID(logger), AccessLog(logger))
	router.GET("/ping", func(c *gin.Context) {
		c.Status(http.StatusCreated)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set(RequestIDHeader, "req-log")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}

	lines := bytes.Split(bytes.TrimSpace(logBuffer.Bytes()), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1", len(lines))
	}

	var entry map[string]any
	if err := json.Unmarshal(lines[0], &entry); err != nil {
		t.Fatalf("Unmarshal() returned error: %v", err)
	}

	if entry["msg"] != "http request completed" {
		t.Fatalf("msg = %v, want %q", entry["msg"], "http request completed")
	}
	if entry["request_id"] != "req-log" {
		t.Fatalf("request_id = %v, want %q", entry["request_id"], "req-log")
	}
	if entry["route"] != "/ping" {
		t.Fatalf("route = %v, want %q", entry["route"], "/ping")
	}
	if status, ok := entry["status"].(float64); !ok || int(status) != http.StatusCreated {
		t.Fatalf("status = %v, want %d", entry["status"], http.StatusCreated)
	}
	if _, ok := entry["latency_ms"].(float64); !ok {
		t.Fatalf("latency_ms = %v, want numeric field", entry["latency_ms"])
	}
}

func TestAccessLogAllowsNilLogger(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(AccessLog(nil))
	router.GET("/ping", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestHTTPMetricsRecordsRequestCountAndLatency(t *testing.T) {
	gin.SetMode(gin.TestMode)

	metrics := observability.NewMetrics()
	router := gin.New()
	router.Use(HTTPMetrics(metrics))
	router.GET("/ping", func(c *gin.Context) {
		c.Status(http.StatusAccepted)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}

	metricsRec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(metricsRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := metricsRec.Body.String()
	if !strings.Contains(body, `webtag_http_requests_total{method="GET",route="/ping",status="202"} 1`) {
		t.Fatalf("metrics body = %q, want request counter exposition", body)
	}
	// Wave 2 M3：histogram 现在带 status_class 标签（2xx/3xx/4xx/5xx）。
	// 这条断言锁定 label set，避免后续无意中把 status_class 改成完整
	// status 码（会把单 (method, route) 的时间序列数 ×N）。
	if !strings.Contains(body, `webtag_http_request_duration_seconds_bucket{method="GET",route="/ping",status_class="2xx"`) {
		t.Fatalf("metrics body = %q, want request duration histogram exposition with status_class label", body)
	}
	// 新 bucket 上限是 25s（取代 DefBuckets 的 10s），fetcher 的尾部
	// 慢请求才能落到具体桶而不是 +Inf。断言这条边界值出现，等同于
	// 锁定 buckets 数组。
	if !strings.Contains(body, `le="25"`) {
		t.Fatalf("metrics body = %q, want le=\"25\" bucket boundary present", body)
	}
}

func TestCORSPreflightFromUnauthorizedOriginIsRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(CORS([]string{"https://allowed.example"}))
	router.OPTIONS("/api/links", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodOptions, "/api/links", nil)
	req.Header.Set("Origin", "https://attacker.example")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d for unauthorized preflight", rec.Code, http.StatusForbidden)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty for unauthorized origin", got)
	}
}

func TestCORSPreflightFromAuthorizedOriginReturnsHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(CORS([]string{"https://allowed.example"}))
	router.OPTIONS("/api/links", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodOptions, "/api/links", nil)
	req.Header.Set("Origin", "https://allowed.example")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://allowed.example" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want allowed origin echoed back", got)
	}
}

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

// TestStatusClass 锁定 HTTP 状态码归桶逻辑——Prometheus
// `webtag_http_request_duration_seconds_count{status_class="..."}` 指标
// 用这个桶名做聚合，回归会让 Grafana 的 5xx 错误率告警基于错的 label
// 漏报或误报。0/600+ 必须归 "0xx" 让标签集闭合，避免高基数指标爆炸。
func TestStatusClass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{"100_continue_is_1xx", 100, "1xx"},
		{"200_ok_is_2xx", 200, "2xx"},
		{"299_boundary_is_2xx", 299, "2xx"},
		{"301_redirect_is_3xx", 301, "3xx"},
		{"400_bad_request_is_4xx", 400, "4xx"},
		{"404_not_found_is_4xx", 404, "4xx"},
		{"500_internal_is_5xx", 500, "5xx"},
		{"599_boundary_is_5xx", 599, "5xx"},
		// 兜底：闭合标签集，避免高基数爆炸。
		{"zero_is_0xx", 0, "0xx"},
		{"negative_is_0xx", -1, "0xx"},
		{"below_100_is_0xx", 99, "0xx"},
		{"at_600_is_0xx", 600, "0xx"},
		{"way_above_is_0xx", 999, "0xx"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := statusClass(tc.status); got != tc.want {
				t.Errorf("statusClass(%d) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}
