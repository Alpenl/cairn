package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"webtag/internal/config"
	"webtag/internal/dto"
	"webtag/internal/handler"
	"webtag/internal/middleware"
	"webtag/internal/migrate"
	"webtag/internal/observability"
)

func (b *runtimeRouterBinding) buildRuntimeRouter(
	cfg config.Config,
	layer *persistenceLayer,
	services runtimeServices,
) (*gin.Engine, *middleware.RateLimiterController, error) {
	if b == nil || b.inventory == nil {
		return nil, nil, errors.New("build runtime router: unbound runtime dependencies")
	}
	if err := b.inventory.validateRouterDependencies(b.dependencies); err != nil {
		return nil, nil, err
	}
	extraMiddleware, rateLimitCtrl := buildExtraMiddleware(cfg, layer)
	metricsHandler := wrapMetricsHandler(cfg, layer.metrics)

	routerOpts := b.routerOptions(cfg, layer)

	dbReadiness := NewDatabaseReadiness(layer.pool)
	readiness := NewAggregatedReadiness(
		ReadinessCheck{Name: "database", Check: dbReadiness.Ready},
		ReadinessCheck{
			Name: "queue",
			Check: func(context.Context) error {
				if services.links.backgrounds.riverQueue != nil && services.links.backgrounds.riverQueue.Ready() {
					return nil
				}
				return errNotReady
			},
		},
	)

	linkRoutes := services.links.routes
	siteRoutes := services.sites.routes
	feedRoutes := services.feeds.routes
	reader := services.reader
	readerReady := reader != nil && layer != nil && layer.reader != nil
	if readerReady {
		var err error
		readerReady, err = readerSchemaReady(layer)
		if !readerReady && layer.logger != nil {
			if err != nil {
				layer.logger.Warn("reader routes disabled: reader schema is not ready", "error", observability.SafeError(err))
			} else {
				layer.logger.Warn("reader routes disabled: reader schema is not ready")
			}
		}
	}
	if !readerReady {
		reader = nil
	}
	readerCapabilities := runtimeReaderCapabilities(cfg, readerReady)
	return NewRouterWithDependencies(handler.Dependencies{
			WebsiteFeatures: &handler.WebsiteFeatures{
				LibraryKindAPI: cfg.LibraryKindAPIEnabled, SiteLibraryWrite: cfg.SiteLibraryWriteEnabled,
				SiteAutoClassification: cfg.SiteAutoClassificationEnabled, SiteAdvancedManagement: cfg.SiteAdvancedManagementEnabled,
				ReaderCapabilities: &readerCapabilities,
			},
			LinksWrite:          linkRoutes.linksWrite,
			LinksRead:           linkRoutes.linksRead,
			LinksContent:        linkRoutes.linksContent,
			ConversionPreview:   siteRoutes.conversionPreview,
			ConversionExecute:   siteRoutes.conversionExecute,
			Translations:        linkRoutes.translations,
			Ingest:              linkRoutes.ingest,
			Jobs:                linkRoutes.jobs,
			Tags:                linkRoutes.tags,
			Tree:                linkRoutes.tree,
			Sites:               siteRoutes.sites,
			SiteManagement:      siteRoutes.siteManagement,
			SiteMerge:           siteRoutes.siteMerge,
			SiteSplit:           siteRoutes.siteSplit,
			ArchiveV2:           siteRoutes.archiveV2,
			ClassificationRules: siteRoutes.classificationRules,
			LibraryReviews:      siteRoutes.libraryReviews,
			LibrarySearch:       siteRoutes.librarySearch,
			ConceptMerges:       conceptMergeHandlerAdapter{svc: services.administration.conceptMerges},
			Feeds:               feedRoutes.feeds,
			Reader:              reader,
		}, metricsHandler, readiness, layer.logger, layer.metrics, routerOpts, extraMiddleware...),
		rateLimitCtrl, nil
}

const readerSchemaProbeTimeout = 500 * time.Millisecond

var readerRequiredTables = []string{
	"reader_thought_ops",
	"reader_thoughts",
	"reader_notes",
	"reader_note_history",
	"reader_inbox",
	"reader_categories",
	"reader_categorizables",
	"reader_todos",
	"reader_engagement",
	"reader_feed_feedback",
	"reader_feed_saves",
	"reader_feed_snapshots",
	"reader_tag_activity",
	"reader_domain_activity",
	"reader_content_history",
	"reader_inbox_jobs",
	"reader_thought_tombstones",
	"reader_host_purge_receipts",
}

// readerSchemaReady keeps a partially migrated database from exposing Reader
// routes that would otherwise fail with table/column errors on the first
// request. A nil pool is treated as ready for dependency-only router tests;
// production always supplies a real pool.
func readerSchemaReady(layer *persistenceLayer) (bool, error) {
	if layer == nil || layer.reader == nil {
		return false, errors.New("reader repository is not configured")
	}
	if layer.pool == nil {
		return true, nil
	}

	readerMigrations := make([]string, 0, len(migrate.Steps()))
	for _, step := range migrate.Steps() {
		if strings.HasPrefix(step.ID, "reader") {
			readerMigrations = append(readerMigrations, step.ID)
		}
	}
	if len(readerMigrations) == 0 {
		return false, errors.New("reader migrations are not defined")
	}

	ctx, cancel := context.WithTimeout(context.Background(), readerSchemaProbeTimeout)
	defer cancel()
	var migrationsReady, tablesReady bool
	err := layer.pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM schema_migrations WHERE version = ANY($1::text[])) = $2,
			(SELECT COUNT(*)
			 FROM pg_catalog.pg_class AS c
			 JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
			 WHERE n.nspname = 'public'
			   AND c.relkind IN ('r', 'p')
			   AND c.relname = ANY($3::text[])) = $4
	`, readerMigrations, len(readerMigrations), readerRequiredTables, len(readerRequiredTables)).Scan(&migrationsReady, &tablesReady)
	if err != nil {
		return false, fmt.Errorf("probe Reader schema: %w", err)
	}
	if !migrationsReady {
		return false, errors.New("reader migrations are incomplete")
	}
	if !tablesReady {
		return false, errors.New("reader tables are incomplete")
	}
	return true, nil
}

func runtimeReaderCapabilities(cfg config.Config, enabled bool) dto.ReaderCapabilitiesResponse {
	if !enabled {
		return dto.ReaderCapabilitiesResponse{}
	}
	return dto.ReaderCapabilitiesResponse{
		Annotations: true,
		Notes:       true,
		Inbox:       true,
		Todos:       true,
		Engagement:  true,
		Home:        true,
		Feed:        true,
		AI:          strings.TrimSpace(cfg.Analyzer.BaseURL) != "" && strings.TrimSpace(cfg.Analyzer.Model) != "",
		Semantic:    strings.TrimSpace(cfg.Embedding.Model) != "",
		Activity:    true,
		History:     true,
		Trash:       true,
	}
}

// runtimeRouterBinding is the only production value allowed to construct the
// router. It retains the acquisition inventory and revalidates its private
// dependency set at the consumption boundary, closing the gap between an
// earlier identity check and the final RouterOptions assignment.
type runtimeRouterBinding struct {
	inventory    *runtimeBuildAcquisitions
	dependencies runtimeRouterDependencies
}

// runtimeRouterDependencies is the candidate dependency set sealed into a
// runtimeRouterBinding after acquisition identity validation. Production never
// passes this copyable value directly to the router constructor.
type runtimeRouterDependencies struct {
	idempotencyCache *middleware.PGIdempotencyCache
}

func newRuntimeRouterDependencies(
	idempotencyCache *middleware.PGIdempotencyCache,
) runtimeRouterDependencies {
	return runtimeRouterDependencies{idempotencyCache: idempotencyCache}
}

// routerOptionsFromConfig 产出 RouterOptions 里**完全由 config 决定**的那部分。
// 需要装配期依赖的字段（logger、仓储、限流器、幂等缓存）由调用方补齐。
//
// 抽出来的是**结构体产物**而不是单个字段的取值函数。上一版抽的是
// `routerAuthOptionsFor(cfg) bool`，测试断言那个函数的返回值——结果是把未测的
// 那一跳往下移了一格：把 `AllowOpenAccess: routerAuthOptionsFor(cfg)` 直接改成
// `AllowOpenAccess: true`，全量测试照样绿。断言守住的是函数体，不是赋值行。
//
// 现在测试断言的是这个函数返回的结构体，赋值行就在函数里，改任何一个字段
// 都会红。
func routerOptionsFromConfig(cfg config.Config) RouterOptions {
	return RouterOptions{
		TrustedProxyCIDRs:      append([]string(nil), cfg.Server.TrustedProxyCIDRs...),
		AdminAuthToken:         cfg.AdminAuthToken,
		MetricsAuthToken:       cfg.MetricsAuthToken,
		ExtensionAPIToken:      cfg.ExtensionAPIToken,
		AllowOpenAccess:        cfg.PublicAPIOpen,
		PProfEnabled:           cfg.PProfEnabled,
		AppEnv:                 cfg.AppEnv,
		MaxRequestBodyBytes:    middleware.DefaultMaxRequestBodyBytes,
		RequestDeadlineTimeout: durationMS(cfg.Server.WriteTimeoutMS),
		GzipEnabled:            cfg.GzipEnabled,
		GzipMinLength:          cfg.GzipMinLength,
	}
}

// routerOptionsWithRuntimeDeps 在 routerOptionsFromConfig 的基础上补齐需要
// 装配期依赖的字段（会话密钥、logger、幂等缓存、聚合缓存失效器）。
//
// 抽成函数与 routerOptionsFromConfig 是同一个理由（见其上方注释）：**赋值行
// 必须待在被断言的函数体内**。此前这些赋值散在 buildRuntimeRouter 里，而
// buildRuntimeRouter 需要真实的 DB 池才能调用，于是没有任何单元测试覆盖得到
// 它们——删掉 `opts.AggregateInvalidator = ...` 这一行，所有 HTTP 写路径都
// 不再失效聚合缓存，而 Go 全量测试照样全绿。现在测试断言这个函数的返回值，
// 删任何一行都会红。
func routerOptionsWithRuntimeDeps(
	cfg config.Config,
	layer *persistenceLayer,
	idempotencyCache *middleware.PGIdempotencyCache,
) RouterOptions {
	binding := &runtimeRouterBinding{dependencies: runtimeRouterDependencies{
		idempotencyCache: idempotencyCache,
	}}
	return binding.routerOptions(cfg, layer)
}

func (b *runtimeRouterBinding) routerOptions(
	cfg config.Config,
	layer *persistenceLayer,
) RouterOptions {
	runtimeDeps := b.dependencies
	opts := routerOptionsFromConfig(cfg)
	opts.SessionSigningKey = []byte(cfg.SessionSigningKey)
	opts.Logger = layer.logger
	opts.IdempotencyEnabled = cfg.IdempotencyEnabled && runtimeDeps.idempotencyCache != nil
	opts.IdempotencyCache = runtimeDeps.idempotencyCache
	// 聚合缓存（标签 + 域名）在成功写请求后统一失效。
	// 这是所有 HTTP 写路径的**唯一**失效点——站点转换 / 合并 / 拆分 / 删除、
	// 资料与标签编辑、审核队列操作都在此覆盖，不必逐个 service 注入失效器。
	opts.AggregateInvalidator = layer.aggregateCacheInvalidator()
	// PF5：条件请求的版本号来源。读版本号由 links 上的语句级触发器维护，
	// 这里只读。
	opts.ConditionalGetRevisions = layer.readRevisions
	return opts
}

var errNotReady = errors.New("component not ready")

func buildExtraMiddleware(cfg config.Config, layer *persistenceLayer) ([]gin.HandlerFunc, *middleware.RateLimiterController) {
	out := []gin.HandlerFunc{middleware.CORS(cfg.Server.CORSOrigins)}
	var rateLimitCtrl *middleware.RateLimiterController

	if cfg.RateLimit.RPS > 0 {
		handler, ctrl := middleware.RateLimit(middleware.RateLimitOptions{
			RPS:       cfg.RateLimit.RPS,
			Burst:     cfg.RateLimit.Burst,
			SkipPaths: []string{"/metrics", "/health", "/ready"},
			Logger:    layer.logger,
		})
		out = append(out, handler)
		rateLimitCtrl = ctrl
	}
	if cfg.Analyzer.AllowUnsafeTargets {
		out = append(out, WithHealthExtraFields(map[string]any{
			"unsafe_targets_allowed": true,
		}))
	}
	return out, rateLimitCtrl
}

func wrapMetricsHandler(cfg config.Config, metrics *observability.Metrics) http.Handler {
	handler := metrics.Handler()
	if cfg.MetricsAuthToken != "" {
		return middleware.BearerAuth(handler, cfg.MetricsAuthToken)
	}
	return handler
}

func warnUnsafeBootDefaults(cfg config.Config, logger *slog.Logger) {
	if cfg.Analyzer.AllowUnsafeTargets {
		logger.Warn("unsafe target validation disabled, do not use in production",
			"flag", "AI_ALLOW_UNSAFE_TARGETS",
		)
	}
	if cfg.MetricsAuthToken == "" {
		logger.Warn("metrics endpoint is unauthenticated, set the env var to require Bearer auth",
			"flag", "METRICS_AUTH_TOKEN",
		)
	}
	if cfg.RateLimit.RPS == 0 {
		logger.Warn("rate limiting is disabled, set the env var > 0 to enable per-IP throttling",
			"flag", "RATE_LIMIT_RPS",
		)
	}
	if cfg.AppEnv != "dev" && cfg.AdminAuthToken == "" {
		logger.Warn("admin routes are sealed: ADMIN_AUTH_TOKEN is empty in non-dev environment; /api/admin/* will return 401 for every request",
			"flag", "ADMIN_AUTH_TOKEN",
			"app_env", cfg.AppEnv,
		)
	}
	if cfg.PublicAPIOpen {
		// 公开 API 现在只有在显式 PUBLIC_API_OPEN=true 时才可能无鉴权开放
		// （multi 模式恒 fail-closed，该开关无效，不打此 WARN）。
		// 纯内网单机下开放没问题，但一旦 CORS 放行了非 localhost 来源，浏览器
		// 里任意页面都能跨域读写知识库——此时把措辞加重。
		if origins := nonLocalhostCORSOrigins(cfg.Server.CORSOrigins); len(origins) > 0 {
			logger.Warn("PUBLIC_API_OPEN=true leaves the public API UNAUTHENTICATED while CORS allows non-localhost origins; any site in those origins can read, export and delete the knowledge base. Disable it and configure EXTENSION_API_TOKEN",
				"flag", "PUBLIC_API_OPEN",
				"non_localhost_cors_origins", origins,
			)
		} else {
			logger.Warn("PUBLIC_API_OPEN=true: the public API serves unauthenticated requests with full read/write/export scope; only appropriate on a trusted network or behind an authenticating proxy",
				"flag", "PUBLIC_API_OPEN",
			)
		}
	}
	if cfg.AppEnv == "prod" && cfg.OTELEndpoint != "" && cfg.OTELInsecure {
		logger.Warn("OTLP exporter is configured to use plaintext gRPC in production; trace data flows unauthenticated, set the env var to false to require TLS",
			"flag", "OTEL_EXPORTER_OTLP_INSECURE",
			"endpoint", cfg.OTELEndpoint,
		)
	}
}

// nonLocalhostCORSOrigins returns the subset of the configured CORS
// origins that are NOT loopback addresses. The "*" wildcard counts as
// non-local (it allows every site). Loopback is recognised by host ==
// localhost / 127.0.0.1 / ::1 regardless of scheme or port, so
// "http://localhost:3000" and "https://127.0.0.1:8080" are treated as
// local. Anything this cannot confidently classify as loopback is
// reported, because the WARN it feeds errs on the side of nagging the
// operator rather than staying silent on a real exposure.
func nonLocalhostCORSOrigins(origins []string) []string {
	out := make([]string, 0, len(origins))
	for _, origin := range origins {
		trimmed := strings.TrimSpace(origin)
		if trimmed == "" {
			continue
		}
		if isLoopbackOrigin(trimmed) {
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isLoopbackOrigin reports whether a CORS origin string points at the
// local machine. It strips an optional scheme and trailing :port, then
// matches the bare host against the loopback set. "*" is explicitly not
// loopback.
func isLoopbackOrigin(origin string) bool {
	host := origin
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	// Drop a trailing path if any (CORS origins should not carry one, but
	// be defensive) then the :port suffix.
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	// IPv6 literal: [::1]:8080 → ::1
	if strings.HasPrefix(host, "[") {
		if end := strings.IndexByte(host, ']'); end >= 0 {
			return host[1:end] == "::1"
		}
		return false
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	switch host {
	case "localhost", "127.0.0.1":
		return true
	default:
		return false
	}
}
