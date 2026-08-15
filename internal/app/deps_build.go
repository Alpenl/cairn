package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"webtag/internal/app/durablework"
	"webtag/internal/buildinfo"
	"webtag/internal/config"
	"webtag/internal/middleware"
	"webtag/internal/observability"
	"webtag/internal/service"
	"webtag/internal/service/linktranslation"
	"webtag/internal/service/translator"
	"webtag/internal/worker"
)

// BuildRuntime is the top-level wiring entrypoint. It constructs the complete
// runtime in explicit acquisition order, registering cleanup next to every
// acquired resource so a later failure unwinds the partial graph in reverse.
// Runtime background startup is deferred until Runtime.Start.
func BuildRuntime(ctx context.Context, cfg config.Config) (*Runtime, error) {
	return buildRuntimeWithOptions(ctx, cfg, runtimeBuildOptions{})
}

//nolint:gocyclo // reason: explicit linear acquisition order is the RF7 ownership inventory; splitting it would hide cleanup adjacency
func buildRuntimeWithOptions(ctx context.Context, cfg config.Config, options runtimeBuildOptions) (built *Runtime, buildErr error) {
	options = options.withDefaults()
	acquisitions := newProductionRuntimeBuildAcquisitions(options.acquisitions)
	cleanupLogger := slog.Default()
	defer func() {
		finalErr, cleanupErr := acquisitions.CleanupFailure(ctx, defaultRuntimeCleanupTimeout, buildErr)
		buildErr = finalErr
		if finalErr != nil {
			built = nil
		}
		if cleanupErr == nil {
			return
		}
		cleanupLogger.ErrorContext(
			context.WithoutCancel(ctx),
			"runtime build cleanup failed",
			"error", observability.SafeError(cleanupErr),
		)
	}()

	var tracerShutdown observability.TracerShutdown
	if err := acquisitions.Acquire(runtimeBuildTracer, func() (runtimeAcquiredResource, error) {
		var err error
		tracerShutdown, err = options.initTracer(ctx, observability.InitTracerOptions{
			Endpoint:       cfg.OTELEndpoint,
			SamplingRatio:  cfg.OTELSamplingRatio,
			Insecure:       cfg.OTELInsecure,
			AppEnv:         cfg.AppEnv,
			ServiceName:    "webtag",
			ServiceVersion: buildinfo.VersionValue(),
		})
		return newRuntimeTracerAcquiredResource(tracerShutdown), err
	}); err != nil {
		return nil, err
	}

	var layer *persistenceLayer
	if err := acquisitions.Acquire(runtimeBuildPersistenceLayer, func() (runtimeAcquiredResource, error) {
		var err error
		layer, err = options.openPersistence(ctx, cfg)
		resource := newRuntimePersistenceAcquiredResource(layer)
		if err != nil {
			return resource, err
		}
		if layer == nil {
			return resource, errors.New("persistence constructor returned a nil layer")
		}
		return resource, nil
	}); err != nil {
		return nil, err
	}
	cleanupLogger = layer.logger

	warnUnsafeBootDefaults(cfg, layer.logger)

	var (
		httpClients *runtimeHTTPClientOwner
		stack       fetcherStack
	)
	if err := acquisitions.Acquire(runtimeBuildHTTPClients, func() (runtimeAcquiredResource, error) {
		httpClients = options.newHTTPClientOwner()
		if httpClients == nil {
			return runtimeAcquiredResource{}, errors.New("HTTP client owner constructor returned nil")
		}
		return newRuntimeBackgroundAcquiredResource(httpClients), nil
	}); err != nil {
		return nil, err
	}
	stack = httpClients.buildFetcherStack(cfg, layer.metrics)
	analyzerSvc := buildAnalyzer(cfg, stack.analyzerClient, stack.visionClient, layer.logger)
	translatorSvc := buildTranslator(cfg, stack.analyzerClient)

	pipeline, embedder := httpClients.buildParsePipeline(cfg, layer, stack.manager, analyzerSvc)
	translationProcessor := buildTranslationProcessor(layer, translatorSvc)
	var readerAI service.ReaderAIBackend
	var readerInboxAI service.ReaderInboxAIBackend
	if analyzerSvc != nil && analyzerSvc.Available() {
		readerAI = analyzerSvc
		readerInboxAI = analyzerSvc
	}
	readerService := service.NewReaderVNextService(layer.reader, readerAI, service.ReaderVNextServiceOptions{
		CursorSigningKey: cfg.ReaderCursorSigningKey,
	})
	readerService.ConfigureMetadataCacheInvalidator(layer.aggregateCacheInvalidator())
	var linkEmbeddingBackfill interface {
		Run(context.Context) (filled int, failed int, skipped int, err error)
	}
	if embedder != nil && embedder.Enabled() {
		linkEmbeddingBackfill = service.NewLinkBackfiller(service.LinkBackfillOptions{
			Store:    layer.links,
			Embedder: embedder,
			Logger:   layer.logger,
		})
	}
	var (
		queue          *worker.RiverQueue
		queueOwnership runtimeAcquiredResource
	)
	if err := acquisitions.Acquire(runtimeBuildRiverQueue, func() (runtimeAcquiredResource, error) {
		var err error
		queue, err = buildQueueWithLinkEmbeddingBackfill(cfg, layer, pipeline, translationProcessor, linkEmbeddingBackfill, readerService)
		queueOwnership = newRuntimeBackgroundAcquiredResource(queue)
		return queueOwnership, err
	}); err != nil {
		return nil, err
	}
	readerService.ConfigureReaderInboxJobs(readerInboxAI, queue)
	inboxCommands := durablework.NewInboxCommands(durablework.InboxCommandsOptions{
		Transactions: layer.pool,
		Inbox:        layer.reader,
		Queue:        queue,
	})
	readerService.ConfigureReaderInboxProposalCommands(inboxCommands)
	var (
		readerInboxReconciler *worker.ReaderInboxOrphanReconciler
		readerInboxOwnership  runtimeAcquiredResource
	)
	if err := acquisitions.Acquire(runtimeBuildReaderInboxReconciler, func() (runtimeAcquiredResource, error) {
		var err error
		readerInboxReconciler, err = options.newReaderInboxOrphanReconciler(
			buildReaderInboxOrphanReconcilerOptions(layer, inboxCommands),
		)
		readerInboxOwnership = newRuntimeBackgroundAcquiredResource(readerInboxReconciler)
		return readerInboxOwnership, err
	}); err != nil {
		return nil, err
	}
	var (
		parseTerminalReconciler *worker.ParseTerminalReconciler
		parseOwnership          runtimeAcquiredResource
	)
	if err := acquisitions.Acquire(runtimeBuildParseReconciler, func() (runtimeAcquiredResource, error) {
		var err error
		parseTerminalReconciler, err = options.newParseTerminalReconciler(
			buildParseTerminalReconcilerOptions(cfg, layer, pipeline),
		)
		parseOwnership = newRuntimeBackgroundAcquiredResource(parseTerminalReconciler)
		return parseOwnership, err
	}); err != nil {
		return nil, err
	}
	var (
		translationTerminalReconciler *worker.TranslationTerminalReconciler
		translationOwnership          runtimeAcquiredResource
	)
	if err := acquisitions.Acquire(runtimeBuildTranslationReconciler, func() (runtimeAcquiredResource, error) {
		var err error
		translationTerminalReconciler, err = options.newTranslationTerminalReconciler(
			buildTranslationTerminalReconcilerOptions(cfg, layer),
		)
		translationOwnership = newRuntimeBackgroundAcquiredResource(translationTerminalReconciler)
		return translationOwnership, err
	}); err != nil {
		return nil, err
	}
	var (
		sitePayloadCleaner *worker.SitePayloadCleaner
		siteCleanup        runtimeAcquiredResource
	)
	if err := acquisitions.Acquire(runtimeBuildSitePayloadCleaner, func() (runtimeAcquiredResource, error) {
		var err error
		sitePayloadCleaner, err = worker.NewSitePayloadCleaner(worker.SitePayloadCleanerOptions{
			Pool: layer.pool, Logger: layer.logger, Metrics: layer.metrics,
		})
		siteCleanup = newRuntimeBackgroundAcquiredResource(sitePayloadCleaner)
		return siteCleanup, err
	}); err != nil {
		return nil, err
	}
	var (
		siteEmbeddingBackfill *worker.SiteEmbeddingBackfillWorker
		siteEmbeddingCleanup  runtimeAcquiredResource
	)
	if err := acquisitions.Acquire(runtimeBuildSiteEmbeddingBackfill, func() (runtimeAcquiredResource, error) {
		var err error
		siteEmbeddingBackfill, err = worker.NewSiteEmbeddingBackfillWorker(worker.SiteEmbeddingBackfillWorkerOptions{
			Runner: service.NewSiteEmbeddingBackfiller(service.SiteEmbeddingBackfillOptions{
				Store: layer.sites, Embedder: embedder, Logger: layer.logger,
			}),
			Logger: layer.logger,
		})
		siteEmbeddingCleanup = newRuntimeBackgroundAcquiredResource(siteEmbeddingBackfill)
		return siteEmbeddingCleanup, err
	}); err != nil {
		return nil, err
	}

	// Phase 13 (v4.0 M2): expose queue depth as webtag_queue_jobs_pending,
	// sourced live from river_job (the retired in-memory QueueInFlight gauge
	// had no producer). Registered here where both metrics + pool are in scope.
	registerQueuePendingGauge(layer.metrics, layer.pool)
	registerRiverCapacityGauges(
		layer.metrics,
		layer.pool,
		riverTerminalRetention(cfg.RiverTerminalRetentionMS),
	)
	registerSitePayloadCleanupBacklogGauge(layer.metrics, layer.pool)
	registerReaderInboxOrphanBacklogGauge(layer.metrics, layer.reader)
	registerLibraryReviewItemsGauge(layer.metrics, layer.pool)

	var services runtimeServices
	if err := acquisitions.Acquire(runtimeBuildFeedScheduler, func() (runtimeAcquiredResource, error) {
		var err error
		services, err = httpClients.buildRuntimeServices(
			cfg,
			layer,
			queue,
			embedder,
			stack.manager,
			stack.fetchClient,
			analyzerSvc,
			readerService,
			runtimeFeatureResources{
				inboxCommands: inboxCommands,
				linkBackgrounds: linkFeatureBackgrounds{
					riverQueue: queue, parseReconciler: parseTerminalReconciler,
					translationReconciler: translationTerminalReconciler,
				},
				linkCleanup: linkFeatureCleanupOwnership{
					riverQueue: queueOwnership, parseReconciler: parseOwnership,
					translationReconciler: translationOwnership,
				},
				siteBackgrounds: siteFeatureBackgrounds{
					payloadCleaner: sitePayloadCleaner, embeddingBackfiller: siteEmbeddingBackfill,
				},
				siteCleanup: siteFeatureCleanupOwnership{
					payloadCleaner: siteCleanup, embeddingBackfiller: siteEmbeddingCleanup,
				},
			},
			options,
		)
		return services.feeds.cleanup.scheduler, err
	}); err != nil {
		return nil, err
	}

	// Phase 13 (v4.0 M2): idempotency cache 下沉 PG（多副本就绪）。进程内 LRU
	// 退役，改用 idempotency_keys 表 + Acquire/Store/Delete。后台清理 goroutine 的
	// Stop 挂进 Runtime.Close。
	var idempotencyCache *middleware.PGIdempotencyCache
	if err := acquisitions.Acquire(runtimeBuildIdempotencyCache, func() (runtimeAcquiredResource, error) {
		if !cfg.IdempotencyEnabled {
			return runtimeAcquiredResource{}, nil
		}
		var err error
		idempotencyCache, err = middleware.NewPGIdempotencyCache(middleware.PGIdempotencyOptions{
			Store:  idempotencyStoreAdapter{repo: layer.idempotency},
			TTL:    durationMS(cfg.IdempotencyTTLMS),
			Logger: layer.logger,
		})
		return newRuntimeBackgroundAcquiredResource(idempotencyCache), err
	}); err != nil {
		return nil, err
	}
	runtimeResources := runtimeStartResources{
		httpClients:           httpClients,
		idempotencyCache:      idempotencyCache,
		riverQueue:            services.links.backgrounds.riverQueue,
		readerInboxReconciler: readerInboxReconciler,
		parseReconciler:       services.links.backgrounds.parseReconciler,
		translationReconciler: services.links.backgrounds.translationReconciler,
		feedScheduler:         services.feeds.backgrounds.scheduler,
		sitePayloadCleaner:    services.sites.backgrounds.payloadCleaner,
		siteEmbeddingBackfill: services.sites.backgrounds.embeddingBackfiller,
	}
	var (
		router        *gin.Engine
		rateLimitCtrl *middleware.RateLimiterController
	)
	routerDependencies, err := acquisitions.BindRouterDependencies(newRuntimeRouterDependencies(
		runtimeResources.idempotencyCache.(*middleware.PGIdempotencyCache),
	))
	if err != nil {
		return nil, err
	}
	if err := acquisitions.Acquire(runtimeBuildRateLimiter, func() (runtimeAcquiredResource, error) {
		var err error
		router, rateLimitCtrl, err = routerDependencies.buildRuntimeRouter(cfg, layer, services)
		return newRuntimeBackgroundAcquiredResource(rateLimitCtrl), err
	}); err != nil {
		return nil, err
	}
	runtimeResources.rateLimiter = rateLimitCtrl

	var migrationWorker *worker.HistoricalMigrationWorker
	if err := acquisitions.Acquire(runtimeBuildHistoricalMigration, func() (runtimeAcquiredResource, error) {
		if !cfg.SiteMigrationEnabled {
			return runtimeAcquiredResource{}, nil
		}
		var err error
		migrationWorker, err = worker.NewHistoricalMigrationWorker(worker.HistoricalMigrationWorkerOptions{
			Runner: service.NewHistoricalMigrationRunner(layer.links), BatchSize: cfg.SiteMigrationBatch,
			DryRun: cfg.SiteMigrationDryRun, Interval: time.Duration(cfg.SiteMigrationIntervalMS) * time.Millisecond,
			Logger: layer.logger,
		})
		return newRuntimeBackgroundAcquiredResource(migrationWorker), err
	}); err != nil {
		return nil, err
	}
	runtimeResources.historicalMigration = migrationWorker
	if err := acquisitions.BindStartResources(&runtimeResources); err != nil {
		return nil, err
	}
	backgrounds := options.startInventory(runtimeResources)
	lifecycle := newRuntimeLifecycle(runtimeLifecycleOptions{
		backgrounds:    backgrounds,
		cleanupTimeout: defaultRuntimeCleanupTimeout,
		persistence:    layer,
		tracerShutdown: tracerShutdown,
	})
	return &Runtime{
		Router: router,
		start:  lifecycle.Start,
		close:  lifecycle.Close,
	}, nil
}

func buildReaderInboxOrphanReconcilerOptions(
	layer *persistenceLayer,
	repairer service.InboxProposalOrphanRepairer,
) worker.ReaderInboxOrphanReconcilerOptions {
	return worker.ReaderInboxOrphanReconcilerOptions{
		Repairer: repairer,
		Logger:   layer.logger,
		Metrics:  layer.metrics,
	}
}

func buildParseTerminalReconcilerOptions(
	cfg config.Config,
	layer *persistenceLayer,
	pipeline worker.ParseTerminalProjector,
) worker.ParseTerminalReconcilerOptions {
	return worker.ParseTerminalReconcilerOptions{
		Pool:         layer.pool,
		Projector:    pipeline,
		Interval:     durationMS(cfg.ParseReconcileIntervalMS),
		BatchSize:    cfg.ParseReconcileBatch,
		RoundTimeout: durationMS(cfg.ParseReconcileRoundTimeoutMS),
		MissingAfter: durationMS(cfg.ParseReconcileMissingAfterMS),
		Logger:       layer.logger,
	}
}

func buildTranslationTerminalReconcilerOptions(
	cfg config.Config,
	layer *persistenceLayer,
) worker.TranslationTerminalReconcilerOptions {
	return worker.TranslationTerminalReconcilerOptions{
		Pool:           layer.pool,
		Projector:      layer.translations,
		LegacyAttempts: layer.translations,
		Interval:       durationMS(cfg.TranslationReconcileIntervalMS),
		BatchSize:      cfg.TranslationReconcileBatch,
		RoundTimeout:   durationMS(cfg.TranslationReconcileRoundTimeoutMS),
		MissingAfter:   durationMS(cfg.TranslationReconcileMissingAfterMS),
		Logger:         layer.logger,
		Metrics:        layer.metrics,
	}
}

func buildTranslationProcessor(
	layer *persistenceLayer,
	translatorService translator.Translator,
) *linktranslation.Processor {
	return linktranslation.NewProcessor(linktranslation.ProcessorOptions{
		Translations: layer.translations,
		Translator:   translatorService,
		Logger:       layer.logger,
	})
}
