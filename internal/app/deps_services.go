package app

import (
	"time"

	"webtag/internal/app/durablework"
	"webtag/internal/config"
	"webtag/internal/fetcher"
	"webtag/internal/handler"
	"webtag/internal/service"
	"webtag/internal/service/analyzer"
	"webtag/internal/service/linktranslation"
	"webtag/internal/service/translator"
	"webtag/internal/service/urllock"
	"webtag/internal/worker"
)

func buildParsePipeline(cfg config.Config, layer *persistenceLayer, fetchManager *fetcher.Manager, analyzerSvc *analyzer.OpenAIAnalyzer) *service.ParsePipeline {
	return service.NewParsePipeline(service.ParsePipelineOptions{
		Links:            layer.links,
		Tags:             layer.tags,
		Tree:             layer.tree,
		Fetcher:          fetchManager,
		Analyzer:         analyzerSvc,
		Logger:           layer.logger,
		URLDirect:        cfg.Fetcher.URLDirect,
		SiteCompleter:    layer.links,
		ReadingCompleter: layer.links,
	})
}

// parseJobTimeout 是单条解析 job 的执行超时（River JobTimeout）。
//
// 推导：一条解析 job 串行经历 fetch 和 AI analyze：
//   - fetch：基础抓取受 fetcher transport 超时约束；
//   - AI analyze：AI_REQUEST_TIMEOUT_MS，默认 60s；
//
// 三段加总在最坏情形约 2–3 分钟。取 5 分钟给抓取重试 / 慢上游 / GC 抖动留
// 余量，又远小于 River 默认 1 分钟会过早砍掉正常 job 的问题（默认 1 分钟连
// 一次 60s 的 AI analyze 都兜不住）。这是一个工程经验值，不是从单一配置项
// 算出的硬上界——上游真要跑过 5 分钟，应当判定为卡死交给 rescuer。
const parseJobTimeout = 5 * time.Minute

func buildRiverQueueOptions(
	cfg config.Config,
	layer *persistenceLayer,
	pipeline service.ParseProcessor,
	translationProcessor linktranslation.JobProcessor,
	readerProcessor service.ReaderInboxSummaryJobProcessor,
) worker.RiverQueueOptions {
	translationJobTimeout := translator.DefaultJobTimeout(durationMS(cfg.Analyzer.RequestTimeoutMS))
	rescueAfter := max(parseJobTimeout, translationJobTimeout) + time.Minute
	options := worker.RiverQueueOptions{
		Pool:                  layer.pool,
		Processor:             pipeline,
		TranslationProcessor:  translationProcessor,
		TranslationJobTimeout: translationJobTimeout,
		MaxWorkers:            cfg.DB.ParseConcurrency,
		JobTimeout:            parseJobTimeout,
		RescueAfter:           rescueAfter,
		TerminalJobRetention:  riverTerminalRetention(cfg.RiverTerminalRetentionMS),
		Logger:                layer.logger,
		ReaderInboxProcessor:  readerProcessor,
	}
	return options
}

func riverTerminalRetention(milliseconds int) time.Duration {
	if milliseconds == -1 {
		return -1
	}
	return durationMS(milliseconds)
}

// runtimeServices keeps feature boundaries visible all the way to route and
// lifecycle composition. It is not a registry: every consumer names the
// feature and field it needs.
type runtimeServices struct {
	links  linkFeature
	sites  siteFeature
	feeds  feedFeature
	reader *service.ReaderApplications
}

func buildRuntimeServices(
	cfg config.Config,
	layer *persistenceLayer,
	queue *worker.RiverQueue,
	fetchManager *fetcher.Manager,
	feedHTTP *fetcher.HTTPClient,
	reader *service.ReaderApplications,
	inboxCommands *durablework.InboxCommands,
	sitePayloadCleaner *worker.SitePayloadCleaner,
) runtimeServices {
	var features runtimeServices
	shared := newRuntimeFeatureShared(layer, queue, inboxCommands)
	features.reader = reader
	features.links = buildLinkFeature(linkFeatureOptions{
		config:       cfg,
		layer:        layer,
		queue:        queue,
		fetchManager: fetchManager,
		shared:       shared,
		backgrounds:  linkFeatureBackgrounds{riverQueue: queue},
	})

	features.sites = buildSiteFeature(siteFeatureOptions{
		config:      cfg,
		layer:       layer,
		link:        features.links,
		shared:      shared,
		backgrounds: siteFeatureBackgrounds{payloadCleaner: sitePayloadCleaner},
	})

	features.feeds = buildFeedFeature(feedFeatureOptions{
		layer: layer, feedHTTP: feedHTTP, link: features.links, shared: shared,
	})
	return features
}

func buildURLLocker() service.URLLocker {
	return urllock.NewInProcessURLLocker()
}

var (
	_ handler.LinkWriteService = (*service.SubmitService)(nil)
	_ handler.IngestService    = (*service.IngestService)(nil)
	_ handler.LinkReadService  = (*service.LinkReadService)(nil)
)

func buildTreeReadService(layer *persistenceLayer) *service.TreeReadService {
	return service.NewTreeReadService(layer.tree)
}
