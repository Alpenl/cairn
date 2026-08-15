package middleware

import "time"

// IdempotencyHeader 是中间件读取的请求头名称（见
// IETF draft-ietf-httpapi-idempotency-key-header）。客户端在生成请求时
// 自己分配一个全局唯一的 key（一般是 UUID），网络抖动重试时复用同一个
// key —— 中间件命中已完成的缓存就直接回放上一次的响应，handler 不被再次调用。
const IdempotencyHeader = "Idempotency-Key"

// 默认参数。生产装配走 BuildRuntime / RouterOptions 显式注入；测试可直接
// 用这些零参数构造。
const (
	// DefaultIdempotencyTTL 是缓存项过期时间。24h 覆盖客户端常规的指数退避
	// 与跨端同步重试；这些流程几乎不会跨这个
	// 窗口；超过它再用同一个 key 大概率是脏数据，应当让 handler 真正再跑。
	DefaultIdempotencyTTL = 24 * time.Hour
)
