package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/gin-gonic/gin"

	"webtag/internal/config"
	"webtag/internal/dto"
	"webtag/internal/handler"
	"webtag/internal/middleware"
)

func buildRuntimeRouter(
	cfg config.Config,
	layer *persistenceLayer,
	services runtimeServices,
	idempotencyCache *middleware.PGIdempotencyCache,
) (*gin.Engine, error) {
	extraMiddleware := buildExtraMiddleware(cfg)

	routerOpts := routerOptionsWithRuntimeDeps(cfg, layer, idempotencyCache)

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
	readerCapabilities := runtimeReaderCapabilities(cfg, reader != nil)
	return NewRouterWithDependencies(handler.Dependencies{
		ReaderCapabilities: &readerCapabilities,
		LinksWrite:         linkRoutes.linksWrite,
		LinksRead:          linkRoutes.linksRead,
		LinksContent:       linkRoutes.linksContent,
		ConversionPreview:  siteRoutes.conversionPreview,
		ConversionExecute:  siteRoutes.conversionExecute,
		Translations:       linkRoutes.translations,
		Ingest:             linkRoutes.ingest,
		Tags:               linkRoutes.tags,
		Tree:               linkRoutes.tree,
		Sites:              siteRoutes.sites,
		SiteManagement:     siteRoutes.siteManagement,
		SiteMerge:          siteRoutes.siteMerge,
		SiteSplit:          siteRoutes.siteSplit,
		ArchiveV2:          siteRoutes.archiveV2,
		LibrarySearch:      siteRoutes.librarySearch,
		Feeds:              feedRoutes.feeds,
		Reader:             reader,
	}, readiness, layer.logger, routerOpts, extraMiddleware...), nil
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
		RelatedTags: true,
		Activity:    true,
		History:     true,
		Trash:       true,
	}
}

// routerOptionsFromConfig 产出 RouterOptions 里**完全由 config 决定**的那部分。
// 需要装配期依赖的字段（会话密钥、身份仓储和幂等缓存）由调用方补齐。
func routerOptionsFromConfig(cfg config.Config) RouterOptions {
	return RouterOptions{
		TrustedProxyCIDRs:      append([]string(nil), cfg.Server.TrustedProxyCIDRs...),
		ExtensionAPIToken:      cfg.ExtensionAPIToken,
		MaxRequestBodyBytes:    middleware.DefaultMaxRequestBodyBytes,
		RequestDeadlineTimeout: durationMS(cfg.Server.WriteTimeoutMS),
	}
}

// routerOptionsWithRuntimeDeps 在 routerOptionsFromConfig 的基础上补齐需要
// 装配期依赖的字段（会话密钥、幂等缓存和安装身份）。
func routerOptionsWithRuntimeDeps(
	cfg config.Config,
	layer *persistenceLayer,
	idempotencyCache *middleware.PGIdempotencyCache,
) RouterOptions {
	opts := routerOptionsFromConfig(cfg)
	opts.SessionSigningKey = []byte(cfg.SessionSigningKey)
	opts.IdempotencyCache = idempotencyCache
	// Authenticated clients use the installation namespace to isolate local data.
	opts.InstallationIdentity = layer.installationIdentity
	return opts
}

var errNotReady = errors.New("component not ready")

func buildExtraMiddleware(cfg config.Config) []gin.HandlerFunc {
	return []gin.HandlerFunc{middleware.CORS(cfg.Server.CORSOrigins)}
}

func warnBootConfiguration(cfg config.Config, logger *slog.Logger) {
	if cfg.SessionSigningKeyEphemeral {
		// 这条 WARN 的收件人是几周后在排查「Reader 又要重填 key 了」的人。
		// 空 SESSION_SIGNING_KEY 不是关闭会话，而是把签名密钥绑在进程生命周期
		// 上：重启即换钥，所有在飞 cookie 当场作废，而 session 模式下浏览器里
		// 不留安装令牌，前端手里没有任何可重放的凭证，只能让用户重新填一次。
		// 自动更新会重启 Core，所以这等价于「每更新一次踢一次线」。
		logger.Warn("SESSION_SIGNING_KEY is empty: the Reader session cookie is signed with a key generated for this process, so every restart (including every automated update) invalidates all Reader sessions and users must re-enter the installation token; set it to a persistent value (openssl rand -base64 32) to keep sessions alive across restarts",
			"flag", "SESSION_SIGNING_KEY",
		)
	}
}
