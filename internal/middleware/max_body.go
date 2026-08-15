package middleware

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// DefaultMaxRequestBodyBytes 是 MaxRequestBody 的默认上限：4 MiB。
// 选择 4 MiB 的原因：
//   - 这是项目当前合法路径里**最大**的 body 上限（/api/ingest，浏览器
//     插件 push 整页 HTML/text snapshot 用，常规 < 200 KiB，95p ~1 MiB，
//     极端 4 MiB 见 handler/links.go:88 ingestMaxJSONBodyBytes）。
//   - 全局中间件必须 ≥ 任何合法路径的需求，否则会把它打成 413（参见
//     Wave 5A reviewer 找出的回归：曾经用 1 MiB 默认值导致 ingest 错误回归）。
//   - 单 handler 想更严格（如 /api/links 默认 1 MiB）
//     可以在 handler 内继续包一层 MaxBytesReader——嵌套时 Go stdlib 行为是
//     **取最严格上限**（内外两层 MaxBytesReader 嵌套，谁的 N 小谁先 trip），
//     所以严格 handler 自管的限制不会被全局抬高的默认值绕过。
//   - 4 MiB 仍能挡住"试探性大 body DoS"（10 GB body 耗内存）。
//
// 修改全局默认前请先确认：是否有 handler 的合法 body 超过新默认值。
const DefaultMaxRequestBodyBytes int64 = 4 << 20

// MaxRequestBody 返回一个 Gin 中间件，给 c.Request.Body 包一层
// http.MaxBytesReader。读到第 maxBytes+1 个字节时立即返回
// *http.MaxBytesError；handler 调用 c.ShouldBindJSON / io.ReadAll 等会
// 透过 errors.As 拿到该 sentinel，并由统一的 JSON 错误中间件转成 413。
//
// 行为：
//   - maxBytes <= 0：中间件退化为 no-op，便于关闭限制（不推荐用于生产）。
//   - 与 handler-级 MaxBytesReader 嵌套：**取最严格上限**——内外两层
//     MaxBytesReader 嵌套时，谁的 N 小谁先 trip。所以全局默认应当
//     设成「所有合法路径里最大值的上限」（参见 DefaultMaxRequestBodyBytes
//     说明），让严格的 handler 自管的小限制仍能在内层先 trip，宽的
//     handler 不会被全局误杀。这与早期文档声称的"取最后一次设置"
//     语义相反，请按本注释为准。
//   - 错误响应不在本中间件触发：handler 读取时才会触发 MaxBytesError，
//     由各 handler 的 bindJSONWithLimit / 显式 errors.As 转成 413。
//
// WHY 现在加：之前只有 /api/links 显式
// 限制过 body。其他 POST/PUT/PATCH 路由（包括 admin 的 approve/reject）
// 默认是无限大——攻击者打一个 10 GB body 到 /api/admin/concept-merges/:id/reject
// 就能在 io.ReadAll 内吃光 server 内存。集中在中间件挂一次，所有路由
// 自动有默认上限。
func MaxRequestBody(maxBytes int64) gin.HandlerFunc {
	if maxBytes <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		if c.Request != nil && c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}
		c.Next()
	}
}

// IsMaxBytesError 是给上层 handler 统一识别 MaxBytesReader 触发的便利
// 函数：errors.As 适配 *http.MaxBytesError 这个具体类型。保留在
// middleware 包内是因为本包同时持有触发它的中间件与已有的
// JSONError 写出函数。
func IsMaxBytesError(err error) bool {
	if err == nil {
		return false
	}
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}
