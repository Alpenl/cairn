package app

import (
	"context"
	"io"

	"webtag/internal/app/durablework"
	"webtag/internal/config"
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
	urlLocker     service.URLLocker
	linkCommands  *durablework.LinkCommands
	inboxCommands *durablework.InboxCommands
}

type linkFeatureRoutes struct {
	linksWrite   handler.LinkWriteService
	linksRead    handler.LinkReadService
	linksContent handler.LinkContentService
	translations handler.LinkTranslationService
	ingest       handler.IngestService
	tags         handler.TagService
	tree         handler.TreeService
}

type linkFeatureServices struct {
	submit       *service.SubmitService
	ingest       *service.IngestService
	tagRead      *service.TagReadService
	treeRead     *service.TreeReadService
	linkRead     *service.LinkReadService
	linkContent  *service.ContentService
	translations *linktranslation.Service
}

type linkFeatureBackgrounds struct {
	riverQueue *worker.RiverQueue
}

type linkFeature struct {
	routes      linkFeatureRoutes
	services    linkFeatureServices
	backgrounds linkFeatureBackgrounds
}

type linkFeatureOptions struct {
	config       config.Config
	layer        *persistenceLayer
	queue        *worker.RiverQueue
	fetchManager *fetcher.Manager
	shared       runtimeFeatureShared
	backgrounds  linkFeatureBackgrounds
}

// buildLinkFeature constructs services only. Its backgrounds were acquired by
// the composition root and remain stopped until Runtime.Start.
func buildLinkFeature(options linkFeatureOptions) linkFeature {
	cfg := options.config
	layer := options.layer
	shared := options.shared

	submitSvc, ingestSvc := service.NewLinkServices(
		layer.links,
		shared.linkCommands,
		shared.urlLocker,
		service.SubmitServiceOptions{
			InboxWriter:           layer.reader,
			InboxProposalCommands: shared.inboxCommands,
		},
	)
	tagRead := service.NewTagReadService(layer.tags)
	treeRead := buildTreeReadService(layer)
	linkRead := service.NewLinkReadService(service.LinkReadServiceOptions{
		Links:            layer.links,
		CursorSigningKey: cfg.CursorSigningKey,
		ContentReader:    layer.links,
		DeleteCommands:   shared.linkCommands,
		MutationLocker:   shared.urlLocker,
	})
	linkContent := service.NewContentService(layer.links, options.fetchManager, layer.logger)
	translationScheduler := durablework.NewTranslationScheduler(
		durablework.TranslationSchedulerOptions{
			Transactions: layer.pool,
			Products:     layer.translations,
			Queue:        options.queue,
		},
	)
	translations := linktranslation.NewService(linktranslation.ServiceOptions{
		Translations:   layer.translations,
		Scheduler:      translationScheduler,
		RequestTimeout: durationMS(cfg.Analyzer.RequestTimeoutMS),
	})
	services := linkFeatureServices{
		submit:       submitSvc,
		ingest:       ingestSvc,
		tagRead:      tagRead,
		treeRead:     treeRead,
		linkRead:     linkRead,
		linkContent:  linkContent,
		translations: translations,
	}
	feature := linkFeature{
		services:    services,
		backgrounds: options.backgrounds,
		routes: linkFeatureRoutes{
			linksWrite:   services.submit,
			linksRead:    services.linkRead,
			linksContent: services.linkContent,
			translations: services.translations,
			ingest:       services.ingest,
			tags:         services.tagRead,
			tree:         services.treeRead,
		},
	}
	return feature
}

type siteFeatureRoutes struct {
	sites          handler.SiteReadService
	siteManagement handler.SiteManagementService
	siteMerge      handler.SiteMergeService
	siteSplit      handler.SiteSplitService
	archiveV2      interface {
		Export(context.Context, io.Writer, service.ArchiveV2ExportOptions) error
	}
	librarySearch     handler.LibrarySearchService
	conversionPreview handler.LinkConversionPreviewService
	conversionExecute handler.LinkConversionExecuteService
}

type siteFeatureServices struct {
	siteRead          *service.SiteReadService
	siteManagement    *service.SiteManagementService
	siteMerge         *service.SiteMergeService
	siteSplit         *service.SiteSplitService
	archiveV2         *service.ArchiveV2Service
	librarySearch     *service.LibrarySearchService
	conversionPreview *service.ConversionPreviewService
	conversionExecute *service.ConversionExecuteService
}

type siteFeatureBackgrounds struct {
	payloadCleaner *worker.SitePayloadCleaner
}

type siteFeature struct {
	routes      siteFeatureRoutes
	services    siteFeatureServices
	backgrounds siteFeatureBackgrounds
}

type siteFeatureOptions struct {
	config      config.Config
	layer       *persistenceLayer
	link        linkFeature
	shared      runtimeFeatureShared
	backgrounds siteFeatureBackgrounds
}

func buildSiteFeature(options siteFeatureOptions) siteFeature {
	cfg := options.config
	layer := options.layer
	librarySearch := service.NewLibrarySearchService(
		layer.links, layer.sites, layer.reader,
		service.LibrarySearchServiceOptions{CursorSigningKey: cfg.CursorSigningKey},
	)
	services := siteFeatureServices{
		siteRead:          service.NewSiteReadService(layer.sites),
		siteManagement:    service.NewSiteManagementService(layer.sites, layer.sites),
		siteMerge:         service.NewSiteMergeService(layer.sites),
		siteSplit:         service.NewSiteSplitService(layer.sites),
		librarySearch:     librarySearch,
		conversionPreview: service.NewConversionPreviewService(layer.links, layer.links, layer.translations, layer.sites),
		conversionExecute: service.NewConversionExecuteService(layer.links, options.shared.linkCommands),
	}
	services.archiveV2 = service.NewArchiveV2Service(
		options.link.services.linkRead,
		layer.sites,
		layer.reader,
	)
	feature := siteFeature{
		services:    services,
		backgrounds: options.backgrounds,
		routes: siteFeatureRoutes{
			sites:             services.siteRead,
			siteManagement:    services.siteManagement,
			siteMerge:         services.siteMerge,
			siteSplit:         services.siteSplit,
			archiveV2:         services.archiveV2,
			librarySearch:     services.librarySearch,
			conversionPreview: services.conversionPreview,
			conversionExecute: services.conversionExecute,
		},
	}
	return feature
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

type feedFeature struct {
	routes      feedFeatureRoutes
	services    feedFeatureServices
	backgrounds feedFeatureBackgrounds
}

type feedFeatureOptions struct {
	layer    *persistenceLayer
	feedHTTP *fetcher.HTTPClient
	link     linkFeature
	shared   runtimeFeatureShared
}

func buildFeedFeature(options feedFeatureOptions) feedFeature {
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
	return feedFeature{
		routes:      feedFeatureRoutes{feeds: feedSvc},
		services:    feedFeatureServices{feeds: feedSvc},
		backgrounds: feedFeatureBackgrounds{scheduler: scheduler},
	}
}

func newRuntimeFeatureShared(
	layer *persistenceLayer,
	queue *worker.RiverQueue,
	inboxCommands *durablework.InboxCommands,
) runtimeFeatureShared {
	return runtimeFeatureShared{
		urlLocker:     buildURLLocker(),
		linkCommands:  durablework.NewLinkCommands(layer.pool, layer.links, queue),
		inboxCommands: inboxCommands,
	}
}
