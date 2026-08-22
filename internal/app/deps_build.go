package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/gin-gonic/gin"

	"webtag/internal/app/durablework"
	"webtag/internal/config"
	"webtag/internal/middleware"
	"webtag/internal/observability"
	"webtag/internal/service"
	"webtag/internal/service/linktranslation"
	"webtag/internal/service/translator"
	"webtag/internal/worker"
)

const (
	runtimeBuildPersistenceLayer   = "persistence"
	runtimeBuildHTTPClients        = "outbound HTTP clients"
	runtimeBuildRiverQueue         = "river queue"
	runtimeBuildSitePayloadCleaner = "site payload cleaner"
	runtimeBuildFeedScheduler      = "feed scheduler"
	runtimeBuildIdempotencyCache   = "idempotency cache"
)

// BuildRuntime is the top-level wiring entrypoint. It constructs the complete
// runtime in explicit acquisition order, registering cleanup next to every
// acquired resource so a later failure unwinds the partial graph in reverse.
// Runtime background startup is deferred until Runtime.Start.
func BuildRuntime(ctx context.Context, cfg config.Config) (*Runtime, error) {
	return buildRuntime(ctx, cfg)
}

//nolint:gocyclo // The linear composition root keeps construction next to cleanup ownership.
func buildRuntime(ctx context.Context, cfg config.Config) (built *Runtime, buildErr error) {
	var cleanup cleanupStack
	cleanupLogger := slog.Default()
	defer func() {
		if buildErr == nil {
			cleanup.actions = nil
			return
		}
		built = nil
		cleanupErr := cleanup.RunDetached(ctx, defaultRuntimeCleanupTimeout)
		if cleanupErr == nil {
			return
		}
		buildErr = errors.Join(buildErr, fmt.Errorf("runtime build cleanup: %w", cleanupErr))
		cleanupLogger.ErrorContext(
			context.WithoutCancel(ctx),
			"runtime build cleanup failed",
			"error", observability.SafeError(cleanupErr),
		)
	}()

	layer, err := openPersistenceLayer(ctx, cfg)
	if layer != nil {
		cleanup.Add(runtimeBuildPersistenceLayer, layer.Close)
	}
	if err != nil {
		return nil, fmt.Errorf("open persistence: %w", err)
	}
	if layer == nil {
		return nil, errors.New("persistence constructor returned a nil layer")
	}
	cleanupLogger = layer.logger

	warnBootConfiguration(cfg, layer.logger)

	httpClients := newRuntimeHTTPClientOwner()
	if httpClients == nil {
		return nil, errors.New("HTTP client owner constructor returned nil")
	}
	cleanup.Add(runtimeBuildHTTPClients, httpClients.Stop)
	stack := httpClients.buildFetcherStack(cfg)
	analyzerSvc := buildAnalyzer(cfg, stack.analyzerClient, stack.visionClient, layer.logger)
	translatorSvc := buildTranslator(cfg, stack.analyzerClient)

	pipeline := buildParsePipeline(cfg, layer, stack.manager, analyzerSvc)
	translationProcessor := buildTranslationProcessor(layer, translatorSvc)
	var readerAI service.ReaderAIBackend
	var readerInboxAI service.ReaderInboxAIBackend
	if analyzerSvc != nil && analyzerSvc.Available() {
		readerAI = analyzerSvc
		readerInboxAI = analyzerSvc
	}
	readerProcessor := service.NewReaderInboxSummaryProcessor(layer.reader, readerInboxAI)
	queue, err := worker.NewRiverQueue(buildRiverQueueOptions(cfg, layer, pipeline, translationProcessor, readerProcessor))
	if queue != nil {
		cleanup.Add(runtimeBuildRiverQueue, queue.Stop)
	}
	if err != nil {
		return nil, fmt.Errorf("construct River queue: %w", err)
	}
	inboxCommands := durablework.NewInboxCommands(layer.pool, layer.reader, queue)
	readerCommands := durablework.NewReaderCommands(layer.pool, layer.reader, queue)
	readerApplications := service.NewReaderApplications(service.ReaderStores{
		Thoughts: layer.reader,
		Notes:    layer.reader,
		Inbox:    layer.reader,
		Todos:    layer.reader,
		Library:  layer.reader,
		Hosts:    layer.reader,
	}, readerAI, service.ReaderApplicationOptions{
		CursorSigningKey:         cfg.CursorSigningKey,
		InboxProposalCommands:    inboxCommands,
		InboxConfirmCommands:     readerCommands,
		InboxBulkConfirmCommands: readerCommands,
		InboxAIConfirmCommands:   readerCommands,
		FeedFeedbackCommands:     readerCommands,
		HostRestoreCommands:      readerCommands,
	})

	sitePayloadCleaner, err := worker.NewSitePayloadCleaner(worker.SitePayloadCleanerOptions{
		Pool: layer.pool, Logger: layer.logger,
	})
	if sitePayloadCleaner != nil {
		cleanup.Add(runtimeBuildSitePayloadCleaner, sitePayloadCleaner.Stop)
	}
	if err != nil {
		return nil, fmt.Errorf("construct site payload cleaner: %w", err)
	}
	services := buildRuntimeServices(
		cfg,
		layer,
		queue,
		stack.manager,
		stack.fetchClient,
		readerApplications,
		inboxCommands,
		sitePayloadCleaner,
	)
	feedScheduler := services.feeds.backgrounds.scheduler
	if feedScheduler != nil {
		cleanup.Add(runtimeBuildFeedScheduler, feedScheduler.Stop)
	}

	// Phase 13 (v4.0 M2): idempotency cache 下沉 PG（多副本就绪）。进程内 LRU
	// 退役，改用 idempotency_keys 表 + Acquire/Store/Delete。后台清理 goroutine 的
	// Stop 挂进 Runtime.Close。
	idempotencyCache, err := middleware.NewPGIdempotencyCache(middleware.PGIdempotencyOptions{
		Store:  idempotencyStoreAdapter{repo: layer.idempotency},
		TTL:    durationMS(cfg.IdempotencyTTLMS),
		Logger: layer.logger,
	})
	if err != nil {
		return nil, fmt.Errorf("construct idempotency cache: %w", err)
	}
	cleanup.Add(runtimeBuildIdempotencyCache, idempotencyCache.Stop)

	var router *gin.Engine
	router, err = buildRuntimeRouter(cfg, layer, services, idempotencyCache)
	if err != nil {
		return nil, fmt.Errorf("construct HTTP router: %w", err)
	}

	backgrounds := []namedRuntimeBackground{
		{name: runtimeBuildHTTPClients, background: httpClients},
		{name: runtimeBuildIdempotencyCache, background: idempotencyCache},
		{name: runtimeBuildRiverQueue, background: services.links.backgrounds.riverQueue},
		{name: runtimeBuildFeedScheduler, background: feedScheduler},
		{name: runtimeBuildSitePayloadCleaner, background: services.sites.backgrounds.payloadCleaner},
	}
	resources := newRuntimeResources(backgrounds, layer)
	return &Runtime{
		Router: router,
		start:  resources.Start,
		close:  resources.Close,
	}, nil
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
