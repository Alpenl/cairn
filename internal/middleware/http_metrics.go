package middleware

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"webtag/internal/observability"
)

// HTTPMetrics 返回一个 Gin 中间件，统计每个 HTTP 请求的总数与耗时。
//
// Wave 2 M3：
//   - Counter 仍然按完整 status 码分桶（method/route/status），与既有
//     dashboard 兼容。
//   - Histogram 改用 status_class（method/route/status_class），把
//     基数从每路由 ×N 个 status 压到固定 4 个。运维真正关心的是
//     "5xx 多了"，不是 "503 vs 504"——前者上 Histogram，后者去 Counter
//     看 raw status。
//
// metrics 为 nil 时退化为无操作。
func HTTPMetrics(metrics *observability.Metrics) gin.HandlerFunc {
	if metrics == nil {
		return func(c *gin.Context) {
			c.Next()
		}
	}

	return func(c *gin.Context) {
		startedAt := time.Now()
		c.Next()

		// 用 FullPath() 而非 URL.Path 作为路由标签，避免每个动态 ID 都成为独立时间序列；
		// 未命中任何路由时统一归到 "unmatched" 以防止指标基数随 404 路径无限增长。
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		method := c.Request.Method
		statusCode := c.Writer.Status()
		status := strconv.Itoa(statusCode)

		metrics.HTTPRequestTotal.WithLabelValues(method, route, status).Inc()
		metrics.HTTPRequestDuration.WithLabelValues(method, route, statusClass(statusCode)).Observe(time.Since(startedAt).Seconds())
	}
}

// statusClass 把 HTTP 状态码归到 1xx/2xx/3xx/4xx/5xx 五个固定桶。
// 0 / 非法码归到 "0xx"，保证标签集闭合。
func statusClass(status int) string {
	if status < 100 || status >= 600 {
		return "0xx"
	}
	return fmt.Sprintf("%dxx", status/100)
}
