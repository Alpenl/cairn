package app

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"webtag/internal/middleware"
	"webtag/internal/observability"
)

func installRouterMiddleware(router *gin.Engine, logger *slog.Logger, metrics *observability.Metrics, opts RouterOptions, extraMiddleware ...gin.HandlerFunc) {
	// 安全响应头放在 RequestID 之后、CORS 之前：RequestID 写的是
	// 响应头同样的位置（不会冲突），CORS 也会写 Vary/Access-Control-*
	// 头，二者互不覆盖；SecurityHeaders 早于业务 handler 跑，确保
	// 即使下游写了短路返回，安全头也已经落到 Writer 上。
	router.Use(
		middleware.RequestID(logger),
		middleware.SecurityHeaders(),
	)
	// Gzip 必须夹在 SecurityHeaders 与 Recovery 之间，两侧都是硬约束：
	//   - 排在 Recovery **之前**（Recovery 是内层）：panic 展开时内层 defer
	//     先跑，Recovery 得以把 JSON 500 写进尚未冲出的缓冲区，再由 Gzip
	//     统一收尾。顺序反过来会先冲缓冲区提交响应头，Recovery 随后写的
	//     500 就拼在了已提交的 body 后面。
	//   - 排在 CORS（extraMiddleware）**之前**：Gzip 的 Vary 是在「第一个
	//     字节即将出去」那一刻才追加的，而那一刻 CORS 的 before-Next 段
	//     早已跑完，两个 Vary 值因此能叠加而不是互相覆盖。详见
	//     middleware/gzip.go 顶部的顺序约束说明。
	if opts.GzipEnabled {
		router.Use(middleware.Gzip(opts.GzipMinLength))
	}
	router.Use(
		middleware.AccessLog(logger),
		middleware.HTTPMetrics(metrics),
		middleware.Recovery(logger),
	)
	// MaxRequestBody / RequestDeadline 都挂在 SecurityHeaders 之后、
	// CORS 与 RateLimit（来自 extraMiddleware）之前。理由：
	//   - 早一点截断 body：CORS preflight 完了之后 RateLimit 才看到请求，
	//     但攻击 body 在 RateLimit 通过那一刻已经在缓冲；MaxBytesReader
	//     越早包装，io.ReadAll/ShouldBindJSON 越早触发 413。
	//   - 早一点设置 deadline：让 RateLimit / CORS 自身的内部分支也共享
	//     ctx，handler 内部的 DB / 出站调用都拿到同一份 deadline。
	if opts.MaxRequestBodyBytes > 0 {
		router.Use(middleware.MaxRequestBody(opts.MaxRequestBodyBytes))
	}
	if opts.RequestDeadlineTimeout > 0 {
		percent := opts.RequestDeadlinePercent
		if percent <= 0 {
			percent = 0.9
		}
		router.Use(middleware.RequestDeadline(opts.RequestDeadlineTimeout, percent))
	}
	if len(extraMiddleware) > 0 {
		router.Use(extraMiddleware...)
	}
}
