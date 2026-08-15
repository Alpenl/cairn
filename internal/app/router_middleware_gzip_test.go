package app

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"webtag/internal/middleware"
	"webtag/internal/observability"
)

// 这个文件补的是「装配层无人守卫」这个洞。
//
// internal/middleware/gzip_test.go 里的全部用例都跑在手搭的 gin.New() 上，
// 于是下面这些改动可以在全仓测试全绿的情况下发生：
//   · 删掉 router_middleware.go 里的 router.Use(middleware.Gzip(...)) —— 压缩静默消失
//   · 删掉 deps_router.go 里 GzipEnabled/GzipMinLength 的透传 —— 零值 false，压缩静默关闭
//   · 把 Gzip 挪到 Recovery 之后 —— 顺序约束被破坏，panic 响应变成不可解压的残流
// 本文件的用例走真实的 installRouterMiddleware，让上述每一条都变红。

func gzipTestRouter(t *testing.T, opts RouterOptions, handlers func(*gin.Engine)) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	installRouterMiddleware(router, nil, nil, opts)
	handlers(router)
	return router
}

// bulkJSON 产出一段稳定超过 DefaultGzipMinLength 的响应体。
func bulkJSON() string {
	var builder strings.Builder
	builder.WriteString(`{"items":[`)
	for i := 0; i < 40; i++ {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(`{"id":"link-`)
		builder.WriteString(strings.Repeat("x", 12))
		builder.WriteString(`","summary":"一段足够长的摘要文本用于越过压缩阈值"}`)
	}
	builder.WriteString(`]}`)
	return builder.String()
}

func decodeGzip(t *testing.T, data []byte) string {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader() error = %v", err)
	}
	defer func() { _ = reader.Close() }()
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read gzip body error = %v", err)
	}
	return string(out)
}

// TestInstallRouterMiddlewareMountsGzipWhenEnabled 守卫 router_middleware.go
// 里的挂载行与 deps_router.go 的配置透传。删掉任意一处这条都会变红。
func TestInstallRouterMiddlewareMountsGzipWhenEnabled(t *testing.T) {
	t.Parallel()
	body := bulkJSON()
	router := gzipTestRouter(t, RouterOptions{
		AppEnv:        "dev",
		GzipEnabled:   true,
		GzipMinLength: middleware.DefaultGzipMinLength,
	}, func(engine *gin.Engine) {
		engine.GET("/api/links", func(c *gin.Context) { c.String(http.StatusOK, body) })
	})

	req := httptest.NewRequest(http.MethodGet, "/api/links", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip —— 生产装配没有挂上压缩中间件", got)
	}
	if decodeGzip(t, rec.Body.Bytes()) != body {
		t.Fatal("解压结果与原响应不一致")
	}
}

// TestInstallRouterMiddlewareSkipsGzipWhenDisabled 守卫 GZIP_ENABLED=false
// 这条真实分支。
//
// 它替换掉的旧用例（middleware 包里的 TestGzipDisabledPathLeavesResponseUntouched）
// 是无效测试：那个用例搭的是一个**从来没挂过 Gzip** 的裸 router，等于在断言
// 「不用中间件就不会压缩」，把 router_middleware.go 的 if 改成恒真也不会红。
func TestInstallRouterMiddlewareSkipsGzipWhenDisabled(t *testing.T) {
	t.Parallel()
	body := bulkJSON()
	router := gzipTestRouter(t, RouterOptions{
		AppEnv:        "dev",
		GzipEnabled:   false,
		GzipMinLength: middleware.DefaultGzipMinLength,
	}, func(engine *gin.Engine) {
		engine.GET("/api/links", func(c *gin.Context) { c.String(http.StatusOK, body) })
	})

	req := httptest.NewRequest(http.MethodGet, "/api/links", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty when GzipEnabled=false", got)
	}
	if rec.Body.String() != body {
		t.Fatal("GZIP_ENABLED=false 时响应体应与未引入压缩前逐字节一致")
	}
}

// TestInstallRouterMiddlewareGzipRunsOutsideRecovery 把「Gzip 必须排在
// Recovery 之前」这条顺序约束钉在**生产装配处**。
//
// middleware 包里的同名断言守的是那个包自己搭的路由；把
// router_middleware.go 里的 Gzip 挪到 Recovery 之后，那条测试不会有反应，
// 这条会。
func TestInstallRouterMiddlewareGzipRunsOutsideRecovery(t *testing.T) {
	t.Parallel()
	partial := bulkJSON() // 超过阈值，写完即提交压缩

	router := gzipTestRouter(t, RouterOptions{
		AppEnv:        "dev",
		GzipEnabled:   true,
		GzipMinLength: middleware.DefaultGzipMinLength,
	}, func(engine *gin.Engine) {
		engine.GET("/api/links", func(c *gin.Context) {
			_, _ = c.Writer.WriteString(partial)
			panic("boom after partial write")
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/api/links", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
	// 顺序错误时 Recovery 的 JSON 500 会以未压缩字节拼在已 Close 的 gzip 流尾部，
	// 这里当场炸在 "invalid header" 上。
	reader, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("响应声明 gzip 但不是合法 gzip 流（Gzip 与 Recovery 顺序反了）: %v", err)
	}
	defer func() { _ = reader.Close() }()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("gzip 流被截断或尾部有垃圾（Gzip 与 Recovery 顺序反了）: %v", err)
	}
	if !strings.HasPrefix(string(decoded), partial) {
		t.Fatal("panic 之前写入的字节丢失")
	}
	var envelope map[string]any
	tail := strings.TrimPrefix(string(decoded), partial)
	if err := json.Unmarshal([]byte(tail), &envelope); err != nil {
		t.Fatalf("Recovery 的错误信封不在压缩流内: %v (tail=%q)", err, tail)
	}
}

// TestInstallRouterMiddlewareGzipPreservesVaryOriginThroughRealChain 在真实
// 中间件链（含 CORS 作为 extraMiddleware，与生产装配同构）上验证两个 Vary
// 值共存。middleware 包的同名用例是在手搭链上验的。
func TestInstallRouterMiddlewareGzipPreservesVaryOriginThroughRealChain(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	installRouterMiddleware(router, nil, nil, RouterOptions{
		AppEnv:        "dev",
		GzipEnabled:   true,
		GzipMinLength: middleware.DefaultGzipMinLength,
	}, middleware.CORS([]string{"https://reader.example.com"}))
	body := bulkJSON()
	router.GET("/api/links", func(c *gin.Context) { c.String(http.StatusOK, body) })

	req := httptest.NewRequest(http.MethodGet, "/api/links", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Origin", "https://reader.example.com")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	joined := strings.ToLower(strings.Join(rec.Header().Values("Vary"), ", "))
	if !strings.Contains(joined, "origin") {
		t.Errorf("Vary = %v, missing Origin", rec.Header().Values("Vary"))
	}
	if !strings.Contains(joined, "accept-encoding") {
		t.Errorf("Vary = %v, missing Accept-Encoding", rec.Header().Values("Vary"))
	}
}

// TestGzipDoesNotDoubleCompressRealMetricsEndpoint 在真实的 /metrics 路由
// （gin.WrapH 包的 promhttp handler）上验证排除生效。
//
// 断言口径要留意：promhttp.HandlerFor 自带 gzip 协商
// （internal/observability/metrics.go:412 用的是默认 HandlerOpts{}，
// DisableCompression 为 false），所以带 Accept-Encoding: gzip 时
// /metrics **本来就会**是 gzip 的——那一层是 Prometheus 自己压的。
// 因此这里不能断言「没有 Content-Encoding」，要断言的是**只有一层**：
// 解压一次就应当直接得到明文展示格式，而不是又一段 gzip 字节。
// 我们的中间件若没有排除 /metrics，这里就会是两层。
func TestGzipDoesNotDoubleCompressRealMetricsEndpoint(t *testing.T) {
	t.Parallel()
	metrics := observability.NewMetrics()
	newRouter := func() *gin.Engine {
		return NewRouterWithDependencies(smokeDeps(), metrics.Handler(), nil, nil, metrics, RouterOptions{
			AppEnv:        "dev",
			GzipEnabled:   true,
			GzipMinLength: middleware.DefaultGzipMinLength,
		})
	}

	// 不带 Accept-Encoding：任何一层都不该压，必须是明文。
	plain := httptest.NewRecorder()
	newRouter().ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if plain.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", plain.Code)
	}
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("/metrics 无 Accept-Encoding 时 Content-Encoding = %q, want empty", got)
	}
	if !strings.Contains(plain.Body.String(), "# HELP") {
		t.Fatal("/metrics 明文响应不是 Prometheus 展示格式")
	}

	// 带 Accept-Encoding: gzip：promhttp 自己压一层，我们不能再叠一层。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	newRouter().ServeHTTP(rec, req)

	decoded := decodeGzip(t, rec.Body.Bytes())
	if !strings.Contains(decoded, "# HELP") {
		t.Fatalf("解压一次后不是 Prometheus 明文（说明被压了两层）: 前 32 字节 = %q",
			decoded[:min(32, len(decoded))])
	}
}
