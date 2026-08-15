package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"webtag/internal/buildinfo"
	"webtag/internal/handler"
	"webtag/internal/middleware"
	"webtag/internal/observability"
)

func TestNewRouterAddsRequestIDHeader(t *testing.T) {
	t.Cleanup(func() {
		gin.SetMode(gin.TestMode)
	})
	gin.SetMode(gin.DebugMode)
	router := NewRouter()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("expected X-Request-ID header on response")
	}
	if gin.Mode() != gin.ReleaseMode {
		t.Fatalf("gin mode = %q, want %q", gin.Mode(), gin.ReleaseMode)
	}
}

func TestRouterIgnoresSpoofedForwardedHeadersWithoutTrustedProxy(t *testing.T) {
	handler, controller := middleware.RateLimit(middleware.RateLimitOptions{RPS: 1, Burst: 1, SweepDisabled: true})
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("start limiter: %v", err)
	}
	t.Cleanup(func() { _ = controller.Stop(context.Background()) })
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{AppEnv: "dev"}, handler)

	for index, spoofed := range []string{"198.51.100.1", "198.51.100.2"} {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.RemoteAddr = "203.0.113.10:4321"
		req.Header.Set("X-Forwarded-For", spoofed)
		req.Header.Set("X-Real-IP", spoofed)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		want := http.StatusOK
		if index == 1 {
			want = http.StatusTooManyRequests
		}
		if rec.Code != want {
			t.Fatalf("request %d status = %d, want %d", index+1, rec.Code, want)
		}
	}
}

func TestRouterUsesForwardedClientFromExplicitTrustedProxy(t *testing.T) {
	var clientIP string
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv: "dev", TrustedProxyCIDRs: []string{"192.0.2.0/24"},
	}, func(c *gin.Context) {
		clientIP = c.ClientIP()
		c.Next()
	})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "192.0.2.10:4321"
	req.Header.Set("X-Forwarded-For", "203.0.113.45")
	router.ServeHTTP(httptest.NewRecorder(), req)
	if clientIP != "203.0.113.45" {
		t.Fatalf("ClientIP() = %q, want forwarded client from trusted ingress", clientIP)
	}
}

func TestRouterUsesRightToLeftTrustedProxyChainAcrossIPFamilies(t *testing.T) {
	var clientIP string
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv: "dev", TrustedProxyCIDRs: []string{"192.0.2.0/24", "2001:db8::/32"},
	}, func(c *gin.Context) {
		clientIP = c.ClientIP()
		c.Next()
	})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "[2001:db8::10]:4321"
	req.Header.Set("X-Forwarded-For", "198.51.100.45, 192.0.2.20")
	router.ServeHTTP(httptest.NewRecorder(), req)
	if clientIP != "198.51.100.45" {
		t.Fatalf("ClientIP() = %q, want first untrusted client before the trusted proxy chain", clientIP)
	}
}

func TestHealthRouteResponseShape(t *testing.T) {
	router := NewRouter()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	var body struct {
		Status    string `json:"status"`
		Version   string `json:"version"`
		Commit    string `json:"commit"`
		BuildTime string `json:"build_time"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal() returned error: %v", err)
	}

	if body.Status != "ok" || body.Version != buildinfo.VersionValue() || body.Commit != buildinfo.CommitValue() || body.BuildTime != buildinfo.BuildTimeValue() {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestReadyRouteReturnsReadyByDefault(t *testing.T) {
	router := NewRouter()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		Status string `json:"status"`
		Ready  bool   `json:"ready"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal() returned error: %v", err)
	}
	if body.Status != "ok" || !body.Ready {
		t.Fatalf("unexpected ready body: %+v", body)
	}
}

func TestReadyRouteReturnsServiceUnavailableWhenReadinessFails(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, readinessCheckerFunc(func(context.Context) error {
		return errors.New("database unavailable")
	}), nil, nil, RouterOptions{AppEnv: "dev"})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body struct {
		Status string `json:"status"`
		Ready  bool   `json:"ready"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal() returned error: %v", err)
	}
	if body.Status != "degraded" || body.Ready {
		t.Fatalf("unexpected degraded ready body: %+v", body)
	}
	if body.Error != "" {
		t.Fatalf("expected /ready not to leak raw error, got %q", body.Error)
	}
}

func TestNewRouterRegistersAPIRoutes(t *testing.T) {
	router := NewRouter()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tags", nil))

	if rec.Code == http.StatusNotFound {
		t.Fatal("expected /api/tags to be registered on the main router")
	}
}

func TestNewRouterExposesMetricsWhenHandlerProvided(t *testing.T) {
	metrics := observability.NewMetrics()
	metrics.ParseRunsTotal.WithLabelValues("success", "basic", "article").Inc()
	router := NewRouterWithDependencies(smokeDeps(), metrics.Handler(), nil, nil, metrics, RouterOptions{AppEnv: "dev"})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("expected /metrics to return exposition data")
	}
	if rec.Body.String() == "" || !containsMetric(rec.Body.String(), "webtag_parser_runs_total") {
		t.Fatalf("metrics body = %q, want parser metrics exposition", rec.Body.String())
	}
}

func TestNewRouterExposesHTTPMetricsWhenHandlerProvided(t *testing.T) {
	metrics := observability.NewMetrics()
	router := NewRouterWithDependencies(smokeDeps(), metrics.Handler(), nil, nil, metrics, RouterOptions{AppEnv: "dev"})

	healthRec := httptest.NewRecorder()
	router.ServeHTTP(healthRec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if healthRec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", healthRec.Code, http.StatusOK)
	}

	metricsRec := httptest.NewRecorder()
	router.ServeHTTP(metricsRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if metricsRec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want %d", metricsRec.Code, http.StatusOK)
	}
	if !containsMetric(metricsRec.Body.String(), "webtag_http_requests_total") {
		t.Fatalf("metrics body = %q, want HTTP request counter exposition", metricsRec.Body.String())
	}
	if !containsMetric(metricsRec.Body.String(), "webtag_http_request_duration_seconds") {
		t.Fatalf("metrics body = %q, want HTTP request duration exposition", metricsRec.Body.String())
	}
}

func TestNewRouterWithDependenciesWritesAccessLog(t *testing.T) {
	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuffer, nil))
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, logger, nil, RouterOptions{AppEnv: "dev"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("X-Request-ID", "req-health")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	lines := strings.Split(strings.TrimSpace(logBuffer.String()), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		t.Fatal("expected structured access log output")
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatalf("Unmarshal() returned error: %v", err)
	}

	if entry["msg"] != "http request completed" {
		t.Fatalf("msg = %v, want %q", entry["msg"], "http request completed")
	}
	if entry["request_id"] != "req-health" {
		t.Fatalf("request_id = %v, want %q", entry["request_id"], "req-health")
	}
	if entry["method"] != http.MethodGet {
		t.Fatalf("method = %v, want %q", entry["method"], http.MethodGet)
	}
	if entry["path"] != "/health" {
		t.Fatalf("path = %v, want %q", entry["path"], "/health")
	}
	if entry["route"] != "/health" {
		t.Fatalf("route = %v, want %q", entry["route"], "/health")
	}
	if status, ok := entry["status"].(float64); !ok || int(status) != http.StatusOK {
		t.Fatalf("status = %v, want %d", entry["status"], http.StatusOK)
	}
}

func containsMetric(body, name string) bool {
	return strings.Contains(body, name)
}

func TestServeOpenAPIReturnsValidSpec(t *testing.T) {
	router := NewRouter()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var spec map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("Unmarshal() returned error: %v", err)
	}
	openAPIVersion, _ := spec["openapi"].(string)
	if !strings.HasPrefix(openAPIVersion, "3.1") {
		t.Fatalf("openapi version = %q, want 3.1.x", openAPIVersion)
	}
	paths, _ := spec["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("expected non-empty paths map in spec")
	}
	for _, required := range []string{"/api/links", "/api/ingest", "/api/jobs/{job_id}", "/health", "/openapi.json", "/static/{path}"} {
		if _, ok := paths[required]; !ok {
			t.Fatalf("expected spec to declare %s path", required)
		}
	}
}

func TestServeDocsReturnsScalarHTML(t *testing.T) {
	router := NewRouter()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-url="/openapi.json"`) {
		t.Fatalf("body missing data-url attribute pointing to /openapi.json: %s", body)
	}
	if !strings.Contains(strings.ToLower(body), "scalar") {
		t.Fatalf("body missing scalar reference: %s", body)
	}
	// Pinning + Subresource Integrity is the only thing keeping a poisoned
	// jsdelivr response from injecting same-origin JS into /docs.
	if !strings.Contains(body, "integrity=") {
		t.Fatalf("body missing integrity= attribute on the Scalar script: %s", body)
	}
}

// TestServeRootRedirectsToReaderOnFullRouter 在完整 NewRouter 上确认站点根
// 通向 Reader。reader_test.go 已在裸 engine 上覆盖了重定向本身，这条额外锁住
// 「Reader 路由确实被 registerStaticAndHealthRoutes 挂进了真实路由表」——
// 之前 GET / 返回的是一页手写调试 UI，那是唯一随二进制发布的界面。
func TestServeRootRedirectsToReaderOnFullRouter(t *testing.T) {
	router := NewRouter()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "/reader/" {
		t.Fatalf("Location = %q, want %q", got, "/reader/")
	}
}

// TestServeAdminConceptMergesReturnsHTMLPage covers the GET
// /admin/concept-merges path that serves the review UI bundled with
// P3-D. Smoke test only — the JS/HTML contract is exercised manually
// against the live API in dev.
func TestServeAdminConceptMergesReturnsHTMLPage(t *testing.T) {
	router := NewRouter()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/concept-merges", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Concept Merge Review") {
		t.Fatalf("body missing UI title: %s", body[:min(len(body), 200)])
	}
	if !strings.Contains(body, "/api/admin/concept-merges") {
		t.Fatalf("body does not reference the API endpoint: %s", body[:min(len(body), 200)])
	}
}

// TestWithHealthExtraFieldsMergesIntoHealthBody verifies the middleware
// surface that lets BuildRuntime expose runtime flags (notably
// unsafe_targets_allowed) on /health without changing the router
// constructor signature.
func TestWithHealthExtraFieldsMergesIntoHealthBody(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{AppEnv: "dev"}, WithHealthExtraFields(map[string]any{
		"unsafe_targets_allowed": true,
		"custom_label":           "value",
	}))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal() returned error: %v", err)
	}
	if body["unsafe_targets_allowed"] != true {
		t.Fatalf("unsafe_targets_allowed = %v, want true", body["unsafe_targets_allowed"])
	}
	if body["custom_label"] != "value" {
		t.Fatalf("custom_label = %v, want \"value\"", body["custom_label"])
	}
	// Base fields must still be present so additive overlay is non-destructive.
	if body["status"] != "ok" {
		t.Fatalf("base status field overwritten: %v", body["status"])
	}
}

type readinessCheckerFunc func(context.Context) error

func (fn readinessCheckerFunc) Ready(ctx context.Context) error {
	return fn(ctx)
}

// smokeDeps returns a Dependencies populated with the same zero-value
// stubs that NewRouter() uses. Tests that exercise non-API routes
// (/health, /ready, /metrics, /docs) can use this to satisfy the M4
// boot-time fail-fast check without dragging in handler test fakes.
func smokeDeps() handler.Dependencies {
	return handler.Dependencies{
		LinksWrite: smokeLinkWriteService{},
		LinksRead:  smokeLinkReadService{},
		Ingest:     smokeIngestService{},
		Jobs:       smokeJobService{},
		Tags:       smokeTagService{},
		Tree:       smokeTreeService{},
	}
}

// TestPprofRouteDisabledByDefault 锁定 Wave 12.5 H3 之后 pprof 必须显式
// 开启：即使设置了 METRICS_AUTH_TOKEN，PProfEnabled=false 时 /debug/pprof/*
// 路径完全不挂载，应当返回 404 而不是 401。
func TestPprofRouteDisabledByDefault(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv:           "dev",
		MetricsAuthToken: "secret-token",
		PProfEnabled:     false,
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/cmdline", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (pprof not mounted when PProfEnabled=false)", rec.Code)
	}
}

// TestPprofRouteDisabledWhenTokenMissing 锁定 fail-closed 行为：
// PProfEnabled=true 但 MetricsAuthToken 为空时，pprof 仍然不挂载（防止
// "我以为打开了其实没鉴权"的运维事故）。
func TestPprofRouteDisabledWhenTokenMissing(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv:       "dev",
		PProfEnabled: true,
		// MetricsAuthToken 故意留空
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/cmdline", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (pprof refused without token)", rec.Code)
	}
}

// TestPprofRouteRejectsMissingToken 验证显式开启 pprof 且配置了 token
// 时，路由确实挂载，未带 Bearer 头返回 401。Drive-by 攻击者打到
// /debug/pprof/heap 必须是 401 而不是 goroutine dump。
func TestPprofRouteRejectsMissingToken(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv:           "dev",
		PProfEnabled:     true,
		MetricsAuthToken: "secret-token",
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/cmdline", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (token required)", rec.Code)
	}
}

// TestRouterMaxRequestBodyEnforcedGlobally 锁定 Wave 5 M4：
// MaxRequestBodyBytes 通过 RouterOptions 挂上去后，所有路由（包括
// 没有 handler-级 MaxBytesReader 的）默认收到全局上限。
func TestRouterMaxRequestBodyEnforcedGlobally(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv:              "dev",
		MaxRequestBodyBytes: 16,
	})

	// 注一个临时 POST 路由直接读 body —— 验证中间件确实包了 Body。
	router.POST("/probe", func(c *gin.Context) {
		_, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithStatus(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	body := bytes.Repeat([]byte("A"), 256)
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/probe", bytes.NewReader(body)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (MaxRequestBody should trigger)", rec.Code)
	}
}

// TestRouterGlobalAndHandlerBodyLimitsTakeStricter 锁定全局中间件和
// handler 内层 MaxBytesReader 嵌套时的真实行为：取严格上限。
// 模拟一个"全局 4 MiB + 内层 16 字节"的嵌套，body=256 字节应被内层
// 拒绝；同时模拟"全局 16 字节 + 内层 4 MiB"，body=256 字节也应被
// 全局拒绝——证明无论谁先包，严格的赢。
func TestRouterGlobalAndHandlerBodyLimitsTakeStricter(t *testing.T) {
	cases := []struct {
		name      string
		globalCap int64
		innerCap  int64
	}{
		{"strict_inner_loose_global", 4 << 20, 16},
		{"strict_global_loose_inner", 16, 4 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
				AppEnv:              "dev",
				MaxRequestBodyBytes: tc.globalCap,
			})
			router.POST("/probe", func(c *gin.Context) {
				c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, tc.innerCap)
				_, err := io.ReadAll(c.Request.Body)
				if err != nil {
					c.AbortWithStatus(http.StatusRequestEntityTooLarge)
					return
				}
				c.Status(http.StatusOK)
			})

			rec := httptest.NewRecorder()
			body := bytes.Repeat([]byte("A"), 256)
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/probe", bytes.NewReader(body)))

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413 (stricter cap should trip)", rec.Code)
			}
		})
	}
}

// TestRouterGlobalBodyLimitAllowsIngestSize 锁定 4 MiB 默认放行 ingest
// 路径的真实 body 大小（1.5 MiB）——防回归：曾经默认值 1 MiB 会把
// /api/ingest 这条 4 MiB 上限路径误打成 413。
func TestRouterGlobalBodyLimitAllowsIngestSize(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv:              "dev",
		MaxRequestBodyBytes: 4 << 20, // 默认值
	})
	router.POST("/probe", func(c *gin.Context) {
		// 模拟 ingest handler：自己包 4 MiB MaxBytesReader（与全局相同）
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<20)
		_, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithStatus(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	body := bytes.Repeat([]byte("A"), int(1.5*float64(1<<20))) // 1.5 MiB
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/probe", bytes.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (1.5 MiB body should pass under 4 MiB cap)", rec.Code)
	}
}

// TestRouterRequestDeadlineInjectsCtxDeadline 锁定 Wave 5 M1：
// RequestDeadlineTimeout 配置后，业务 handler 拿到的 ctx 必须带 deadline。
func TestRouterRequestDeadlineInjectsCtxDeadline(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv:                 "dev",
		RequestDeadlineTimeout: 10 * time.Second,
		RequestDeadlinePercent: 0.9,
	})

	var hasDeadline bool
	router.GET("/probe", func(c *gin.Context) {
		_, hasDeadline = c.Request.Context().Deadline()
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe", nil))

	if !hasDeadline {
		t.Fatal("ctx has no deadline; RequestDeadline middleware not installed")
	}
}

// TestPprofRouteAcceptsValidToken closes the loop: with PProfEnabled=true,
// MetricsAuthToken set, and the right Bearer header attached, the handler
// returns 200.
func TestPprofRouteAcceptsValidToken(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv:           "dev",
		PProfEnabled:     true,
		MetricsAuthToken: "secret-token",
	})

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/cmdline", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (valid token)", rec.Code)
	}
}
