package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics 聚合了 WebTag 所有 Prometheus 指标，并持有独立的 Registry，
// 便于在测试中创建多份互不干扰的实例，也避免污染全局默认注册表。
type Metrics struct {
	registry *prometheus.Registry

	ParseRunsTotal                       *prometheus.CounterVec
	ParseFailuresTotal                   *prometheus.CounterVec
	ParseLowConfidenceTotal              *prometheus.CounterVec
	TaggingRetrieveFallbackTotal         *prometheus.CounterVec
	TaggingRecallCandidatesTotal         *prometheus.CounterVec
	ParseEnrichmentSkippedTotal          *prometheus.CounterVec
	FetchEscalationTotal                 *prometheus.CounterVec
	EnsureParentPathDepth                prometheus.Histogram
	TreeResponseTruncatedTotal           *prometheus.CounterVec
	HTTPRequestTotal                     *prometheus.CounterVec
	HTTPRequestDuration                  *prometheus.HistogramVec
	SiteSearchDuration                   *prometheus.HistogramVec
	LibraryClassificationTotal           *prometheus.CounterVec
	LibraryClassificationCorrectionTotal *prometheus.CounterVec
	SiteAggregationTotal                 *prometheus.CounterVec
	SiteEntryTotal                       *prometheus.CounterVec
	SiteConversionTotal                  *prometheus.CounterVec
	SiteMergeTotal                       *prometheus.CounterVec
	SiteSplitTotal                       *prometheus.CounterVec
	SitePayloadPurgeTotal                *prometheus.CounterVec
	ReaderContentHistoryDeletedTotal     *prometheus.CounterVec
	ReaderContentHistoryCleanupRunsTotal *prometheus.CounterVec
	ReaderInboxDispatchRepairsTotal      *prometheus.CounterVec
	TranslationSourceOutcomesTotal       *prometheus.CounterVec
	TranslationTerminalOutcomesTotal     *prometheus.CounterVec

	// DBQueryDuration tracks the latency of individual DB queries,
	// labelled by method (e.g. "Exec", "Query", "QueryRow").
	// Buckets span 1ms–4s (ExponentialBuckets(0.001,2,12)) to cover
	// both fast index lookups and heavier analytical queries.
	DBQueryDuration *prometheus.HistogramVec

	// FetcherDuration tracks per-fetch HTTP round-trip latency.
	// Labels: fetcher_type (basic, jina, arxiv, …) and host_class
	// (derived from the request hostname).  Buckets span 50ms–25s
	// because remote fetches can legitimately take tens of seconds.
	FetcherDuration *prometheus.HistogramVec

	// PDFParseOutcomesTotal tracks the isolated PDF helper result using only
	// bounded outcome and resource-limit labels. It must never include a URL,
	// document identifier, or extracted text.
	PDFParseOutcomesTotal *prometheus.CounterVec

	// PgxpoolAcquireCount / PgxpoolIdleConns / PgxpoolTotalConns are
	// real-time gauges fed by a background goroutine reading pool.Stat()
	// every 15 s.  They surface pool pressure without requiring a
	// full-fledged pgx tracer on the pool itself.
	PgxpoolAcquireCount prometheus.GaugeFunc
	PgxpoolIdleConns    prometheus.GaugeFunc
	PgxpoolTotalConns   prometheus.GaugeFunc
}

// NewMetrics 构造一个新的 Metrics 实例：内部使用专属 Registry，
// 注册解析、HTTP、DB、Fetcher 等所有预定义指标。队列相关指标不在此处：
// Phase 13 改用 River 后，队列深度由 GaugeFunc（webtag_queue_jobs_pending，
// 见 registerQueuePendingGauge）按 river_job 表实时查询，不再由队列层埋点上报。
// pgxpool 相关 GaugeFunc 在此处置 nil，需要在 pool 可用后通过
// RegisterPgxpoolGauges 完成注册。
func NewMetrics() *Metrics {
	registry := prometheus.NewRegistry()
	factory := promauto.With(registry)

	return &Metrics{
		registry: registry,
		ParseRunsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag",
			Subsystem: "parser",
			Name:      "runs_total",
			Help:      "Total parser pipeline runs partitioned by result and fetcher type.",
		}, []string{"result", "fetcher_type", "content_type"}),
		ParseFailuresTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag",
			Subsystem: "parser",
			Name:      "failures_total",
			Help:      "Total parser pipeline failures partitioned by error category and stage.",
		}, []string{"stage", "error_category"}),
		ParseLowConfidenceTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag",
			Subsystem: "parser",
			Name:      "low_confidence_total",
			Help:      "Total parser outputs marked as low confidence partitioned by reason and fetcher type.",
		}, []string{"reason", "fetcher_type"}),
		// TaggingRetrieveFallbackTotal counts parse runs that fell back from
		// the v3 retrieve-then-select tagging path to legacy free-generation,
		// partitioned by reason: "embedding_failed" (content embedding call
		// errored), "retrieve_failed" (nearest-neighbour candidate query
		// errored), "cold_start" (concept词表 below CONCEPT_COLD_START_MIN),
		// "no_candidates" (warm词表 but recall returned nothing for this
		// model). A rising "embedding_failed"/"retrieve_failed" rate means the
		// embedding backend is degrading tagging quality; "cold_start" is
		// expected high early then trending to zero as the词表 fills.
		TaggingRetrieveFallbackTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag",
			Subsystem: "tagging",
			Name:      "retrieve_fallback_total",
			Help:      "Total parse runs that fell back from retrieve-then-select tagging to free-generation, partitioned by reason (embedding_failed, retrieve_failed, cold_start, no_candidates).",
		}, []string{"reason"}),
		// TaggingRecallCandidatesTotal counts candidate concepts injected into
		// the analyzer prompt, partitioned by which recall leg produced them:
		// "direct" (content↔concept cosine recall) and "neighbour"
		// (content↔link↔concept co-occurrence). Only post-dedup contributions
		// are counted, so the neighbour series measures what that leg ADDED
		// rather than what it returned.
		//
		// Without this the co-occurrence leg is unfalsifiable: its whole
		// premise is recalling concepts the direct leg cannot reach, and a
		// neighbour series near zero would mean the leg is paying a query per
		// parse for nothing. It is also the only way to tune
		// neighbourConceptLimit against real prompt budgets instead of by
		// argument.
		TaggingRecallCandidatesTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag",
			Subsystem: "tagging",
			Name:      "recall_candidates_total",
			Help:      "Total candidate concepts injected into the analyzer prompt, partitioned by recall leg (direct, neighbour). Counted after cross-leg dedup.",
		}, []string{"leg"}),
		// ParseEnrichmentSkippedTotal counts post-terminal enrichment steps that
		// did not complete for a link that was already written to "done".
		//
		// These steps are fail-soft by design: the parse contract does not
		// depend on them, so a failure only logs a WARN. The problem is that
		// this makes the resulting gap invisible — there is no reconciliation
		// worker, and a missing vector or concept edge stays missing until the
		// link happens to be parsed again. A single embedding outage therefore
		// leaves a permanent hole in semantic search and the concept graph with
		// nothing to report it.
		//
		// This counter makes the hole countable. Labels:
		//   kind:   "content_embedding" | "concept_attach"
		//   reason: "not_wired"  该能力未装配（预期内，用于确认灰度状态）
		//           "disabled"   已装配但运行时关闭
		//           "empty_input" 无可用输入，跳过属正常
		//           "failed"     调用或写入出错——这一项才是真正的数据缺口
		//
		// 告警应盯 reason="failed"：它直接等于「有多少链接永久缺了向量/概念」。
		ParseEnrichmentSkippedTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag",
			Subsystem: "parser",
			Name:      "enrichment_skipped_total",
			Help:      "Total post-terminal enrichment steps skipped or failed for an already-done link, partitioned by kind (content_embedding, concept_attach) and reason (not_wired, disabled, empty_input, failed).",
		}, []string{"kind", "reason"}),
		// FetchEscalationTotal counts auto-escalations from the light/省 token
		// fetch path to a full deep Fetch (Phase 10 low-confidence escalation
		// ladder). A run escalates when it walked the light path (global
		// PreferLight or a link with no explicit parse_depth) and the light
		// result was judged thin (fetcher_type+thin / thin_content / body below
		// threshold). The "outcome" label distinguishes whether the re-fetch
		// recovered usable content ("recovered") or the deep result was still
		// thin ("still_thin"). A high still_thin share means the upstream pages
		// are genuinely sparse and the extra fetch is not paying off; a high
		// recovered share confirms the ladder is buying tagging quality. Each
		// link escalates at most once, so this counter also bounds the extra
		// fetch amplification.
		FetchEscalationTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag",
			Subsystem: "fetch",
			Name:      "escalation_total",
			Help:      "Total light→deep fetch auto-escalations partitioned by outcome (recovered, still_thin).",
		}, []string{"outcome"}),
		// Histogram of the path_depth observed by ensureParent. This remains
		// useful after skeleton creation was removed: it tracks how many
		// ancestor URLs each parse may need to lookup. PromQL like
		//   histogram_quantile(0.95,
		//     rate(webtag_parser_ensure_parent_path_depth_bucket[5m]))
		// or
		//   sum(rate(webtag_parser_ensure_parent_path_depth_bucket
		//     {le="5"}[24h])) /
		//     sum(rate(webtag_parser_ensure_parent_path_depth_count[24h]))
		// gives operators the trigger signal directly. Buckets up to
		// 32 because urlmeta.AncestorURLs caps at that depth.
		EnsureParentPathDepth: factory.NewHistogram(prometheus.HistogramOpts{
			Namespace: "webtag",
			Subsystem: "parser",
			Name:      "ensure_parent_path_depth",
			Help:      "Distribution of path_depth observed when the pipeline walks the ancestor chain in ensureParent. Tracks the M9 trigger threshold (path_depth>5 share) without a separate dashboard query.",
			Buckets:   []float64{0, 1, 2, 3, 5, 8, 13, 21, 32},
		}),
		// TreeResponseTruncatedTotal counts /api/tree responses that hit
		// the SQL LIMIT 5000 cap. Lets operators alert on truncation
		// frequency directly instead of grepping slog Warn lines. Label
		// "domain_filter" is "all" when no domain filter is supplied;
		// otherwise "filtered" to keep cardinality bounded (we never
		// emit the actual domain).
		TreeResponseTruncatedTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag",
			Subsystem: "tree",
			Name:      "responses_truncated_total",
			Help:      "Total /api/tree responses that hit the soft cap and were returned truncated. Use to alert on consumers fetching too-large slices.",
		}, []string{"domain_filter"}),
		HTTPRequestTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag",
			Subsystem: "http",
			Name:      "requests_total",
			Help:      "Total HTTP requests partitioned by method, route, and status.",
		}, []string{"method", "route", "status"}),
		// Wave 2 M3：histogram 增加 status_class 标签（2xx/3xx/4xx/5xx）
		// 而不是完整 status 码，原因是后者会让单 (method,route) 组合的
		// 时间序列数量 ×N。bucket 上限从 DefBuckets 的 10s 扩到 25s 是
		// 为了真实捕获 fetcher → analyzer 这条 60s 总预算下偶发慢请求
		// 的尾部——之前所有 >10s 的请求都汇聚成 +Inf 桶，p99/p999
		// 完全不可读。
		HTTPRequestDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "webtag",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "HTTP request latency partitioned by method, route, and status class (2xx/3xx/4xx/5xx). Buckets 5ms–25s.",
			Buckets:   []float64{0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25},
		}, []string{"method", "route", "status_class"}),
		SiteSearchDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "webtag",
			Subsystem: "site",
			Name:      "search_duration_seconds",
			Help:      "Website grouped-search duration partitioned by execution mode (keyword, semantic).",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 12),
		}, []string{"mode"}),
		LibraryClassificationTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag", Subsystem: "library", Name: "classification_total",
			Help: "Final collection classifications partitioned by bounded request/final/source/confidence/version dimensions.",
		}, []string{"requested", "final", "source", "confidence_band", "version"}),
		LibraryClassificationCorrectionTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag", Subsystem: "library", Name: "classification_correction_total",
			Help: "Successful user corrections of automatic classifications, partitioned by source/target kind and whether a personal rule determined the original result.",
		}, []string{"from", "to", "had_rule"}),
		SiteAggregationTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag", Subsystem: "site", Name: "aggregation_total",
			Help: "Automatic site aggregation outcomes partitioned by result and identity adapter.",
		}, []string{"result", "adapter"}),
		SiteEntryTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag", Subsystem: "site", Name: "entry_total",
			Help: "Site entry aggregation operations partitioned by operation (created, recollected).",
		}, []string{"operation"}),
		SiteConversionTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag", Subsystem: "site", Name: "conversion_total",
			Help: "Collection conversion executions partitioned by source, target, and result.",
		}, []string{"from", "to", "result"}),
		SiteMergeTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag", Subsystem: "site", Name: "merge_total",
			Help: "Site merge executions partitioned by result (success, conflict, error).",
		}, []string{"result"}),
		SiteSplitTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag", Subsystem: "site", Name: "split_total",
			Help: "Site split executions partitioned by result (success, conflict, error).",
		}, []string{"result"}),
		SitePayloadPurgeTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag", Subsystem: "site", Name: "payload_purge_total",
			Help: "Expired site payload cleanup attempts partitioned by trigger and result.",
		}, []string{"trigger", "result"}),
		ReaderContentHistoryDeletedTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag", Subsystem: "reader_content_history", Name: "deleted_total",
			Help: "Content history snapshots deleted by automatic retention.",
		}, []string{}),
		ReaderContentHistoryCleanupRunsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag", Subsystem: "reader_content_history", Name: "cleanup_runs_total",
			Help: "Content history retention runs partitioned by backlog and bounded failure category.",
		}, []string{"backlog", "failure_category"}),
		ReaderInboxDispatchRepairsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag", Subsystem: "reader_inbox_dispatch", Name: "repairs_total",
			Help: "Inbox proposal orphan repair outcomes partitioned by bounded outcome (success, failure).",
		}, []string{"outcome"}),
		TranslationSourceOutcomesTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag", Subsystem: "translation_source", Name: "outcomes_total",
			Help: "Translation source rollout outcomes partitioned by bounded outcome (legacy_unverified_write, verified_schedule, content_revision_conflict, source_block_conflict, translation_schema_transition).",
		}, []string{"outcome"}),
		TranslationTerminalOutcomesTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag", Subsystem: "translation_terminal_reconciler", Name: "outcomes_total",
			Help: "Translation terminal reconciliation outcomes partitioned by bounded terminal code or rejection reason.",
		}, []string{"outcome"}),

		DBQueryDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "webtag",
			Subsystem: "db",
			Name:      "query_duration_seconds",
			Help:      "DB query latency partitioned by method (Exec, Query, QueryRow). Buckets 1ms–4s.",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 12),
		}, []string{"method"}),

		FetcherDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "webtag",
			Subsystem: "fetcher",
			Name:      "duration_seconds",
			Help:      "Per-fetch HTTP round-trip latency partitioned by fetcher_type and host_class. Buckets 50ms–25s.",
			Buckets:   prometheus.ExponentialBuckets(0.05, 2, 10),
		}, []string{"fetcher_type", "host_class"}),

		PDFParseOutcomesTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "webtag",
			Subsystem: "pdf_parse",
			Name:      "outcomes_total",
			Help:      "Total isolated PDF parse outcomes partitioned by bounded outcome and resource limit type.",
		}, []string{"outcome", "limit"}),

		// PgxpoolAcquireCount / PgxpoolIdleConns / PgxpoolTotalConns are
		// registered as GaugeFuncs later via RegisterPgxpoolGauges once
		// the pool is available.  The zero-value nil fields are safe: the
		// instrumented code guards against nil Metrics.
		PgxpoolAcquireCount: nil,
		PgxpoolIdleConns:    nil,
		PgxpoolTotalConns:   nil,
	}
}

// PgxpoolStatReader is a minimal subset of *pgxpool.Pool that
// RegisterPgxpoolGauges needs, expressed as an interface so the method
// can be called from deps.go without importing pgxpool here.
type PgxpoolStatReader interface {
	Stat() PgxpoolStat
}

// PgxpoolStat mirrors the fields of *pgxpool.Stat that we expose as gauges.
type PgxpoolStat struct {
	AcquireCount int64
	IdleConns    int32
	TotalConns   int32
}

// RegisterPgxpoolGauges wires three GaugeFuncs that read pgxpool.Stat()
// on every Prometheus scrape. Calling this more than once on the same Metrics
// is a no-op (the second call would panic on duplicate registration, so we
// guard with nil checks on the fields). deps.go calls this once after the
// pool is open.
func (m *Metrics) RegisterPgxpoolGauges(stat func() PgxpoolStat) {
	if m == nil || stat == nil {
		return
	}
	factory := promauto.With(m.registry)

	if m.PgxpoolAcquireCount == nil {
		m.PgxpoolAcquireCount = factory.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "webtag",
			Subsystem: "pgxpool",
			Name:      "acquire_count",
			Help:      "Cumulative number of successful pool acquires (pgxpool.Stat.AcquireCount).",
		}, func() float64 {
			return float64(stat().AcquireCount)
		})
	}
	if m.PgxpoolIdleConns == nil {
		m.PgxpoolIdleConns = factory.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "webtag",
			Subsystem: "pgxpool",
			Name:      "idle_conns",
			Help:      "Current number of idle connections in the pool (pgxpool.Stat.IdleConns).",
		}, func() float64 {
			return float64(stat().IdleConns)
		})
	}
	if m.PgxpoolTotalConns == nil {
		m.PgxpoolTotalConns = factory.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "webtag",
			Subsystem: "pgxpool",
			Name:      "total_conns",
			Help:      "Current total number of connections in the pool (pgxpool.Stat.TotalConns).",
		}, func() float64 {
			return float64(stat().TotalConns)
		})
	}
}

// RegisterGaugeFunc registers a custom GaugeFunc against this Metrics'
// private registry. Used for gauges whose value source is owned outside
// the observability package (e.g. concept_merge_proposals_pending, fed by
// the proposal repository through a cached counter). Subsystem + Name
// must be unique within the "webtag" namespace; a duplicate registration
// panics (the same fail-loud contract as promauto), so callers must
// register each gauge exactly once at wiring time. A nil receiver or nil
// value func is a no-op so opt-out wiring stays a one-liner.
func (m *Metrics) RegisterGaugeFunc(subsystem, name, help string, value func() float64) {
	if m == nil || value == nil {
		return
	}
	promauto.With(m.registry).NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: "webtag",
		Subsystem: subsystem,
		Name:      name,
		Help:      help,
	}, value)
}

// RegisterConstGaugeFunc is RegisterGaugeFunc with bounded constant labels.
// It is intended for finite state matrices (for example review kind/status),
// never request-derived labels.
func (m *Metrics) RegisterConstGaugeFunc(subsystem, name, help string, labels prometheus.Labels, value func() float64) {
	if m == nil || value == nil {
		return
	}
	promauto.With(m.registry).NewGaugeFunc(prometheus.GaugeOpts{Namespace: "webtag", Subsystem: subsystem, Name: name, Help: help, ConstLabels: labels}, value)
}

// Handler 返回当前 Metrics 对应的 Prometheus 暴露 HTTP handler；
// m 为 nil 时退回到全局默认 Registry 的 handler，避免调用方需要额外做 nil 判断。
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return promhttp.Handler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
