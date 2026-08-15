package app

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
)

// newReaderRouter 只挂 Reader 相关路由，避免把整台 NewRouter 的依赖拖进来。
func newReaderRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	registerReaderRoutes(r)
	return r
}

// readerFixture 是一份最小的假产物：一个入口页 + 一个带内容哈希的资源。
func readerFixture() fstest.MapFS {
	return fstest.MapFS{
		"index.html":         &fstest.MapFile{Data: []byte(`<div id="root"></div>`)},
		"assets/app-abc.js":  &fstest.MapFile{Data: []byte("console.log(1)")},
		"sw.js":              &fstest.MapFile{Data: []byte("self.addEventListener('install',()=>{})")},
		"workbox-abc1234.js": &fstest.MapFile{Data: []byte("// workbox runtime")},
	}
}

// 精确使用全局 readerAssets 的用例不能省：旧实现只对这个 FS 走 package-level
// handler，MapFS 测试全部绕开了那条生产分支。两种来源都从实参派生文件服务，
// 才能保证存在性判断命中的资源随后不会被另一份 handler 回成 404。
func TestReaderFilesForServesProductionAndInjectedFS(t *testing.T) {
	t.Parallel()

	embeddedPlaceholder, err := fs.ReadFile(readerAssets, ".gitkeep")
	if err != nil {
		t.Fatalf("读取嵌入 Reader 占位文件: %v", err)
	}

	for _, tc := range []struct {
		name   string
		assets fs.FS
		path   string
		want   string
	}{
		{
			name:   "production readerAssets",
			assets: readerAssets,
			path:   "/reader/.gitkeep",
			want:   string(embeddedPlaceholder),
		},
		{
			name:   "injected MapFS",
			assets: readerFixture(),
			path:   "/reader/assets/app-abc.js",
			want:   "console.log(1)",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)

			readerFilesFor(tc.assets).ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200", tc.path, w.Code)
			}
			if got := w.Body.String(); got != tc.want {
				t.Fatalf("GET %s body = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// doReader 用注入的产物 FS 起一个只挂 Reader 的 router 并发一次 GET。
func doReader(t *testing.T, assets fstest.MapFS, target string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/reader/*filepath", func(c *gin.Context) { serveReaderFrom(c, assets) })
	return do(t, r, http.MethodGet, target)
}

func do(t *testing.T, r *gin.Engine, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

// 站点根必须把人送到 Reader。这条曾经指向一页手写调试 UI——那是唯一随二进制
// 发布的界面，而真正的产品前端从未被 serve 过。
func TestRootRedirectsToReader(t *testing.T) {
	w := do(t, newReaderRouter(), http.MethodGet, "/")
	if w.Code != http.StatusFound {
		t.Fatalf("GET / status = %d, want %d", w.Code, http.StatusFound)
	}
	if got := w.Header().Get("Location"); got != "/reader/" {
		t.Fatalf("GET / Location = %q, want %q", got, "/reader/")
	}
}

// 少一个尾斜杠，index.html 里的相对资源就会解析到上一层目录。补斜杠是硬要求。
func TestReaderWithoutTrailingSlashRedirects(t *testing.T) {
	w := do(t, newReaderRouter(), http.MethodGet, "/reader")
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /reader status = %d, want %d", w.Code, http.StatusMovedPermanently)
	}
	if got := w.Header().Get("Location"); got != "/reader/" {
		t.Fatalf("GET /reader Location = %q, want %q", got, "/reader/")
	}
}

// 重定向必须带上 query。扩展的「打开订阅」跳的就是 /reader?view=subscriptions，
// 丢掉参数会让用户落到默认列表，而且没有任何迹象说明参数被吃了。
func TestReaderRedirectPreservesQuery(t *testing.T) {
	r := newReaderRouter()

	for _, tc := range []struct{ from, want string }{
		{"/reader?view=subscriptions", "/reader/?view=subscriptions"},
		{"/?view=sites&site_id=abc", "/reader/?view=sites&site_id=abc"},
		{"/reader", "/reader/"},
		{"/", "/reader/"},
	} {
		w := do(t, r, http.MethodGet, tc.from)
		if got := w.Header().Get("Location"); got != tc.want {
			t.Errorf("GET %s → Location %q, want %q", tc.from, got, tc.want)
		}
	}
}

// 产物在与不在，是两条不同的分支，两条都要在门禁上真的跑。
//
// 这条以前写成「按当前形态该是什么」：`if !readerBuilt() { …; return }`。
// 而产物不进版本控制、CI 也不跑 make reader-build——门禁上永远走 return 那一半，
// 另一半（含 Cache-Control: no-cache 断言）从不执行。它不是 t.Skip，所以
// make test-no-skip 也抓不到：条件 return 是那条门禁结构上的盲区。
//
// 改用 MapFS 注入两种形态，两条分支都真的跑。
func TestReaderIndexReflectsBuildState(t *testing.T) {
	t.Parallel()

	t.Run("产物缺席返回可照做的说明页", func(t *testing.T) {
		t.Parallel()
		w := doReader(t, fstest.MapFS{}, "/reader/")

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
		}
		if !strings.Contains(w.Body.String(), "Reader 尚未构建") {
			t.Fatalf("应返回说明页，实际 body = %q", truncate(w.Body.String()))
		}
		// 说明页必须给出可照做的构建命令，否则运维只知道坏了不知道怎么修。
		if !strings.Contains(w.Body.String(), "make build-full") {
			t.Error("说明页未给出 make build-full 指引")
		}
	})

	t.Run("产物就位返回入口页且禁缓存", func(t *testing.T) {
		t.Parallel()
		w := doReader(t, readerFixture(), "/reader/")

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("Content-Type = %q, want text/html", ct)
		}
		// 入口页缓存住 = 用户被钉在旧版本上，因为它引用的资源名每次构建都变。
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("Cache-Control = %q, want no-cache", cc)
		}
	})

	// assets/ 下的文件名带内容哈希，可以长期强缓存——少了这条，每次导航都要
	// 重新拉一遍 React 运行时与 markdown 栈（约 470 KB）。
	t.Run("哈希资源可长期强缓存", func(t *testing.T) {
		t.Parallel()
		w := doReader(t, readerFixture(), "/reader/assets/app-abc.js")

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Fatalf("Cache-Control = %q, want 含 immutable", cc)
		}
	})

	// Service Worker 脚本是更新推送的**唯一**通道。强缓存住它等于让用户永远
	// 收不到新版本——而修复本身也要靠它放行才能送达，属于远程修不回来的那类
	// 故障。workbox 运行时同理。
	t.Run("Service Worker 脚本禁止强缓存", func(t *testing.T) {
		t.Parallel()
		for _, path := range []string{"/reader/sw.js", "/reader/workbox-abc1234.js"} {
			w := doReader(t, readerFixture(), path)
			if w.Code != http.StatusOK {
				t.Fatalf("%s status = %d, want 200", path, w.Code)
			}
			cc := w.Header().Get("Cache-Control")
			if cc != "no-cache" {
				t.Fatalf("%s Cache-Control = %q, want no-cache", path, cc)
			}
			if strings.Contains(cc, "immutable") {
				t.Fatalf("%s 被当成哈希资源强缓存了", path)
			}
		}
	})
}

// 路径归一化：`..` 段必须在查文件之前就被吃掉。
//
// 这条以前是断言「响应体里不含 root:」——一个永远成立的判据：readerAssets 是
// embed.FS，物理上就不可能返回子树外的文件，所以删掉 path.Clean 它照样绿
// （实测过）。注释却写着"这条锁住它不被简化掉"，是它做不到的事。
// 改成对归一化行为本身断言，现在删掉 Clean 会当场变红。
func TestReaderRelPathNormalizes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ raw, want string }{
		{"/../../etc/passwd", "etc/passwd"},
		{"/assets/../../../etc/shadow", "etc/shadow"},
		{"/./index.html", "index.html"},
		{"/assets/app.js", "assets/app.js"},
		{"/", ""},
		{"", ""},
	} {
		if got := readerRelPath(tc.raw); got != tc.want {
			t.Errorf("readerRelPath(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// serveReader 必须真的**用**归一化结果，而不是各算各的。
//
// 用 fstest.MapFS 造一个只含 index.html 的假产物，**不依赖 make reader-build**。
// 上一版这条测试写在真实 serveReader 上、开头 `if !readerBuilt() { t.Skip }`
// ——而产物不进版本控制、CI 的 test job 也不跑 make reader-build，于是它在闸门上
// 一次都没执行过。一条从未在闸门上跑过的回归防线，与不存在等价。
func TestServeReaderUsesNormalizedPath(t *testing.T) {
	t.Parallel()

	assets := readerFixture()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/reader/*filepath", func(c *gin.Context) { serveReaderFrom(c, assets) })

	// /reader/assets/../index.html：
	//   归一化后 = "index.html"（存在）→ 交给文件服务，它自己再规范化一次 URL
	//              并回 301 到 /reader/index.html
	//   不归一化 = 字面量 "assets/../index.html"（假产物里没有这个名字）且带
	//              扩展名 → 直接 404
	w := do(t, r, http.MethodGet, "/reader/assets/../index.html")
	if w.Code == http.StatusNotFound {
		t.Fatalf("status = 404——serveReader 没有使用归一化后的路径做存在性判断")
	}
	if w.Code != http.StatusOK && w.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 200 或 301", w.Code)
	}
}

// 产物缺席时返回说明页而不是 404——同样不依赖真实产物即可验证。
func TestServeReaderNotBuiltPage(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/reader/*filepath", func(c *gin.Context) { serveReaderFrom(c, fstest.MapFS{}) })

	w := do(t, r, http.MethodGet, "/reader/")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "make build-full") {
		t.Error("说明页未给出可照做的构建指引")
	}
}

// SPA 深链回落 index.html；带扩展名的缺失资源真 404。
//
// 这两条行为原本各有一个走真实产物的用例，开头是 `if !readerBuilt() { t.Skip }`
// ——产物不进版本控制、CI 也不跑 make reader-build，它们在闸门上从不执行。
// 已删除：一条恒 skip 的测试不是通过的测试，留着只会被记成「已覆盖」。
func TestServeReaderFallbackRules(t *testing.T) {
	t.Parallel()

	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<div id=\"root\"></div>")}}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/reader/*filepath", func(c *gin.Context) { serveReaderFrom(c, assets) })

	if w := do(t, r, http.MethodGet, "/reader/some/deep/view"); w.Code != http.StatusOK {
		t.Errorf("深链 status = %d, want 200（SPA 刷新必须能起来）", w.Code)
	}
	if w := do(t, r, http.MethodGet, "/reader/assets/missing.js"); w.Code != http.StatusNotFound {
		t.Errorf("缺失资源 status = %d, want 404（回落 HTML 会掩盖构建问题）", w.Code)
	}
}

// 归一化之后的路径一律在子树内查找，穿越请求最终打不出去。
func TestReaderRejectsPathTraversal(t *testing.T) {
	w := do(t, newReaderRouter(), http.MethodGet, "/reader/../../etc/passwd")
	if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "root:") {
		t.Fatal("路径穿越读到了子树外的文件")
	}
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
