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
	var clientIP string
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, RouterOptions{}, func(c *gin.Context) {
		clientIP = c.ClientIP()
		c.Next()
	})

	for _, spoofed := range []string{"198.51.100.1", "198.51.100.2"} {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.RemoteAddr = "203.0.113.10:4321"
		req.Header.Set("X-Forwarded-For", spoofed)
		req.Header.Set("X-Real-IP", spoofed)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if clientIP != "203.0.113.10" {
			t.Fatalf("ClientIP() = %q, want direct peer; spoofed X-Forwarded-For was %q", clientIP, spoofed)
		}
	}
}

func TestRouterUsesForwardedClientFromExplicitTrustedProxy(t *testing.T) {
	var clientIP string
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, RouterOptions{
		TrustedProxyCIDRs: []string{"192.0.2.0/24"},
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
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, RouterOptions{
		TrustedProxyCIDRs: []string{"192.0.2.0/24", "2001:db8::/32"},
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
	router := NewRouterWithDependencies(smokeDeps(), readinessCheckerFunc(func(context.Context) error {
		return errors.New("database unavailable")
	}), nil, RouterOptions{})

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

func TestNewRouterWithDependenciesWritesAccessLog(t *testing.T) {
	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuffer, nil))
	router := NewRouterWithDependencies(smokeDeps(), nil, logger, RouterOptions{})

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

func TestRemovedRuntimeRoutesAreNotServed(t *testing.T) {
	router := NewRouter()

	for _, path := range []string{"/docs", "/openapi.json", "/static/openapi.json", "/debug/pprof/cmdline"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, recorder.Code)
		}
	}
}

// TestServeRootRedirectsToReaderOnFullRouter 在完整 NewRouter 上确认站点根
// 通向 Reader。reader_test.go 已在裸 engine 上覆盖了重定向本身，这条额外锁住
// 「Reader 路由确实被 registerOperationalRoutes 挂进了真实路由表」——
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

type readinessCheckerFunc func(context.Context) error

func (fn readinessCheckerFunc) Ready(ctx context.Context) error {
	return fn(ctx)
}

// smokeDeps returns a Dependencies populated with the same zero-value
// stubs that NewRouter() uses. Tests that exercise operational routes
// (/health, /ready) can use this to satisfy the M4
// boot-time fail-fast check without dragging in handler test fakes.
func smokeDeps() handler.Dependencies {
	return handler.Dependencies{
		LinksWrite: smokeLinkWriteService{},
		LinksRead:  smokeLinkReadService{},
		Ingest:     smokeIngestService{},
		Tags:       smokeTagService{},
		Tree:       smokeTreeService{},
	}
}

// TestRouterMaxRequestBodyEnforcedGlobally 锁定 Wave 5 M4：
// MaxRequestBodyBytes 通过 RouterOptions 挂上去后，所有路由（包括
// 没有 handler-级 MaxBytesReader 的）默认收到全局上限。
func TestRouterMaxRequestBodyEnforcedGlobally(t *testing.T) {
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, RouterOptions{
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
			router := NewRouterWithDependencies(smokeDeps(), nil, nil, RouterOptions{
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
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, RouterOptions{
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
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, RouterOptions{
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
