// Package config parses environment variables into the runtime
// configuration. Wave 12.4 H2 grouped the previously flat Config
// struct (39 fields) into 6 sub-structs so deps.go can pass each
// sub-config to the right constructor instead of unpacking field by
// field, and so adding a new knob lands in one obvious place.
package config

// ServerConfig groups the HTTP-server-level knobs: where to listen,
// what CORS origins to accept, and the per-request timeout budget.
//
// ShutdownTimeoutMS 是优雅停机的总预算（默认 30s）。Server.Shutdown
// 使用同一个 deadline 依次排空 HTTP 请求并执行 Lifecycle.Close；前一段
// 消耗越久，后台 worker 与持久化资源关闭可用的剩余预算越少。值应低于
// k8s terminationGracePeriodSeconds，给容器自身退出开销留出余量。
type ServerConfig struct {
	ListenAddr          string
	CORSOrigins         []string
	TrustedProxyCIDRs   []string
	ReadHeaderTimeoutMS int
	ReadTimeoutMS       int
	WriteTimeoutMS      int
	IdleTimeoutMS       int
	ShutdownTimeoutMS   int
}

// DBConfig groups the PostgreSQL pool sizing and connection lifecycle knobs
// with the parse-worker concurrency that consumes pooled connections.
//
// Lifecycle 三个字段（MaxConnLifetimeMS / MaxConnIdleTimeMS /
// HealthCheckPeriodMS）<=0 时保留 pgx 默认（1h / 30min / 1min）。生产
// 推荐 30min / 10min / 1min，应对 PgBouncer 或 RDS Proxy 中间层强制
// reset 空闲连接导致的偶发 "connection reset by peer"。
type DBConfig struct {
	MaxConns            int
	MinConns            int
	ParseConcurrency    int
	MaxConnLifetimeMS   int
	MaxConnIdleTimeMS   int
	HealthCheckPeriodMS int
}

// FetcherConfig groups retry knobs for the outbound HTTP fetch path.
type FetcherConfig struct {
	RetryAttempts int
	RetryDelayMS  int
	// URLDirect 启用混合 Grok 路由：普通网页优先本地完整抓取，X/Twitter
	// 优先由联网分析端点自抓 URL，本地抓取失败时也用 URL 直连兜底。
	// PARSE_MODE=fetcher 会关闭所有 URL 直连分析。
	URLDirect bool
}

// AnalyzerConfig groups the OpenAI-compatible AI client knobs. Every provider
// request uses the same SSRF-safe transport. Structured output automatically
// falls back when a provider rejects the response format.
type AnalyzerConfig struct {
	BaseURL          string
	APIKey           string
	Model            string
	RetryAttempts    int
	RetryDelayMS     int
	RequestTimeoutMS int
}

// Config is the full parsed configuration. Most fields are grouped
// under sub-configs by domain (Server / DB / Fetcher / Analyzer /
// runtime features). The handful of remaining top-level
// fields are either core (DatabaseURL — connection string for the
// pool) or single-field knobs (LogLevel and GitHubToken)
// that did not earn their own sub-struct.
type Config struct {
	Server   ServerConfig
	DB       DBConfig
	Fetcher  FetcherConfig
	Analyzer AnalyzerConfig

	DatabaseURL string

	LogLevel string
	// LogFormat 控制日志 handler：json（默认，生产）或 text（本地开发）。
	// 解析见 observability.ParseLogFormat —— 未知值回退到 json，保证
	// 日志输出永远是结构化的，便于聚合系统索引。
	LogFormat   string
	GitHubToken string

	// AppEnv 是运行环境标记（dev / staging / prod），默认 "prod"。
	// dev 允许从本地配置派生 Reader 游标签名密钥；该值也写入追踪资源。
	AppEnv string

	// ExtensionAPIToken, when non-empty, gates the public API surface
	// (/api/ingest, /api/links*, /api/tags, /api/tree) behind a
	// `Authorization: Bearer <token>`
	// check so the WebTag browser extension can authenticate.
	//
	// 它是单实例的静态总钥匙，也是必填配置。所有外部 API 始终要求该
	// Bearer 凭证或由它换取的浏览器会话。
	ExtensionAPIToken string

	// SessionSigningKey（env SESSION_SIGNING_KEY）是 Reader 浏览器会话 cookie 的
	// HMAC-SHA256 签名密钥。
	//
	// Reader 用静态安装令牌换一张有期限的 httpOnly cookie，页面脚本读不到
	// 凭证本身，也就外带不走。Reader 会渲染来自第三方网页的标题、摘要和正文，
	// 这条防线针对的正是那个面。为空时进程会生成一次性密钥，重启后所有会话
	// 失效；显式配置后会话可跨重启存活。
	//
	// 换密钥会让所有在飞会话立即失效（等价于强制全体重新登录），这是撤回
	// 无状态会话的唯一手段，也是它的设计代价。
	SessionSigningKey string

	// SessionSigningKeyEphemeral 记录上面那把密钥是不是进程启动时随机生成的
	// 应急密钥（即 SESSION_SIGNING_KEY 留空）。留空是个静默陷阱：会话签名密钥
	// 随进程消失，每次重启（含每次自动更新）都会让所有 Reader 会话当场失效，
	// 而 session 模式下浏览器里不留安装令牌，用户只能重新填一次 key。启动时
	// 据此打 WARN，把这条因果关系摆到日志里，而不是留给几周后的困惑。
	SessionSigningKeyEphemeral bool

	// RiverTerminalRetentionMS applies to cancelled/completed/discarded rows.
	// -1 keeps rows indefinitely for rollback; a positive value lets River
	// clean terminal execution history after that duration.
	RiverTerminalRetentionMS int

	// CursorSigningKey 是所有公开分页游标共用的 HMAC-SHA256 签名密钥。
	// 非开发环境必须显式配置；开发环境可派生稳定本地值。
	CursorSigningKey string

	// IdempotencyTTLMS 是缓存项过期时间（毫秒）。<=0 → 走
	// middleware.DefaultIdempotencyTTL（24h）。env IDEMPOTENCY_TTL_MS。
	IdempotencyTTLMS int
}
