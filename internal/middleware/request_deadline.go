package middleware

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
)

// RequestDeadline 返回一个 Gin 中间件：基于 HTTP server 的 WriteTimeout
// 派生 per-request ctx deadline，让 handler 内部的下游调用（DB 查询、
// analyzer HTTP 出站、queue.Enqueue 等）能在 server 真正
// 超时关闭连接之前主动放弃，避免"server 端 goroutine 已悬空而 handler
// 仍在跑"。
//
// 参数：
//   - writeTimeout：HTTP server 的 WriteTimeout（来自 cfg.Server.WriteTimeoutMS）。
//     必须与 http.Server.WriteTimeout 保持一致，否则推导出的 deadline
//     与底层 server 的 socket 写超时不匹配，意义就消失了。
//   - percent：deadline 占 WriteTimeout 的比例，取值 (0, 1]。0.9 是默认
//     选择——给 handler 留 10% 缓冲（典型 WriteTimeout=15s 时是 1.5s）
//     处理 ctx.Done() 之后的清理逻辑（rollback、error response 落盘等），
//     这部分必须在 server 真正关 socket 前完成。
//
// 行为：
//   - writeTimeout <= 0 或 percent <= 0：中间件退化为 no-op，不附加 deadline。
//     适用于本地开发把 WriteTimeout 调成 0（无限）的调试场景。
//   - percent > 1：clamp 到 1（永远不会比 server 的 WriteTimeout 更宽松）。
//
// WHY 不在 handler 里逐个写 context.WithTimeout：业务 handler 数量多，
// 漏一个就是一个慢请求把 server goroutine 挂住的潜在事故。集中在中间件
// 里挂一次，所有 handler 自动受益；handler 仍可在内部用
// context.WithTimeout 进一步收紧（例如 DR 调用要 10s 子预算）。
func RequestDeadline(writeTimeout time.Duration, percent float64) gin.HandlerFunc {
	if writeTimeout <= 0 || percent <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	if percent > 1 {
		percent = 1
	}
	deadline := time.Duration(float64(writeTimeout) * percent)
	if deadline <= 0 {
		// 防御：浮点截断到 0 时退回 no-op，避免 WithTimeout(ctx, 0) 立即
		// 触发 DeadlineExceeded 把所有请求一刀切死。
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), deadline)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
