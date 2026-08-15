package app

import (
	"context"
	"fmt"
	"io"

	"webtag/internal/app/durablework"
	"webtag/internal/config"
	"webtag/internal/embedding"
	feedremote "webtag/internal/feed"
	"webtag/internal/fetcher"
	"webtag/internal/handler"
	"webtag/internal/service"
	"webtag/internal/service/linktranslation"
	"webtag/internal/worker"
)

// runtimeFeatureShared is the deliberately small cross-feature surface. The
// durable command adapter and URL locker are constructed once, then shared by
// link, site conversion, and feed ingest. A feature
// must not create a second transaction owner or a private lock domain.
type runtimeFeatureShared struct {
	urlLocker            service.URLLocker
	aggregateInvalidator service.MultiCacheInvalidator
	linkCommands         *durablework.LinkCommands
	inboxCommands        *durablework.InboxCommands
}

type linkFeatureRoutes struct {
	linksWrite   handler.LinkWriteService
	linksRead    handler.LinkReadService
	linksContent handler.LinkContentService
	translations handler.LinkTranslationService
	ingest       handler.IngestService
	jobs         handler.JobService
	tags         handler.TagService
	tree         handler.TreeService
}

type linkFeatureServices struct {
	submit       *service.SubmitService
	ingest       *service.IngestService
	jobRead      *service.JobReadService
	tagRead      *service.TagReadService
	treeRead     *service.TreeReadService
	linkRead     *service.LinkReadService
	linkContent  *service.ContentService
	translations *linktranslation.Service
}

type linkFeatureBackgrounds struct {
	riverQueue            *worker.RiverQueue
	parseReconciler       *worker.ParseTerminalReconciler
	translationReconciler *worker.TranslationTerminalReconciler
}

type linkFeatureCleanupOwnership struct {
	riverQueue            runtimeAcquiredResource
	parseReconciler       runtimeAcquiredResource
	translationReconciler runtimeAcquiredResource
}

type linkFeature struct {
	routes      linkFeatureRoutes
	services    linkFeatureServices
	backgrounds linkFeatureBackgrounds
	cleanup     linkFeatureCleanupOwnership
}

type linkFeatureOptions struct {
	config       config.Config
	layer        *persistenceLayer
	queue        *worker.RiverQueue
	embedder     *embedding.Client
	fetchManager *fetcher.Manager
	shared       runtimeFeatureShared
	backgrounds  linkFeatureBackgrounds
	cleanup      linkFeatureCleanupOwnership
}

// buildLinkFeature constructs services only. Its backgrounds were acquired by
// the composition root and remain stopped until Runtime.Start.
func buildLinkFeature(options linkFeatureOptions) (linkFeature, error) {
	cfg := options.config
	layer := options.layer
	shared := options.shared
	var inboxWriter service.InboxCaptureWriter
	readerReady, _ := readerSchemaReady(layer)
	if readerReady {
		inboxWriter = layer.reader
	}

	submitSvc, ingestSvc := service.NewLinkServices(
		layer.links,
		layer.jobs,
		shared.linkCommands,
		shared.urlLocker,
		service.SubmitServiceOptions{
			TagCacheInvalidator:     shared.aggregateInvalidator,
			DisableSiteLibraryWrite: !cfg.SiteLibraryWriteEnabled,
			InboxWriter:             inboxWriter,
			InboxJobScheduler:       options.queue,
			InboxProposalCommands:   shared.inboxCommands,
		},
	)
	jobRead := service.NewJobReadService(layer.jobs, layer.links)
	tagRead := service.NewTagReadService(layer.tags, layer.tagCache)
	treeRead := buildTreeReadService(layer)
	linkRead := service.NewLinkReadService(service.LinkReadServiceOptions{
		Links:            layer.links,
		TagInvalidator:   shared.aggregateInvalidator,
		CursorSigningKey: cfg.CursorSigningKey,
		QueryEmbedder:    options.embedder,
		Logger:           layer.logger,
		ConceptExporter:  layer.concepts,
		ContentReader:    layer.links,
		DeleteCommands:   shared.linkCommands,
		MutationLocker:   shared.urlLocker,
	})
	linkContent := service.NewContentService(layer.links, options.fetchManager, layer.logger)
	translationScheduler := durablework.NewTranslationScheduler(
		translationSchedulerOptionsFromConfig(cfg, layer, options.queue),
	)
	translations := linktranslation.NewService(linktranslation.ServiceOptions{
		Translations:   layer.translations,
		Scheduler:      translationScheduler,
		RequestTimeout: durationMS(cfg.Analyzer.RequestTimeoutMS),
	})
	services := linkFeatureServices{
		submit:       submitSvc,
		ingest:       ingestSvc,
		jobRead:      jobRead,
		tagRead:      tagRead,
		treeRead:     treeRead,
		linkRead:     linkRead,
		linkContent:  linkContent,
		translations: translations,
	}
	feature := linkFeature{
		services:    services,
		backgrounds: options.backgrounds,
		cleanup:     options.cleanup,
		routes: linkFeatureRoutes{
			linksWrite:   services.submit,
			linksRead:    services.linkRead,
			linksContent: services.linkContent,
			translations: services.translations,
			ingest:       services.ingest,
			jobs:         services.jobRead,
			tags:         services.tagRead,
			tree:         services.treeRead,
		},
	}
	if err := feature.validateOwnership(); err != nil {
		return feature, err
	}
	return feature, nil
}

func (f linkFeature) validateOwnership() error {
	checks := []struct {
		name       string
		background runtimeManagedBackground
		cleanup    runtimeAcquiredResource
	}{
		{runtimeBuildRiverQueue, f.backgrounds.riverQueue, f.cleanup.riverQueue},
		{runtimeBuildParseReconciler, f.backgrounds.parseReconciler, f.cleanup.parseReconciler},
		{runtimeBuildTranslationReconciler, f.backgrounds.translationReconciler, f.cleanup.translationReconciler},
	}
	for _, check := range checks {
		if err := featureOwnsBackground(check.name, check.background, check.cleanup); err != nil {
			return err
		}
	}
	return nil
}

type siteFeatureRoutes struct {
	sites          handler.SiteReadService
	siteManagement handler.SiteManagementService
	siteMerge      handler.SiteMergeService
	siteSplit      handler.SiteSplitService
	archiveV2      interface {
		ValidateSelection(service.ArchiveV2Selection) error
		ExportSelected(context.Context, io.Writer, service.ArchiveV2ExportOptions) error
	}
	classificationRules handler.ClassificationRuleService
	libraryReviews      handler.LibraryReviewService
	librarySearch       handler.LibrarySearchService
	conversionPreview   handler.LinkConversionPreviewService
	conversionExecute   handler.LinkConversionExecuteService
}

type siteFeatureServices struct {
	siteRead            *service.SiteReadService
	siteManagement      *service.SiteManagementService
	siteMerge           *service.SiteMergeService
	siteSplit           *service.SiteSplitService
	archiveV2           *service.ArchiveV2Service
	classificationRules *service.ClassificationRuleService
	libraryReviews      *service.LibraryReviewService
	librarySearch       *service.LibrarySearchService
	conversionPreview   *service.ConversionPreviewService
	conversionExecute   *service.ConversionExecuteService
}

type siteFeatureBackgrounds struct {
	payloadCleaner      *worker.SitePayloadCleaner
	embeddingBackfiller *worker.SiteEmbeddingBackfillWorker
}

type siteFeatureCleanupOwnership struct {
	payloadCleaner      runtimeAcquiredResource
	embeddingBackfiller runtimeAcquiredResource
}

type siteFeature struct {
	routes      siteFeatureRoutes
	services    siteFeatureServices
	backgrounds siteFeatureBackgrounds
	cleanup     siteFeatureCleanupOwnership
}

type siteFeatureOptions struct {
	config      config.Config
	layer       *persistenceLayer
	embedder    *embedding.Client
	link        linkFeature
	shared      runtimeFeatureShared
	backgrounds siteFeatureBackgrounds
	cleanup     siteFeatureCleanupOwnership
}

func buildSiteFeature(options siteFeatureOptions) (siteFeature, error) {
	cfg := options.config
	layer := options.layer
	var librarySearch *service.LibrarySearchService
	readerReady, _ := readerSchemaReady(layer)
	if readerReady {
		librarySearch = service.NewLibrarySearchServiceWithMetricsAndOptions(
			layer.links, layer.sites, options.embedder, layer.metrics,
			service.LibrarySearchServiceOptions{CursorSigningKey: cfg.ReaderCursorSigningKey},
			layer.reader,
		)
	} else {
		librarySearch = service.NewLibrarySearchServiceWithMetricsAndOptions(
			layer.links, layer.sites, options.embedder, layer.metrics,
			service.LibrarySearchServiceOptions{CursorSigningKey: cfg.ReaderCursorSigningKey},
		)
	}
	services := siteFeatureServices{
		siteRead:            service.NewSiteReadService(layer.sites),
		siteManagement:      service.NewSiteManagementService(layer.sites, layer.sites),
		siteMerge:           service.NewSiteMergeServiceWithMetrics(layer.sites, layer.metrics),
		siteSplit:           service.NewSiteSplitServiceWithMetrics(layer.sites, layer.metrics),
		classificationRules: service.NewClassificationRuleService(layer.classificationRules),
		libraryReviews:      service.NewLibraryReviewService(layer.libraryReviews, layer.links),
		librarySearch:       librarySearch,
		conversionPreview:   service.NewConversionPreviewService(layer.links, layer.links, layer.translations, layer.sites),
		conversionExecute: service.NewConversionExecuteServiceWithOptions(service.ConversionExecuteServiceOptions{
			Links: layer.links, Commands: options.shared.linkCommands, Metrics: layer.metrics,
			DisableSiteLibraryWrite: !cfg.SiteLibraryWriteEnabled,
		}),
	}
	var readerArchive service.ReaderArchiveExporter
	if readerReady {
		readerArchive = service.NewReaderArchiveExporter(layer.reader)
	}
	services.archiveV2 = service.NewArchiveV2Service(
		options.link.services.linkRead,
		layer.sites,
		layer.classificationRules,
	).WithReaderArchive(readerArchive)
	feature := siteFeature{
		services:    services,
		backgrounds: options.backgrounds,
		cleanup:     options.cleanup,
		routes: siteFeatureRoutes{
			sites:               services.siteRead,
			siteManagement:      services.siteManagement,
			siteMerge:           services.siteMerge,
			siteSplit:           services.siteSplit,
			archiveV2:           services.archiveV2,
			classificationRules: services.classificationRules,
			libraryReviews:      services.libraryReviews,
			librarySearch:       services.librarySearch,
			conversionPreview:   services.conversionPreview,
			conversionExecute:   services.conversionExecute,
		},
	}
	checks := []struct {
		name       string
		background runtimeManagedBackground
		cleanup    runtimeAcquiredResource
	}{
		{runtimeBuildSitePayloadCleaner, feature.backgrounds.payloadCleaner, feature.cleanup.payloadCleaner},
		{runtimeBuildSiteEmbeddingBackfill, feature.backgrounds.embeddingBackfiller, feature.cleanup.embeddingBackfiller},
	}
	for _, check := range checks {
		if err := featureOwnsBackground(check.name, check.background, check.cleanup); err != nil {
			return feature, err
		}
	}
	return feature, nil
}

type feedFeatureRoutes struct {
	feeds handler.FeedService
}

type feedFeatureServices struct {
	feeds *service.FeedService
}

type feedFeatureBackgrounds struct {
	scheduler *worker.FeedScheduler
}

type feedFeatureCleanupOwnership struct {
	scheduler runtimeAcquiredResource
}

type feedFeature struct {
	routes      feedFeatureRoutes
	services    feedFeatureServices
	backgrounds feedFeatureBackgrounds
	cleanup     feedFeatureCleanupOwnership
}

type feedFeatureOptions struct {
	layer    *persistenceLayer
	feedHTTP *fetcher.HTTPClient
	link     linkFeature
	shared   runtimeFeatureShared
}

func buildFeedFeature(options feedFeatureOptions) (feedFeature, error) {
	feedSvc := service.NewFeedService(service.FeedServiceOptions{
		Store:    options.layer.feeds,
		Remote:   feedremote.NewRemote(options.feedHTTP, feedremote.NewParser()),
		Analyzer: options.link.services.ingest,
		Locker:   options.shared.urlLocker,
		Logger:   options.layer.logger,
	})
	scheduler := worker.NewFeedScheduler(worker.FeedSchedulerOptions{
		Claims: options.layer.feeds, Refresher: feedSvc, Logger: options.layer.logger,
	})
	cleanup := newRuntimeBackgroundAcquiredResource(scheduler)
	return feedFeature{
		routes:      feedFeatureRoutes{feeds: feedSvc},
		services:    feedFeatureServices{feeds: feedSvc},
		backgrounds: feedFeatureBackgrounds{scheduler: scheduler},
		cleanup:     feedFeatureCleanupOwnership{scheduler: cleanup},
	}, nil
}

func featureOwnsBackground(
	name string,
	background runtimeManagedBackground,
	cleanup runtimeAcquiredResource,
) error {
	identity, _, bound := cleanup.resolveOwnership()
	if !bound {
		return fmt.Errorf("build %s feature: cleanup ownership is not bound", name)
	}
	if !sameRuntimeResourceInstance(identity, background) {
		return fmt.Errorf("build %s feature: background and cleanup refer to different instances", name)
	}
	return nil
}

func newRuntimeFeatureShared(
	cfg config.Config,
	layer *persistenceLayer,
	queue *worker.RiverQueue,
) runtimeFeatureShared {
	if layer != nil && layer.reader != nil && queue != nil {
		layer.reader.BindLinkLifecycleQueue(queue)
	}
	return runtimeFeatureShared{
		urlLocker:            buildURLLocker(cfg, layer.pool),
		aggregateInvalidator: layer.aggregateCacheInvalidator(),
		linkCommands: durablework.NewLinkCommands(durablework.LinkCommandsOptions{
			Transactions: layer.pool,
			Links:        layer.links,
			Queue:        queue,
		}),
		inboxCommands: durablework.NewInboxCommands(durablework.InboxCommandsOptions{
			Transactions: layer.pool,
			Inbox:        layer.reader,
			Queue:        queue,
		}),
	}
}
