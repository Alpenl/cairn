// Package config parses environment variables into the runtime
// configuration. Wave 12.4 H2 grouped the previously flat Config
// struct (39 fields) into 6 sub-structs so deps.go can pass each
// sub-config to the right constructor instead of unpacking field by
// field, and so adding a new knob lands in one obvious place.
package config

import "webtag/internal/model"

// ServerConfig groups the HTTP-server-level knobs: where to listen,
// what CORS origins to accept, and the per-request timeout budget.
//
// ShutdownTimeoutMS 是优雅停机的总预算（默认 30s）。Server.Shutdown
// 把它拆成两段：阶段 1（70%）httpServer.Shutdown 拒收新请求 + 等
// in-flight 请求自然结束；阶段 2（30%）Lifecycle.Close 排空 worker
// queue、关闭 DB 连接池、flush tracer。值应与 k8s
// terminationGracePeriodSeconds 对齐（一般留 10%~20% 的余量给容器
// 自身的退出开销）。
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
// AllowUnsafeTargets lives on AnalyzerConfig because only the analyzer
// path (which fans out to test-helper localhost servers in dev) flips
// it on; the fetcher always honours the SSRF allow-list.
type FetcherConfig struct {
	RetryAttempts int
	RetryDelayMS  int
	// PreferLight makes the parse pipeline default to the truncated
	// "tag-only" fetch path (Range-limited HTTP + first ~1000 chars of
	// body) instead of the full Fetch chain. Saves ~7x LLM input
	// tokens and ~3x readability CPU per link when downstream consumers
	// only care about tags / titles. Per-link
	// SourceMetadata["parse_depth"] overrides this either way at submit
	// time, so a "deep mode" link in an otherwise light-mode deployment
	// still gets the full body. Off by default — flip to true once you
	// have validated tag precision on your own corpus.
	PreferLight bool
	// URLDirect 启用混合 Grok 路由：普通网页优先本地完整抓取，X/Twitter
	// 优先由联网分析端点自抓 URL，本地抓取失败时也用 URL 直连兜底。
	// PARSE_MODE=fetcher 会关闭所有 URL 直连分析。
	URLDirect bool
}

// AnalyzerConfig groups the OpenAI-compatible AI client knobs.
// AllowUnsafeTargets gates the SSRF dialer at the analyzer transport
// only; the regular fetcher transport never honours it. RequestTimeoutMS
// caps a single chat-completion call (per-attempt budget); the retry
// settings sit between attempts.
// DisableStructuredOutput turns off the strict json_schema response_format
// the analyzer sends on the plain-analysis path. Default false: the analyzer
// already demotes itself when a gateway answers 400/422 to the block, so this
// env only exists for a gateway that accepts response_format and then
// mishandles it (returns 200 with an unusable body), which auto-demotion
// cannot detect.
type AnalyzerConfig struct {
	BaseURL                 string
	APIKey                  string
	Model                   string
	AllowUnsafeTargets      bool
	RetryAttempts           int
	RetryDelayMS            int
	RequestTimeoutMS        int
	DisableStructuredOutput bool
}

// EmbeddingConfig groups the OpenAI-compatible embedding client knobs used by
// parse-time link vectors and semantic link search. Model=="" disables that
// vector leg and query search falls back to ILIKE. BaseURL / APIKey inherit the
// analyzer's AI_BASE_URL / AI_API_KEY at load time so the common
// single-provider deployment needs zero extra config; both transports
// honour the same SSRF allow-list via AllowUnsafeTargets (inherited from
// AnalyzerConfig). Dimensions is the embedding vector width the upstream
// model emits and the pgvector columns are sized to.
type EmbeddingConfig struct {
	Model              string
	BaseURL            string
	APIKey             string
	Dimensions         int
	AllowUnsafeTargets bool
	RetryAttempts      int
	RetryDelayMS       int
	RequestTimeoutMS   int
}

// RateLimitConfig groups the per-IP token bucket knobs installed by
// middleware.RateLimit. RPS=0 disables the limiter entirely (default).
// Burst=0 with RPS>0 lets the middleware derive a sensible default
// burst from the rate.
type RateLimitConfig struct {
	RPS   float64
	Burst int
}

// Config is the full parsed configuration. Most fields are grouped
// under sub-configs by domain (Server / DB / Fetcher / Analyzer /
// RateLimit). The handful of remaining top-level
// fields are either core (DatabaseURL — connection string for the
// pool) or single-field knobs (LogLevel, GitHubToken,
// MetricsAuthToken, TagCacheTTLMS, YtdlpBinaryPath, YtdlpTimeoutMS)
// that did not earn their own sub-struct.
type Config struct {
	Server    ServerConfig
	DB        DBConfig
	Fetcher   FetcherConfig
	Analyzer  AnalyzerConfig
	Embedding EmbeddingConfig
	RateLimit RateLimitConfig

	DatabaseURL string

	LogLevel string
	// LogFormat 控制日志 handler：json（默认，生产）或 text（本地开发）。
	// 解析见 observability.ParseLogFormat —— 未知值回退到 json，保证
	// 日志输出永远是结构化的，便于聚合系统索引。
	LogFormat     string
	TagCacheTTLMS int
	// TreeCacheTTLMS 是域名摘要（/api/tree?view=domains）缓存的 TTL。
	// 该端点此前完全无缓存，每次调用跑一遍全表 `GROUP BY domain`，而 Reader
	// 每次进主界面与每次点同步都会请求一次。与 TAG_CACHE_TTL_MS 同默认值。
	TreeCacheTTLMS  int
	GitHubToken     string
	YtdlpBinaryPath string
	YtdlpTimeoutMS  int

	// GzipEnabled 控制响应压缩中间件的挂载（env GZIP_ENABLED，默认 true）。
	//
	// 默认开是因为不开的代价是持续的：API 响应此前完全裸奔，而列表项 DTO
	// 携带 summary / description / classification_explanation 等长文本字段，
	// 一页 30 条几十 KB，且 Reader 每 30 秒静默刷新一次第 1 页。反代层
	// （Caddy 的 encode）只覆盖有反代的部署；直连 Go 二进制的自托管用户
	// 拿不到任何压缩，因此这一层必须在应用内。
	GzipEnabled bool

	// GzipMinLength 是启用压缩的响应体字节下限（env GZIP_MIN_LENGTH，
	// 默认 1024）。低于此值不压缩：小响应压完往往更大（gzip 头尾固定开销
	// 约 20 字节，短 JSON 熵又高），白白搭上 CPU 和一次 Vary 协商。
	GzipMinLength int

	// MetricsAuthToken, when non-empty, gates the /metrics endpoint
	// behind a `Authorization: Bearer <token>` check. Leaving it unset
	// preserves the single-host default of an open scrape endpoint;
	// any deployment exposing /metrics on a network-reachable address
	// SHOULD configure a token rather than relying on network policy
	// alone.
	MetricsAuthToken string

	// AdminAuthToken 是 /api/admin/* 路由的 Bearer 鉴权 token。
	// 与 MetricsAuthToken 不同，admin 接口拥有写入权（approve/reject
	// 概念合并提案）且不应当对外裸奔，因此采取 fail-closed 策略：
	//   - AppEnv != "dev" 且 AdminAuthToken 为空 → 启动 Warn，且 admin
	//     路由 100% 返回 401（保留启动以避免破坏现有 prod 部署，但
	//     谁也无法误操作 admin 接口）。
	//   - AppEnv == "dev" 且 AdminAuthToken 为空 → 路由开放，方便本地
	//     调试 admin/concept-merges UI。
	AdminAuthToken string

	// AppEnv 是运行环境标记（dev / staging / prod），默认 "prod"。
	// 仅用于把若干"开发环境豁免"开关（当前是 AdminAuthToken 空允许）
	// 与生产环境隔离。任何"危险默认值"都应当只对 dev 放行，prod 必须
	// 显式配置。
	AppEnv string

	// ExtensionAPIToken, when non-empty, gates the public API surface
	// (/api/ingest, /api/links*, /api/jobs*, /api/tags, /api/tree and
	// their /api/v1/* aliases) behind a `Authorization: Bearer <token>`
	// check so the WebTag browser extension can authenticate.
	//
	// 它是单实例的静态总钥匙。留空**不**等于开放：公开 API 默认
	// fail-closed（见 PublicAPIOpen），不会在数据库里生成第二套动态凭证。
	// 无鉴权开放需要显式设 PUBLIC_API_OPEN=true，那种情况下启动会打 WARN；
	// 若 CORS_ORIGINS 同时放行了非 localhost 来源，措辞会进一步加重。
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

	// TranslationJobsRollout coordinates versioned River worker registration
	// and translation API scheduling during the RF6A rolling cutover.
	TranslationJobsRollout model.TranslationJobsRolloutStage

	// TranslationSourceRollout controls whether RF5A translation scheduling
	// accepts legacy requests without a verified saved-content revision or
	// summary source hash. Fresh deployments default to strict. Existing
	// deployments must explicitly select compat for the bounded client rollout
	// before the translation-source contract migration.
	TranslationSourceRollout TranslationSourceRolloutStage

	// Translation reconcile knobs bound RF6B's terminal-state repair loop.
	// MissingAfterMS must remain greater than RoundTimeoutMS so one timed-out
	// reconciliation round cannot make an otherwise live job look missing.
	TranslationReconcileIntervalMS     int
	TranslationReconcileBatch          int
	TranslationReconcileRoundTimeoutMS int
	TranslationReconcileMissingAfterMS int

	// Parse reconcile knobs extend the existing terminal-history repair loop
	// with RF6C missing-job recovery. RiverTerminalRetentionMS is shared by
	// cancelled/completed/discarded rows. A finite value must exceed the
	// missing threshold, the declared recovery downtime, and one full bounded
	// reconciler cycle; -1 is reserved for rollback to infinite retention.
	ParseReconcileIntervalMS     int
	ParseReconcileBatch          int
	ParseReconcileRoundTimeoutMS int
	ParseReconcileMissingAfterMS int
	RiverTerminalRetentionMS     int
	RiverMaxRecoveryDowntimeMS   int

	// PublicAPIOpen（env PUBLIC_API_OPEN，默认 false）显式允许零凭证访问。
	//
	// 默认 fail-closed；确实需要无鉴权开放（纯内网、前面有反代鉴权）时
	// 才显式设 PUBLIC_API_OPEN=true。
	PublicAPIOpen bool

	// PProfEnabled 控制是否挂载 /debug/pprof/* 路由。默认 false：
	// 即使设置了 MetricsAuthToken，pprof 也保持关闭，因为 heap/goroutine
	// dump 包含进程内存里的字面量（含 token / 上游服务凭证残片）。
	// 启用时还必须同时配置 MetricsAuthToken —— 否则 router 启动时打
	// Warn 并继续以关闭状态运行，避免裸 pprof 暴露 SSRF/远程读取攻击面。
	PProfEnabled bool

	// CursorSigningKey 是显式配置的列表游标 HMAC-SHA256 签名密钥。留空时
	// Link 列表保留历史明文游标格式，避免破坏仍持有旧游标的客户端。
	CursorSigningKey string
	// ReaderCursorSigningKey 是 Reader activity 与 Thought 搜索使用的有效密钥。
	// Load 优先读取 READER_CURSOR_SIGNING_KEY，其次回退到显式的
	// CURSOR_SIGNING_KEY。非 dev 环境必须显式配置；仅 dev 允许从本地部署凭据
	// 做域隔离派生。该字段不改变 Link 列表的明文兼容策略。
	ReaderCursorSigningKey string

	// IdempotencyEnabled 控制是否挂载 middleware.Idempotency。第二轮工程
	// 审计 API HIGH A1：所有 POST/PUT/PATCH/DELETE 没有 Idempotency-Key
	// 语义，网络抖动重试可能产生重复资源。中间件仅在请求头带
	// Idempotency-Key 时介入；无该头则透传，向后兼容。默认 true：装一
	// 层缓存挡客户端毫秒级重试 race。
	//
	// 注意（多 pod 限制）：缓存是进程内 map，不跨 pod 共享。同一 key 命
	// 中 pod A 不会让 pod B 知道，pod B 仍会调一次 handler。中间件不替
	// 代后端 dedupe（DB 唯一约束 + advisory lock 才是真正的防线），
	// 但能挡掉单 pod 内"客户端重试 → 重复 enqueue"这一类毫秒级 race。
	IdempotencyEnabled bool

	// IdempotencyTTLMS 是缓存项过期时间（毫秒）。<=0 → 走
	// middleware.DefaultIdempotencyTTL（24h）。env IDEMPOTENCY_TTL_MS。
	IdempotencyTTLMS int

	// SiteMigrationEnabled controls the historical collection migration worker.
	// It defaults to false because applying historical moves requires an
	// operator-reviewed dry-run before any library data changes.
	SiteMigrationEnabled    bool
	SiteMigrationDryRun     bool
	SiteMigrationBatch      int
	SiteMigrationIntervalMS int

	// Website collection rollout controls are deliberately independent. They
	// never remove the additive schema or make existing sites unreadable; each
	// only controls the corresponding mutation/automation surface.
	LibraryKindAPIEnabled         bool
	SiteLibraryWriteEnabled       bool
	SiteAutoClassificationEnabled bool
	SiteAdvancedManagementEnabled bool

	// OTELEndpoint 是 OpenTelemetry OTLP gRPC 端点（host:port，如
	// "otel-collector:4317"）。env OTEL_EXPORTER_OTLP_ENDPOINT。
	//   - 空（默认）→ 安装 noop tracer，不产生任何网络出口。
	//   - 非空 → 第二轮工程审计 H4 真实接入：BatchSpanProcessor + OTLP gRPC
	//     exporter + ParentBased(TraceIDRatioBased) 采样器；Gin / outbound HTTP
	//     / pgx 全链路都会记录 span。详见 observability.InitTracer。
	OTELEndpoint string

	// OTELSamplingRatio 是根采样比例（ParentBased + TraceIDRatioBased 的
	// ratio 参数），默认 0.05（5%）。env OTEL_SAMPLING_RATIO。
	//   - 0 → 几乎不采样（极端情况下首个 span 仍会被 ParentBased 上游决定）。
	//   - 1 → 全采样，本地调试 / 验证链路完整性时用。
	//   - prod 推荐 0.01 ~ 0.10，避免 trace 后端爆。
	// validateConfig 限制取值范围在 [0, 1]，超出直接 fail-fast。
	OTELSamplingRatio float64

	// OTELInsecure 控制 OTLP gRPC 是否走明文（关 TLS）。env
	// OTEL_EXPORTER_OTLP_INSECURE，默认 true。
	//   - true（默认）：本地 docker-compose / k8s 同集群 collector 用明文 gRPC
	//     可以省去证书配置；
	//   - false：跨网络 / 跨集群上报必须走 TLS，运维要在容器内挂可信根证书。
	// 当 OTELEndpoint 为空时本字段被忽略（noop 路径无网络出口）。
	OTELInsecure bool
}

// TranslationSourceRolloutStage controls the RF5A source-identity compatibility
// window independently from the RF6A River job rollout.
type TranslationSourceRolloutStage string

const (
	// TranslationSourceRolloutCompat accepts legacy requests without expected
	// source identity and persists them as unverified rows.
	TranslationSourceRolloutCompat TranslationSourceRolloutStage = "compat"
	// TranslationSourceRolloutStrict requires the expected saved-content
	// revision or summary source hash for every new translation schedule.
	TranslationSourceRolloutStrict TranslationSourceRolloutStage = "strict"
)

// Valid reports whether the configured source-identity rollout stage is
// supported by this binary.
func (s TranslationSourceRolloutStage) Valid() bool {
	switch s {
	case TranslationSourceRolloutCompat, TranslationSourceRolloutStrict:
		return true
	default:
		return false
	}
}

// idempotencyEnv groups the two live idempotency settings read by Load.
// TTL==0 means middleware.NewPGIdempotencyCache applies its default.
type idempotencyEnv struct {
	enabled bool
	ttlMS   int
}

// otelEnv 把 OTLP 采样比例 + insecure 两个 env 收成本地小结构。Endpoint
// 不在这里读，因为它没有 err 分支，直接在 Load 里 envString 即可。
type otelEnv struct {
	samplingRatio float64
	insecure      bool
}
