package app

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

// readerFS 内嵌 Reader（reader/ 目录下的 Vite + React 应用）的构建产物。
//
// 目录在干净检出下只有一个 .gitkeep：产物由 `make reader-build` 或 Dockerfile
// 的 reader-builder 阶段写入，不进版本控制。`all:` 前缀是必需的——没有它，
// 嵌入会跳过以 `.` 开头的文件，空目录（只剩 .gitkeep）会让编译直接失败。
//
// 这样切分的意义：`go build` / `go test ./...` 在没有 Node 工具链的机器上照常
// 通过，只是运行期 /reader/ 返回一页说明而不是应用本体。发布镜像里两者都在。
//
//go:embed all:assets/reader
var readerFS embed.FS

// readerAssets 是 assets/reader 的子树，在 init 阶段解析一次，避免每个请求都付
// fs.Sub 的代价。
var readerAssets = resolveReaderAssets()

func resolveReaderAssets() fs.FS {
	sub, err := fs.Sub(readerFS, "assets/reader")
	if err != nil {
		// 嵌入指令保证 assets/reader 在编译期存在，走到这里意味着二进制损坏。
		// 与 static.go 的 mustSubAssets 同样 fail-fast。
		panic("internal/app: 内嵌 assets/reader 子树缺失，构建产物已损坏: " + err.Error())
	}
	return sub
}

// readerNotBuiltPage 是 Reader 产物缺席时 /reader/ 返回的说明页。
//
// 不复用 404：运维要区分「路由没挂上」和「二进制里没带前端」，前者是 bug，
// 后者是构建方式的选择。给一页能照着做的说明比一个裸 404 有用得多。
const readerNotBuiltPage = `<!doctype html>
<html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Reader 尚未构建 - Cairn</title>
<style>
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
background:#f6f4f1;color:#2c2723;font:15px/1.7 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
main{max-width:560px;padding:32px 36px;background:#fff;border:1px solid #e4ded6;border-radius:10px}
h1{margin:0 0 12px;font-size:19px}
code{background:#f2efec;border-radius:4px;padding:2px 6px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px}
pre{background:#f2efec;border-radius:6px;padding:12px 14px;overflow-x:auto;font-size:13px}
a{color:#b5502f}
</style></head>
<body><main>
<h1>Reader 尚未构建</h1>
<p>后端已就绪，但这个二进制没有内嵌 Reader 前端产物。</p>
<p>从源码构建完整二进制时，运行：</p>
<pre>make build-full     # 构建 Reader，注入静态产物，再编译后端</pre>
<p>后端 API 不受影响，可继续使用：<a href="/docs">API 文档</a> ·
<a href="/openapi.json">openapi.json</a> · <a href="/health">/health</a></p>
</main></body></html>`

// registerReaderRoutes 把 Reader 挂到 /reader/ 下，并让站点根跳转过去。
//
// 路由形状：
//   - GET /            → 302 /reader/
//   - GET /reader      → 301 /reader/（补尾斜杠，否则相对资源路径会算错一层）
//   - GET /reader/*any → 静态产物；命中不到且不像文件的路径回落 index.html
//
// 回落 index.html 是 SPA 的必要条件：Reader 用 query 参数表达视图
// （?view=subscriptions 等），但直接深链到 /reader/xxx 时不能 404，否则浏览器
// 刷新就把用户打到错误页。带扩展名的路径（.js/.css/.png…）不回落——那是资源
// 请求，回落 HTML 只会让浏览器拿到一段 MIME 不符的内容并报更难懂的错。
func registerReaderRoutes(router *gin.Engine) {
	router.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, withQuery("/reader/", c))
	})
	router.GET("/reader", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, withQuery("/reader/", c))
	})
	router.GET("/reader/*filepath", serveReader)
	router.HEAD("/reader/*filepath", serveReader)
}

// withQuery 把原请求的 query 带到重定向目标上。
//
// 不带会静默丢参数：Reader 用 query 表达视图（?view=subscriptions&site_id=…），
// 而扩展里「打开订阅」正是跳 /reader?view=subscriptions —— 少了这一步，用户
// 点进来看到的是默认列表，且没有任何报错提示他参数被吃了。
func withQuery(target string, c *gin.Context) string {
	if raw := c.Request.URL.RawQuery; raw != "" {
		return target + "?" + raw
	}
	return target
}

func serveReader(c *gin.Context) { serveReaderFrom(c, readerAssets) }

// serveReaderFrom 是 serveReader 的可注入形态。**只收产物来源一个参数**——
// 「是否已构建」和「静态文件服务」都由它派生。
//
// 上一版收三个参数（assets / built / files），于是生产接线行
// `serveReaderFrom(c, readerAssets, readerBuilt, readerFileServer)` 成了一处
// 无人守的三元组：把 files 传成 http.NotFoundHandler()，全量测试照样绿，而生产
// 环境 /reader/assets/*.js 会全部 404、Reader 白屏。测试注入的假 FS 三者自洽，
// 生产传错了也测不出来——那次重构把可测性提上去的同时，把接线行的覆盖弄没了，
// 相对重构前是净退化（同一个变异在重构前是红的）。
//
// 处置不是「再加一条测试守那三个实参」，而是让不一致**表达不出来**：files 从
// assets 派生，built 也从 assets 派生，于是只剩一个参数可以传错，而它正是测试
// 注入的那一个。
func serveReaderFrom(c *gin.Context, assets fs.FS) {
	if !assetExists(assets, "index.html") {
		c.Data(http.StatusServiceUnavailable, "text/html; charset=utf-8", []byte(readerNotBuiltPage))
		return
	}

	rel := readerRelPath(c.Param("filepath"))
	if rel == "" || rel == "." || !assetExists(assets, rel) {
		// 带扩展名 = 资源请求，找不到就是找不到。
		if rel != "" && path.Ext(rel) != "" {
			c.Status(http.StatusNotFound)
			return
		}
		serveIndexFrom(c, assets)
		return
	}

	// Vite 产物里 assets/ 下的文件名带内容哈希，可以长期强缓存；其余文件
	// （favicon 等）保守处理，交给标准库的 Last-Modified 协商。
	if strings.HasPrefix(rel, "assets/") {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	}
	// Service Worker 脚本本身**绝不能被强缓存**：它是更新推送的唯一通道，
	// 缓存住它等于让用户永远收不到新版本，而修复本身也要靠它放行才能送达。
	// workbox 运行时同理（sw.js 的内容哈希变了才会拉新的 workbox-*.js）。
	if rel == "sw.js" || rel == "registerSW.js" || strings.HasPrefix(rel, "workbox-") {
		c.Header("Cache-Control", "no-cache")
	}
	readerFilesFor(assets).ServeHTTP(c.Writer, c.Request)
}

// readerFilesFor 由调用方给出的产物 FS 派生静态文件服务。这里刻意不缓存生产
// handler：一旦 package-level handler 和 assets 分开存在，生产接线就可能只更新
// 其中一个，而注入 FS 的测试仍全部通过。每请求构造两个小 handler 的代价可忽略，
// 换来的是文件服务与存在性判断必然读取同一份 FS。
func readerFilesFor(assets fs.FS) http.Handler {
	return http.StripPrefix("/reader/", http.FileServer(http.FS(assets)))
}

// readerRelPath 把 gin 的 *filepath 参数归一化成相对 assets/reader 的路径。
//
// 抽成纯函数是为了它可测：直接对 serveReader 断言「读不到 /etc/passwd」是个
// 永远成立的判据——readerAssets 是 embed.FS，物理上就不可能返回子树外的文件，
// 于是那种断言删掉 path.Clean 也照样绿。这里改成对**归一化行为**断言，
// 删掉 Clean 会当场变红。
func readerRelPath(raw string) string {
	return strings.TrimPrefix(path.Clean("/"+raw), "/")
}

// serveIndexFrom 写回 SPA 入口。index.html 必须禁缓存：它引用的资源文件名
// 每次构建都变，缓存住入口等于把用户钉死在旧版本上。
func serveIndexFrom(c *gin.Context, assets fs.FS) {
	data, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		c.String(http.StatusInternalServerError, "failed to load reader index")
		return
	}
	c.Header("Cache-Control", "no-cache")
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}

func assetExists(assets fs.FS, rel string) bool {
	f, err := assets.Open(rel)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return false
	}
	return true
}
