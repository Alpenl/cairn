package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// largeBody 是一段稳定超过 DefaultGzipMinLength 的 JSON 风格文本，
// 用来把响应推过压缩阈值。内容有冗余，压缩比接近真实的列表响应。
func largeBody() string {
	var builder strings.Builder
	builder.WriteString(`{"items":[`)
	for i := 0; i < 60; i++ {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(`{"id":"00000000-0000-0000-0000-0000000000`)
		builder.WriteString("00")
		builder.WriteString(`","summary":"链接摘要正文，重复内容用于制造可压缩的冗余"}`)
	}
	builder.WriteString(`]}`)
	return builder.String()
}

func gunzip(t *testing.T, data []byte) string {
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

// newGzipRouter 装配一个只挂 Gzip 的最小路由，body 由调用方给定。
func newGzipRouter(body string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Gzip(DefaultGzipMinLength))
	router.GET("/api/links", func(c *gin.Context) {
		c.String(http.StatusOK, body)
	})
	router.GET("/health", func(c *gin.Context) {
		c.String(http.StatusOK, body)
	})
	router.GET("/metrics", func(c *gin.Context) {
		c.String(http.StatusOK, body)
	})
	return router
}

func doGzipGet(router *gin.Engine, path string, acceptEncoding string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestGzipCompressesLargeResponseAndRoundTripsByteForByte 是本中间件的
// 主验收：超过阈值的响应带 Content-Encoding: gzip，且解压结果与不带
// Accept-Encoding 时的响应逐字节相同。压缩改变的只能是传输编码，
// 一个字节的语义差异都不允许。
func TestGzipCompressesLargeResponseAndRoundTripsByteForByte(t *testing.T) {
	t.Parallel()
	body := largeBody()
	router := newGzipRouter(body)

	plain := doGzipGet(router, "/api/links", "")
	if plain.Header().Get("Content-Encoding") != "" {
		t.Fatalf("Content-Encoding = %q, want empty without Accept-Encoding", plain.Header().Get("Content-Encoding"))
	}

	compressed := doGzipGet(router, "/api/links", "gzip")
	if got := compressed.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := gunzip(t, compressed.Body.Bytes()); got != plain.Body.String() {
		t.Fatalf("decompressed body differs from plain body")
	}
	if compressed.Body.Len() >= plain.Body.Len() {
		t.Fatalf("compressed %d bytes >= plain %d bytes, compression achieved nothing",
			compressed.Body.Len(), plain.Body.Len())
	}
	// 量化门槛：JSON 实测通常落在 15~25%，留到 40% 作为不至于误报的红线。
	// 低于这个比例说明压缩根本没生效或级别被改坏了。
	if ratio := float64(compressed.Body.Len()) / float64(plain.Body.Len()); ratio > 0.40 {
		t.Fatalf("compression ratio = %.2f, want <= 0.40", ratio)
	}
}

// TestGzipSkipsResponsesBelowMinLength 锁定小响应不进压缩路径：
// gzip 头尾固定开销会让短 JSON 压完更大。
func TestGzipSkipsResponsesBelowMinLength(t *testing.T) {
	t.Parallel()
	router := newGzipRouter(`{"ok":true}`)

	rec := doGzipGet(router, "/api/links", "gzip")
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty for sub-threshold body", got)
	}
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Fatalf("body = %q, want the original short body", got)
	}
}

// TestGzipSkipsExcludedPaths 锁定 /health 与 /metrics 不被压缩——它们是
// 被高频轮询的端点，压缩只有开销没有收益。
func TestGzipSkipsExcludedPaths(t *testing.T) {
	t.Parallel()
	router := newGzipRouter(largeBody())

	for _, path := range []string{"/health", "/metrics"} {
		rec := doGzipGet(router, path, "gzip")
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("%s Content-Encoding = %q, want empty", path, got)
		}
	}
}

// TestGzipSkipsHeadRequests 锁定 HEAD 不进压缩路径：它不返回 body，
// 压缩只会凭空生成一副 gzip 头尾。
func TestGzipSkipsHeadRequests(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Gzip(DefaultGzipMinLength))
	router.HEAD("/api/links", func(c *gin.Context) {
		c.String(http.StatusOK, largeBody())
	})

	req := httptest.NewRequest(http.MethodHead, "/api/links", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty for HEAD", got)
	}
}

// TestGzipPreservesCORSVaryOrigin 是本中间件不采用 gin-contrib/gzip 的
// 直接原因所对应的回归测试。
//
// 该库的 removeGzipHeaders() 执行 Header().Del("Vary")，删掉的是整条
// Vary 头——包括 CORS 写的 Vary: Origin。cors.go 里那行上面挂着一整段
// 注释解释它为什么必须恒在（防止共享缓存把不同 Origin 的请求折叠成同
// 一条缓存项）。这里对**三条路径**都断言两个 Vary 值共存：压缩的、
// 未达阈值的、以及错误响应——后两条正是该库会删掉 Vary 的路径。
func TestGzipPreservesCORSVaryOrigin(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name string
		body string
		code int
	}{
		{name: "compressed", body: largeBody(), code: http.StatusOK},
		{name: "below min length", body: `{"ok":true}`, code: http.StatusOK},
		{name: "error response", body: `{"error":{"message":"boom"}}`, code: http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			router := gin.New()
			// 生产顺序：Gzip 在外，CORS 在内（CORS 来自 extraMiddleware）。
			router.Use(Gzip(DefaultGzipMinLength))
			router.Use(CORS([]string{"https://reader.example.com"}))
			router.GET("/api/links", func(c *gin.Context) {
				c.String(tc.code, tc.body)
			})

			req := httptest.NewRequest(http.MethodGet, "/api/links", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			req.Header.Set("Origin", "https://reader.example.com")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			values := rec.Header().Values("Vary")
			joined := strings.ToLower(strings.Join(values, ", "))
			if !strings.Contains(joined, "origin") {
				t.Errorf("Vary = %v, missing Origin (CORS 的缓存不变量被压缩层吃掉了)", values)
			}
			if !strings.Contains(joined, "accept-encoding") {
				t.Errorf("Vary = %v, missing Accept-Encoding", values)
			}
		})
	}
}

// TestGzipRecoveryOrderKeepsPanicResponseDecodable 锁定「Gzip 在外、
// Recovery 在内」这个顺序约束。
//
// handler 必须**先写过阈值再 panic**，这条测试才有意义：panic 前一个
// 字节没写时，缓冲区是空的，两种顺序产出完全一样的响应（本测试的上一版
// 就是这样，翻转顺序照样绿，等于没测）。写过阈值之后压缩已经提交，
// 两种顺序才分道扬镳：
//
//   - Gzip 在外（正确）：Recovery 的 defer 更内层、先执行，它那份 JSON 500
//     写进尚未冲出的 gzip 流里，最后由 Gzip 的 finish() 统一 Close。
//     客户端拿到的是一条完整、可解压的 gzip 流。
//   - Gzip 在内（错误）：Gzip 的 finish() 先跑，Close 掉 gzip 流并提交响应；
//     Recovery 随后写的 JSON 500 以**未压缩的原始字节**拼在 gzip 流尾部。
//     响应头仍写着 Content-Encoding: gzip，于是客户端解压时撞上尾部垃圾。
func TestGzipRecoveryOrderKeepsPanicResponseDecodable(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	partial := largeBody() // 超过 minLength，写完即提交压缩

	router := gin.New()
	router.Use(Gzip(DefaultGzipMinLength))
	router.Use(Recovery(slog.New(slog.NewTextHandler(io.Discard, nil))))
	router.GET("/api/links", func(c *gin.Context) {
		_, _ = c.Writer.WriteString(partial)
		panic("boom after partial write")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/links", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip (handler wrote past the threshold)",
			rec.Header().Get("Content-Encoding"))
	}
	// 核心断言：响应头声明了 gzip，body 就必须是一条能完整解压的 gzip 流。
	// 顺序错误时尾部会挂上未压缩的 JSON，这里当场炸。
	reader, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("response declares gzip but is not a gzip stream: %v", err)
	}
	defer func() { _ = reader.Close() }()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("gzip stream is truncated or has trailing garbage: %v", err)
	}

	if !strings.HasPrefix(string(decoded), partial) {
		t.Fatalf("decoded body lost the bytes written before the panic")
	}
	// Recovery 的错误信封必须也在这条流里，而不是掉在流外面。
	if !strings.Contains(string(decoded), ErrCodeInternalError) {
		t.Fatalf("panic envelope %q missing from the compressed stream; Recovery wrote outside the gzip layer",
			ErrCodeInternalError)
	}
}

// TestGzipDoesNotDoubleCompress 锁定上游 handler 自己设了 Content-Encoding
// 时中间件让路，不再套第二层——双重压缩的客户端解一次得到的是 gzip 字节。
func TestGzipDoesNotDoubleCompress(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	payload := []byte("already encoded payload that is quite long " + largeBody())
	router := gin.New()
	router.Use(Gzip(DefaultGzipMinLength))
	router.GET("/api/export", func(c *gin.Context) {
		c.Header("Content-Encoding", "br")
		c.Data(http.StatusOK, "application/json", payload)
	})

	rec := doGzipGet(router, "/api/export", "gzip")
	if got := rec.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want br preserved", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("body was re-encoded on top of an existing Content-Encoding")
	}
}

// TestGzipDropsStaleContentLength 锁定压缩时上游声明的 Content-Length
// 被删除。留着它客户端会按未压缩长度截断响应。
func TestGzipDropsStaleContentLength(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	body := largeBody()
	router := gin.New()
	router.Use(Gzip(DefaultGzipMinLength))
	router.GET("/api/links", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", []byte(body))
	})

	rec := doGzipGet(router, "/api/links", "gzip")
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed when compressing", got)
	}
	if got := gunzip(t, rec.Body.Bytes()); got != body {
		t.Fatalf("round-trip mismatch")
	}
}

// TestGzipStreamsIncrementallyOnFlush 锁定流式端点（/api/export 系列按
// 游标批次直写 c.Writer）在压缩层下仍然是流式的：handler 每写一批并
// Flush，字节就该到客户端，而不是攒到请求结束才一次性吐出。
//
// 这条不是理论关切：export 的整个设计前提就是"从不缓冲超过一批"，
// 一个会整体缓冲的压缩层会让知识库导出重新变成内存炸弹。
func TestGzipStreamsIncrementallyOnFlush(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	batch := strings.Repeat("streamed export batch payload; ", 80) // 远超阈值
	observed := make([]int, 0, 3)

	router := gin.New()
	router.Use(Gzip(DefaultGzipMinLength))
	router.GET("/api/export", func(c *gin.Context) {
		for i := 0; i < 3; i++ {
			_, _ = c.Writer.WriteString(batch)
			c.Writer.Flush()
			observed = append(observed, c.Writer.Size())
		}
	})

	rec := doGzipGet(router, "/api/export", "gzip")

	// 每次 Flush 之后累计出网字节都应严格增长——说明数据是分批出去的。
	for i := 1; i < len(observed); i++ {
		if observed[i] <= observed[i-1] {
			t.Fatalf("bytes on the wire after flush %d = %d, not greater than previous %d; response was buffered whole",
				i, observed[i], observed[i-1])
		}
	}
	if got := gunzip(t, rec.Body.Bytes()); got != strings.Repeat(batch, 3) {
		t.Fatalf("streamed body round-trip mismatch")
	}
}

// TestGzipSkipsRangeRequests 锁定 Range 请求不被压缩。
//
// 压了它响应就自相矛盾：客户端要的是 identity 表示里的某个字节区间，
// Content-Range 也按未压缩坐标声明区间，而 body 却是压缩流的一部分。
// 这条路径真实可达——Go 自托管的 /reader/* 走 http.FileServer，原生支持 Range。
func TestGzipSkipsRangeRequests(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	payload := []byte(strings.Repeat("0123456789", 20000)) // 200000 B
	router := gin.New()
	router.Use(Gzip(DefaultGzipMinLength))
	router.GET("/reader/asset.js", func(c *gin.Context) {
		http.ServeContent(c.Writer, c.Request, "asset.js", time.Time{}, bytes.NewReader(payload))
	})

	req := httptest.NewRequest(http.MethodGet, "/reader/asset.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Range", "bytes=0-49999")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty for a Range request", got)
	}
	if rec.Body.Len() != 50000 {
		t.Fatalf("body = %d B, want 50000 —— Content-Range 声明的区间与实际 body 不符", rec.Body.Len())
	}
	if !bytes.Equal(rec.Body.Bytes(), payload[:50000]) {
		t.Fatal("返回的区间内容不是原始字节")
	}
}

// TestGzipHonoursAcceptEncodingQualityZero 锁定 RFC 9110 §12.5.3 的 q=0
// 语义：`gzip;q=0` 是客户端**显式拒绝** gzip，不是接受。
//
// 用 strings.Contains 判定会把这些全部读成"接受"，等于服务端无视客户端的
// 明确表态；`notgzipfoo` 那条则是纯粹的子串误命中。
func TestGzipHonoursAcceptEncodingQualityZero(t *testing.T) {
	t.Parallel()

	cases := []struct {
		header string
		want   bool
	}{
		{header: "gzip", want: true},
		{header: "gzip, deflate, br", want: true},
		{header: "GZIP", want: true},
		{header: " gzip ; q=0.5 ", want: true},
		{header: "*", want: true},
		{header: "gzip;q=0", want: false},
		{header: "identity, gzip;q=0", want: false},
		{header: "deflate, gzip;q=0.0", want: false},
		{header: "notgzipfoo", want: false},
		{header: "br", want: false},
		{header: "*;q=0", want: false},
		{header: "", want: false},
		// 显式列出的 gzip 说了算，通配符不该反悔。
		{header: "*, gzip;q=0", want: false},
		{header: "*;q=0, gzip", want: true},
	}

	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			t.Parallel()
			router := newGzipRouter(largeBody())
			rec := doGzipGet(router, "/api/links", tc.header)
			compressed := rec.Header().Get("Content-Encoding") == "gzip"
			if compressed != tc.want {
				t.Fatalf("Accept-Encoding %q -> compressed=%v, want %v", tc.header, compressed, tc.want)
			}
		})
	}
}

// TestGzipPassthroughAfterPartialWriteKeepsAllBytes 锁定「先写了一些字节、
// 之后才出现不能压缩的理由」这条路径不丢数据。
//
// 早先的实现里 commitPassthrough 只切换模式、不排空缓冲区，而 finish() 的
// `mode == gzipBuffering` 分支此时已不成立——缓冲区里那几十字节永久消失，
// 全程没有任何错误。这条测试就是钉住那个洞。
func TestGzipPassthroughAfterPartialWriteKeepsAllBytes(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	const first = "FIRST-CHUNK-BEFORE-HEADER;"
	second := strings.Repeat("SECOND;", 300)

	router := gin.New()
	router.Use(Gzip(DefaultGzipMinLength))
	router.GET("/api/thing", func(c *gin.Context) {
		// 先写一小段（仍在缓冲区里，未达阈值）……
		_, _ = c.Writer.WriteString(first)
		// ……然后才声明自己已经是别的编码。
		c.Writer.Header().Set("Content-Encoding", "br")
		_, _ = c.Writer.WriteString(second)
	})

	rec := doGzipGet(router, "/api/thing", "gzip")

	if got := rec.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want br preserved", got)
	}
	if got := rec.Body.String(); got != first+second {
		t.Fatalf("body 丢字节：len=%d want=%d, 前 40 字节 = %q",
			len(got), len(first+second), got[:min(40, len(got))])
	}
}

// TestGzipSetsContentTypeBeforeCompressing 锁定 Content-Type 按**未压缩**
// 内容确定。
//
// handler 未显式设 Content-Type 时，net/http 会对首块 body 做
// DetectContentType；若那时看到的已经是 gzip 字节，嗅探结果就是
// application/x-gzip，再叠加 SecurityHeaders 的 X-Content-Type-Options:
// nosniff，浏览器会直接拒绝渲染。
func TestGzipSetsContentTypeBeforeCompressing(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	html := "<!doctype html><html><body>" + strings.Repeat("<p>正文段落</p>", 200) + "</body></html>"
	router := gin.New()
	router.Use(Gzip(DefaultGzipMinLength))
	router.GET("/reader/", func(c *gin.Context) {
		// 刻意不设 Content-Type，交给嗅探。
		_, _ = c.Writer.WriteString(html)
	})

	rec := doGzipGet(router, "/reader/", "gzip")

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	contentType := rec.Header().Get("Content-Type")
	if strings.Contains(contentType, "gzip") {
		t.Fatalf("Content-Type = %q —— 嗅探看到的是压缩后的字节", contentType)
	}
	if !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html*", contentType)
	}
}

// TestGzipStreamsExportWithoutExplicitFlush 覆盖**真实**的流式导出形态。
//
// 与 TestGzipStreamsIncrementallyOnFlush 的区别很重要：那条测试的假 handler
// 每批都显式调 c.Writer.Flush()，而真正的 /api/export 系列从不调 Flush
// （internal/handler/export.go 只是把 c.Writer 交给 service.Export，
// 全仓 grep "Flush()" 在该文件零命中）。所以那条测的是一条生产里不存在的
// 路径。这里按真实形态验证：只写不 Flush，压缩层也不能把整个导出攒在内存里。
func TestGzipStreamsExportWithoutExplicitFlush(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	batch := strings.Repeat("{\"id\":\"link\",\"summary\":\"导出批次\"},", 40) // 每批远超阈值
	const batches = 5
	peakBuffered := 0

	router := gin.New()
	router.Use(Gzip(DefaultGzipMinLength))
	router.GET("/api/export", func(c *gin.Context) {
		writer, ok := c.Writer.(*gzipWriter)
		if !ok {
			t.Errorf("c.Writer 不是 gzipWriter，压缩层没挂上")
			return
		}
		for i := 0; i < batches; i++ {
			_, _ = c.Writer.WriteString(batch)
			if len(writer.buffer) > peakBuffered {
				peakBuffered = len(writer.buffer)
			}
		}
	})

	rec := doGzipGet(router, "/api/export", "gzip")

	// 核心断言：缓冲区峰值不超过阈值。整体缓冲的实现会攒到 batches*len(batch)。
	if peakBuffered > DefaultGzipMinLength {
		t.Fatalf("缓冲区峰值 = %d B，超过 minLength %d —— 压缩层把流式导出变成了整体缓冲",
			peakBuffered, DefaultGzipMinLength)
	}
	if got := gunzip(t, rec.Body.Bytes()); got != strings.Repeat(batch, batches) {
		t.Fatal("流式导出的内容在压缩后不完整")
	}
}
