package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// DefaultGzipMinLength 是 Gzip 中间件的默认压缩下限（字节）。
//
// 低于此值不压缩：gzip 的头尾固定开销约 20 字节，而短 JSON 熵高、压缩比差，
// 压完往往比原文更大——白白搭上 CPU，还让响应多背一次 Vary 协商。
const DefaultGzipMinLength = 1024

// gzipExcludedPaths 是恒不压缩的路径集合。
//
//   - /metrics：Prometheus 抓取端自己会协商编码，且抓取器与服务端通常同机
//     部署，压缩纯属浪费。
//   - /health、/ready：响应体只有几十字节，恒低于任何合理的 minLength，
//     列在这里是为了连"判断一次"都省掉——它们是被高频轮询的端点。
//   - /debug/pprof：profile 产物是二进制，部分端点本身已压缩。
var gzipExcludedPaths = map[string]struct{}{
	"/metrics": {},
	"/health":  {},
	"/ready":   {},
}

// Gzip 返回响应压缩中间件。
//
// # 为什么不用 gin-contrib/gzip
//
// 该库 v1.2.6 的 removeGzipHeaders() 在两条常规路径上（4xx/5xx 响应、
// 未达 minLength 的小响应）执行 Header().Del("Vary")。Del 删的是**整个**
// Vary 头，包括 CORS 中间件设置的 Vary: Origin——而 cors.go 里那行上面
// 挂着一整段注释说明它为什么必须恒在（防止 CDN / 共享缓存把不同 Origin
// 的请求折叠进同一条缓存项）。引入该库等于让绝大多数响应静默丢掉那个
// 不变量。它另外还 Del("ETag") 并把强 ETag 改写成弱 ETag，与后续
// 条件请求（If-None-Match）的设计直接冲突。
//
// # 中间件顺序约束（改动 installRouterMiddleware 时必须一并复核）
//
//  1. **必须排在 Recovery 之前**（即 Recovery 是内层）。panic 时的栈展开
//     顺序是「内层 defer 先跑」：若本中间件在 Recovery 之内，我们的
//     finish() 会先把缓冲区冲出去、提交响应头，Recovery 随后再写它那份
//     JSON 500，得到一个响应头已定、body 被拼接两次的残响应。反过来，
//     Recovery 在内层时它先把 500 写进我们的缓冲区，再由我们统一冲出，
//     响应完整且被正确压缩。
//
//  2. **Vary: Accept-Encoding 只能在"即将出字节"的时刻追加，不能在
//     c.Next() 之前追加**。CORS 中间件用 c.Header("Vary", "Origin")
//     （Set 语义）写 Vary，而它挂在 extraMiddleware 里、排在本中间件
//     之后——先追加就会被它整条覆盖掉。缓冲设计正好给出了这个时机：
//     第一个字节要到达 wire 的那一刻，CORS 的 before-Next 段早已执行完。
//     因此 commit() 与 finish() 各自负责追加，且**全程只 Add 不 Set、
//     更不 Del**，与 CORS 的 Vary: Origin 叠加共存。
func Gzip(minLength int) gin.HandlerFunc {
	if minLength <= 0 {
		minLength = DefaultGzipMinLength
	}
	pool := &sync.Pool{
		New: func() any {
			// 级别 5 而非 DefaultCompression(6)：对 JSON 这类高冗余文本，
			// 5 与 6 的压缩比差异在 1% 量级，CPU 却省下约三成。列表端点
			// 每 30 秒被每个在线 Reader 打一次，这三成是持续成本。
			writer, _ := gzip.NewWriterLevel(nil, 5)
			return writer
		},
	}

	return func(c *gin.Context) {
		if !gzipEligible(c.Request) {
			c.Next()
			return
		}

		gz := pool.Get().(*gzip.Writer)
		writer := &gzipWriter{
			ResponseWriter: c.Writer,
			gz:             gz,
			minLength:      minLength,
		}
		original := c.Writer
		c.Writer = writer

		defer func() {
			writer.finish()
			c.Writer = original
			// 归还池子前把 gzip.Writer 从 ResponseWriter 上摘下来：不摘的话
			// 池里每个空闲 writer 都攥着一个已结束请求的 ResponseWriter，
			// 那条引用会一直把该请求的整条响应对象图钉在堆上。
			gz.Reset(io.Discard)
			pool.Put(gz)
		}()

		c.Next()
	}
}

// gzipEligible 判断这个请求是否有资格进入压缩路径。判断只依赖请求，
// 不依赖响应——响应侧的决定（够不够 minLength、上游是否已自行编码）
// 留给 gzipWriter 在写入时动态处理。
func gzipEligible(req *http.Request) bool {
	// HEAD 不返回 body，压缩没有意义；顺带避免为一个空 body 生成 gzip 头尾。
	if req.Method == http.MethodHead {
		return false
	}
	// Range 请求要的是 identity 表示里的某个字节区间。压缩之后 body 是整体
	// 压缩流的一部分，而 Content-Range 仍然按未压缩坐标声明区间，两者自相
	// 矛盾——curl -r、断点续传、<video>/<audio> 的分段请求都会拿到坏数据。
	// 这条路径真实可达：Go 自托管的 /reader/* 走 http.FileServer，原生支持 Range。
	if req.Header.Get("Range") != "" {
		return false
	}
	if !acceptsGzip(req.Header.Get("Accept-Encoding")) {
		return false
	}
	// 连接升级（WebSocket 等）走的是 Hijack 路径，不能套压缩层。
	if strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade") {
		return false
	}
	if _, excluded := gzipExcludedPaths[req.URL.Path]; excluded {
		return false
	}
	return !strings.HasPrefix(req.URL.Path, "/debug/pprof")
}

// acceptsGzip 按 RFC 9110 §12.5.3 解析 Accept-Encoding。
//
// 不能用 strings.Contains(header, "gzip")：那会把 `gzip;q=0`（客户端**显式
// 拒绝** gzip）读成接受，也会被 `notgzipfoo` 这种子串误命中。q=0 的语义是
// "not acceptable"，照给就是服务端无视客户端的明确表态。
func acceptsGzip(header string) bool {
	wildcardAcceptable := false
	wildcardSeen := false

	for _, part := range strings.Split(header, ",") {
		token, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		token = strings.ToLower(strings.TrimSpace(token))
		switch token {
		case "gzip":
			// 显式列出 gzip 时它说了算，不再看通配符。
			return encodingQualityPositive(params)
		case "*":
			wildcardSeen = true
			wildcardAcceptable = encodingQualityPositive(params)
		}
	}

	// 没有显式提到 gzip 时才轮到通配符表态。
	return wildcardSeen && wildcardAcceptable
}

// encodingQualityPositive 报告参数串里的 q 值是否 > 0。缺省（没有 q 参数）
// 按 RFC 视为 q=1；无法解析的 q 值同样按缺省处理，宁可压也不要因为一个畸形
// 参数就退化成不压。
func encodingQualityPositive(params string) bool {
	for _, param := range strings.Split(params, ";") {
		name, value, found := strings.Cut(strings.TrimSpace(param), "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}
		quality, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return true
		}
		return quality > 0
	}
	return true
}

// gzipMode 是 gzipWriter 的三态。初始为 buffering：在攒够 minLength 之前
// 既不落盘也不压缩，因为此时还不知道该走哪条路。
type gzipMode int

const (
	// gzipBuffering —— 还没攒够 minLength，数据暂存 buffer。
	gzipBuffering gzipMode = iota
	// gzipCompressing —— 已提交压缩：响应头已写 Content-Encoding，
	// 后续写入直接进 gzip.Writer。
	gzipCompressing
	// gzipPassthrough —— 已提交不压缩：直接写底层 Writer。
	gzipPassthrough
)

// gzipWriter 包装 gin.ResponseWriter，按 minLength 动态决定是否压缩。
//
// 缓冲是必需的而不是优化：绝大多数 gin handler（c.JSON 等）不设
// Content-Length，因此在第一次 Write 之前无从知道响应有多大，而
// Content-Encoding 又必须赶在第一个字节之前落定。攒够阈值再决定是唯一
// 能同时做到「小响应不压缩」和「响应头正确」的办法。
type gzipWriter struct {
	gin.ResponseWriter
	gz        *gzip.Writer
	buffer    []byte
	minLength int
	mode      gzipMode
}

func (w *gzipWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *gzipWriter) Write(data []byte) (int, error) {
	switch w.mode {
	case gzipCompressing:
		return w.gz.Write(data)
	case gzipPassthrough:
		return w.ResponseWriter.Write(data)
	}

	// buffering。两种情况必须当场让路，不能再套一层压缩：
	//   · 上游 handler 自己设了 Content-Encoding（直接吐一份已压缩的产物）。
	//   · 响应是 206 Partial Content。即便请求侧的 Range 检查已经挡掉了绝大
	//     多数情形，handler 仍可能自行产出 206；压了它，Content-Range 声明的
	//     区间与实际 body 就对不上。
	if w.Header().Get("Content-Encoding") != "" || w.Status() == http.StatusPartialContent {
		w.commitPassthrough()
		return w.ResponseWriter.Write(data)
	}

	w.buffer = append(w.buffer, data...)
	if len(w.buffer) < w.minLength {
		// 已如实收下这批字节；调用方按 io.Writer 契约只关心「有没有全收」。
		return len(data), nil
	}

	w.commitCompressing() // 内部排空缓冲区
	return len(data), nil
}

// commitCompressing 提交「这个响应要压缩」：写响应头、接上 gzip.Writer，
// 并把缓冲区排空进去。必须在任何字节到达 wire 之前调用。
func (w *gzipWriter) commitCompressing() {
	header := w.Header()
	// Content-Type 必须在压缩前按**未压缩**内容定好。留空的话 net/http 会对
	// 首块 body 做 DetectContentType，而那时它看到的已经是 gzip 字节，嗅探结果
	// 是 application/x-gzip；再叠加 SecurityHeaders 的 X-Content-Type-Options:
	// nosniff，浏览器会直接拒绝渲染。
	if header.Get("Content-Type") == "" && len(w.buffer) > 0 {
		header.Set("Content-Type", http.DetectContentType(w.buffer))
	}
	header.Set("Content-Encoding", "gzip")
	// 压缩后长度与上游声明的不符，留着会让客户端按错误长度截断。
	header.Del("Content-Length")
	w.addVary()
	w.gz.Reset(w.ResponseWriter)
	w.mode = gzipCompressing

	if len(w.buffer) > 0 {
		buffered := w.buffer
		w.buffer = nil
		_, _ = w.gz.Write(buffered)
	}
}

// commitPassthrough 提交「这个响应不压缩」，并把缓冲区原样排空。
//
// 排空这一步不是可选的：曾经的版本只切 mode 不动 buffer，于是「先写了几百
// 字节、再设 Content-Encoding」这条路径会让已收下的字节永久消失——finish()
// 里的 `mode == gzipBuffering` 此时已不成立，没有任何人再去看那个 buffer，
// 而且全程无错误。
//
// 仍然追加 Vary：同一个 URL 对别的客户端**是**会变的，按 RFC 9110 §12.5.5
// 这时也该声明 Vary，否则共享缓存可能把这份未压缩响应回给一个只收 gzip 的
// 客户端。
func (w *gzipWriter) commitPassthrough() {
	w.addVary()
	w.mode = gzipPassthrough
	if len(w.buffer) > 0 {
		buffered := w.buffer
		w.buffer = nil
		_, _ = w.ResponseWriter.Write(buffered)
	}
}

// addVary 追加 Vary: Accept-Encoding。
//
// 只用 Add：CORS 中间件已经用 Set 写过 Vary: Origin，Set 会整条覆盖、
// Del 会连它一起删。两个值必须共存，缺任何一个都会让共享缓存串味。
// 幂等由 hasVaryAcceptEncoding 保证——commit 路径互斥，正常只会调用一次，
// 但 Flush 提前提交的路径下多调一次也不该产生重复值。
func (w *gzipWriter) addVary() {
	header := w.Header()
	for _, existing := range header.Values("Vary") {
		if strings.EqualFold(strings.TrimSpace(existing), "Accept-Encoding") {
			return
		}
	}
	header.Add("Vary", "Accept-Encoding")
}

// Flush 让流式端点（/api/export 系列按游标批次直写 c.Writer）能把已产出的
// 部分推给客户端。仍在缓冲态时，显式 Flush 表达的是「现在就出字节」，
// 因此当场按当前缓冲量决定走哪条路，不再等阈值。
func (w *gzipWriter) Flush() {
	if w.mode == gzipBuffering {
		w.commitByBufferSize()
	}
	if w.mode == gzipCompressing {
		_ = w.gz.Flush()
	}
	w.ResponseWriter.Flush()
}

// finish 在中间件退出时收尾：把仍在缓冲区里的数据按当前体量决定压不压，
// 然后关闭 gzip.Writer 写出footer。
func (w *gzipWriter) finish() {
	if w.mode == gzipBuffering {
		w.commitByBufferSize()
	}
	if w.mode == gzipCompressing {
		_ = w.gz.Close()
	}
}

// commitByBufferSize 是缓冲态的收尾决策：够阈值就压，不够就原样吐出。
// 两条分支各自负责排空缓冲区。
func (w *gzipWriter) commitByBufferSize() {
	if len(w.buffer) >= w.minLength {
		w.commitCompressing()
		return
	}
	w.commitPassthrough()
}
