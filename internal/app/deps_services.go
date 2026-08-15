package app

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/app/durablework"
	"webtag/internal/concept"
	"webtag/internal/config"
	"webtag/internal/embedding"
	"webtag/internal/fetcher"
	"webtag/internal/handler"
	"webtag/internal/repository"
	"webtag/internal/service"
	"webtag/internal/service/analyzer"
	"webtag/internal/service/linktranslation"
	"webtag/internal/service/translator"
	"webtag/internal/service/urllock"
	"webtag/internal/worker"
)

// 编译期断言：service 实现满足 handler 声明的契约。断言放在装配层而非 service
// 包内——service 是 handler 的下层，不得反向导入它。装配层同时看得见两侧，是校验
// 桥接关系的唯一正确位置。
var _ handler.ReaderService = (*service.ReaderVNextService)(nil)

// buildParsePipeline assembles the parse pipeline and returns its embedding
// client so link semantic search can reuse the same hardened transport.
func (o *runtimeHTTPClientOwner) buildParsePipeline(cfg config.Config, layer *persistenceLayer, fetchManager *fetcher.Manager, analyzerSvc *analyzer.OpenAIAnalyzer) (*service.ParsePipeline, *embedding.Client) {
	embedder := o.buildEmbeddingClient(cfg, layer.metrics)
	var classificationRules service.ClassificationRuleMatcher
	if cfg.SiteAdvancedManagementEnabled {
		classificationRules = service.NewClassificationRuleMatcher(layer.classificationRules)
	}

	pipeline := service.NewParsePipeline(service.ParsePipelineOptions{
		Links:    layer.links,
		Jobs:     layer.jobs,
		Tags:     layer.tags,
		Tree:     layer.tree,
		Fetcher:  fetchManager,
		Analyzer: analyzerSvc,
		// 解析完成会把链接从 pending 变成 done，而域名聚合的 SQL 带
		// status='done' 过滤——因此它同样改变域名计数，必须一并失效。
		TagCacheInvalidator: layer.aggregateCacheInvalidator(),
		TagCache:            layer.tagCache,
		Metrics:             layer.metrics,
		Logger:              layer.logger,
		PreferLight:         cfg.Fetcher.PreferLight,
		URLDirect:           cfg.Fetcher.URLDirect,
		// Phase 9: write the link content vector at parse time (fail-soft).
		// Concept candidates are intentionally not wired: links.tags is the
		// canonical tag surface, while this embedder only serves link search.
		Embedder: embedder,
		// The link repo is the semantic-vector writer.
		// No-op when the embedder is disabled (LinkEmbeddingWriter set but the
		// pipeline guards on contentEmbedder.Enabled()).
		LinkEmbeddingWriter:           layer.links,
		SiteCompleter:                 layer.links,
		ReadingCompleter:              layer.links,
		ClassificationRules:           classificationRules,
		DisableSiteAutoClassification: !cfg.SiteAutoClassificationEnabled,
	})
	return pipeline, embedder
}

// parseJobTimeout 是单条解析 job 的执行超时（River JobTimeout）。
//
// 推导：一条解析 job 串行经历 fetch → AI analyze → embedding 写入三段：
//   - fetch：基础抓取受 fetcher transport 超时约束，且可能 light→deep 升级
//     （二次抓取）或走 ytdlp（defaultYtdlpTimeout 30s）；
//   - AI analyze：AI_REQUEST_TIMEOUT_MS，默认 60s；
//   - embedding 写入：可选的链接内容向量化，再叠一次 embedding 请求超时。
//
// 三段加总在最坏情形约 2–3 分钟。取 5 分钟给抓取重试 / 慢上游 / GC 抖动留
// 余量，又远小于 River 默认 1 分钟会过早砍掉正常 job 的问题（默认 1 分钟连
// 一次 60s 的 AI analyze 都兜不住）。这是一个工程经验值，不是从单一配置项
// 算出的硬上界——上游真要跑过 5 分钟，应当判定为卡死交给 rescuer。
const parseJobTimeout = 5 * time.Minute

func buildQueueWithLinkEmbeddingBackfill(
	cfg config.Config,
	layer *persistenceLayer,
	pipeline service.ParseProcessor,
	translationProcessor linktranslation.JobProcessor,
	backfill interface {
		Run(context.Context) (filled int, failed int, skipped int, err error)
	},
	readerProcessor ...service.ReaderInboxSummaryJobProcessor,
) (*worker.RiverQueue, error) {
	options := buildRiverQueueOptions(cfg, layer, pipeline, translationProcessor, readerProcessor...)
	options.LinkEmbeddingBackfill = backfill
	return worker.NewRiverQueue(options)
}

func buildRiverQueueOptions(
	cfg config.Config,
	layer *persistenceLayer,
	pipeline service.ParseProcessor,
	translationProcessor linktranslation.JobProcessor,
	readerProcessor ...service.ReaderInboxSummaryJobProcessor,
) worker.RiverQueueOptions {
	translationJobTimeout := translator.DefaultJobTimeout(durationMS(cfg.Analyzer.RequestTimeoutMS))
	// River's rescuer threshold is client-wide, so it must cover the largest
	// per-worker timeout — River only validates it against Config.JobTimeout and
	// is blind to per-worker Timeout() overrides. Missing one lets the rescuer
	// re-enqueue a job that is still running: a low AI_REQUEST_TIMEOUT_MS shrinks
	// translationJobTimeout below the 15-minute backfill/retention workers, and
	// the rescuer does not stop the original goroutine, so both rounds run at
	// once and repeat their paid embedding calls. The one-minute buffer avoids
	// racing job cancellation.
	rescueAfter := max(
		parseJobTimeout,
		translationJobTimeout,
		service.LinkEmbeddingBackfillJobTimeout,
		worker.ContentHistoryRetentionJobTimeout,
	) + time.Minute
	options := worker.RiverQueueOptions{
		Pool:                    layer.pool,
		Processor:               pipeline,
		TranslationProcessor:    translationProcessor,
		TranslationAttempts:     layer.translations,
		TranslationJobsRollout:  cfg.TranslationJobsRollout,
		TranslationJobTimeout:   translationJobTimeout,
		ContentHistoryRetention: repository.NewPGXReaderContentHistoryCleanupRepository(layer.pool),
		MaxWorkers:              cfg.DB.ParseConcurrency,
		JobTimeout:              parseJobTimeout,
		RescueAfter:             rescueAfter,
		TerminalJobRetention:    riverTerminalRetention(cfg.RiverTerminalRetentionMS),
		Logger:                  layer.logger,
		Metrics:                 layer.metrics,
	}
	if len(readerProcessor) > 0 {
		options.ReaderInboxProcessor = readerProcessor[0]
	}
	return options
}

func riverTerminalRetention(milliseconds int) time.Duration {
	if milliseconds == -1 {
		return -1
	}
	return durationMS(milliseconds)
}

type runtimeAdministrationServices struct {
	conceptMerges *concept.MergeAdminService
}

type runtimeFeatureResources struct {
	inboxCommands   *durablework.InboxCommands
	linkBackgrounds linkFeatureBackgrounds
	linkCleanup     linkFeatureCleanupOwnership
	siteBackgrounds siteFeatureBackgrounds
	siteCleanup     siteFeatureCleanupOwnership
}

// runtimeServices keeps feature boundaries visible all the way to route and
// lifecycle composition. It is not a registry: every consumer names the
// feature and field it needs.
type runtimeServices struct {
	links          linkFeature
	sites          siteFeature
	feeds          feedFeature
	reader         *service.ReaderVNextService
	administration runtimeAdministrationServices
}

// translationSchedulerOptionsFromConfig keeps the config-to-adapter assignment
// in a directly testable composition-root product. Load rejects unknown stages;
// the defensive != compat mapping also fails closed when a caller bypasses Load
// and constructs Config manually.
func translationSchedulerOptionsFromConfig(
	cfg config.Config,
	layer *persistenceLayer,
	queue *worker.RiverQueue,
) durablework.TranslationSchedulerOptions {
	return durablework.TranslationSchedulerOptions{
		Transactions:         layer.pool,
		Products:             layer.translations,
		Queue:                queue,
		Metrics:              layer.metrics,
		StrictSourceIdentity: cfg.TranslationSourceRollout != config.TranslationSourceRolloutCompat,
	}
}

func (o *runtimeHTTPClientOwner) buildRuntimeServices(
	cfg config.Config,
	layer *persistenceLayer,
	queue *worker.RiverQueue,
	embedder *embedding.Client,
	fetchManager *fetcher.Manager,
	feedHTTP *fetcher.HTTPClient,
	analyzerSvc *analyzer.OpenAIAnalyzer,
	reader *service.ReaderVNextService,
	resources runtimeFeatureResources,
	constructors runtimeBuildOptions,
) (features runtimeServices, err error) {
	shared := newRuntimeFeatureShared(cfg, layer, queue)
	if resources.inboxCommands != nil {
		shared.inboxCommands = resources.inboxCommands
	}
	features.reader = reader
	features.reader.ConfigureReaderInboxProposalCommands(shared.inboxCommands)
	features.links, err = constructors.buildLinkFeature(linkFeatureOptions{
		config:       cfg,
		layer:        layer,
		queue:        queue,
		embedder:     embedder,
		fetchManager: fetchManager,
		shared:       shared,
		backgrounds:  resources.linkBackgrounds,
		cleanup:      resources.linkCleanup,
	})
	if err != nil {
		return features, fmt.Errorf("build link feature: %w", err)
	}

	features.sites, err = constructors.buildSiteFeature(siteFeatureOptions{
		config:      cfg,
		layer:       layer,
		embedder:    embedder,
		link:        features.links,
		shared:      shared,
		backgrounds: resources.siteBackgrounds,
		cleanup:     resources.siteCleanup,
	})
	if err != nil {
		return features, fmt.Errorf("build site feature: %w", err)
	}

	// The scheduler is the one resource acquired by a feature constructor. Keep
	// the partially returned bundle on error so the outer acquisition can run
	// its bound cleanup before unwinding earlier resources.
	features.feeds, err = constructors.buildFeedFeature(feedFeatureOptions{
		layer: layer, feedHTTP: feedHTTP, link: features.links, shared: shared,
	})
	if err != nil {
		return features, fmt.Errorf("build feed feature: %w", err)
	}

	features.administration = runtimeAdministrationServices{
		conceptMerges: concept.NewMergeAdminService(concept.MergeAdminServiceOptions{
			Proposals: layer.conceptProposals,
			Merger:    layer.conceptProposals,
			Concepts:  layer.concepts,
		}),
	}
	registerPendingProposalsGauge(layer.metrics, layer.conceptProposals)
	return features, nil
}

func buildURLLocker(cfg config.Config, pool *pgxpool.Pool) service.URLLocker {
	if pool == nil {
		return urllock.NewInProcessURLLocker()
	}
	if cfg.DB.MaxConns < 2 {
		// A one-connection pool cannot hold a session advisory lock and run the
		// protected repository operation at the same time. The in-process fallback
		// is therefore valid only for a single application instance; config rejects
		// MODE=multi with DB_MAX_CONNS<2 so a cross-replica deployment cannot
		// silently lose distributed serialization.
		return urllock.NewInProcessURLLocker()
	}

	// Session advisory locks occupy one pool connection for the whole critical
	// section. The submission gate is capped at half the pool so every
	// lock-holding callback can still make progress on its repository query or
	// transaction instead of all connections becoming mutually waiting locks.
	gate := urllock.NewAdvisoryLockGate(cfg.DB.MaxConns / 2)
	return urllock.NewAdvisoryURLLockerWithGate(pool, urllock.AdvisoryLockClassSubmit, gate)
}

type conceptMergeHandlerAdapter struct{ svc *concept.MergeAdminService }

func (ad conceptMergeHandlerAdapter) ListPending(ctx context.Context, limit, offset int) ([]handler.ConceptMergeView, error) {
	rows, err := ad.svc.ListPendingViews(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]handler.ConceptMergeView, 0, len(rows))
	for _, v := range rows {
		out = append(out, handler.ConceptMergeView{
			ID:             v.ID,
			WinnerID:       v.WinnerID,
			WinnerName:     v.WinnerName,
			WinnerUseCount: v.WinnerUseCount,
			LoserID:        v.LoserID,
			LoserName:      v.LoserName,
			LoserUseCount:  v.LoserUseCount,
			Score:          v.Score,
			LLMReason:      v.LLMReason,
			CreatedAtUnix:  v.CreatedAtUnix,
		})
	}
	return out, nil
}

func (ad conceptMergeHandlerAdapter) Approve(ctx context.Context, id uuid.UUID, by string) error {
	return ad.svc.Approve(ctx, id, by)
}

func (ad conceptMergeHandlerAdapter) Reject(ctx context.Context, id uuid.UUID, by string) error {
	return ad.svc.Reject(ctx, id, by)
}

var (
	_ handler.LinkWriteService = (*service.SubmitService)(nil)
	_ handler.IngestService    = (*service.IngestService)(nil)
	_ handler.LinkReadService  = (*service.LinkReadService)(nil)
)

// buildTreeReadService 装配树读服务并接上域名摘要缓存。
//
// 抽成函数与 routerOptionsWithRuntimeDeps 同理：**接线那一行必须待在被断言的
// 函数体内**。此前它内联在 buildRuntimeServices 里，而后者需要真实的队列 /
// 嵌入客户端 / 抓取器才能调用，于是删掉 `.WithDomainCache(...)` 会让生产环境
// 的 /api/tree?view=domains 缓存整个消失（PF7 的头号指标退回 10→10），
// 而 Go 全量测试照样全绿。
func buildTreeReadService(layer *persistenceLayer) *service.TreeReadService {
	return service.NewTreeReadService(layer.tree, layer.metrics).
		WithDomainCache(layer.domainCache)
}
