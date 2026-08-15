package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/concept"
	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/repository"
	"webtag/internal/service/analyzer"
	"webtag/internal/service/urlmeta"
	"webtag/internal/textutil"
)

const maxAnalyzerExistingTags = 50

// collectionFinalizer owns the terminal 收藏去向 transition after analysis.
// Pipeline orchestration crosses this seam once with an analyzed link; all
// reading-vs-site decisions, rollout gates, parent lookup, terminal writes,
// cache invalidation, metrics, and best-effort enrichment stay behind it.
type collectionFinalizer struct {
	tree                          AncestorLinkLookup
	tagCacheInvalidator           CacheInvalidator
	metrics                       *observability.Metrics
	logger                        *slog.Logger
	conceptAttacher               ConceptAttacher
	contentEmbedder               RetrievalEmbedder
	linkEmbeddingWriter           LinkEmbeddingWriter
	siteCompleter                 repository.SiteParseCompleter
	readingCompleter              repository.ReadingParseCompleter
	intentReader                  requestedLibraryIntentReader
	disableSiteAutoClassification bool
}

type requestedLibraryIntentReader interface {
	GetParseInputByID(context.Context, uuid.UUID) (*repository.LinkParseInput, error)
}

type collectionFinalizationRequest struct {
	LinkID                   uuid.UUID
	JobID                    uuid.UUID
	ExpectedMetadataRevision int64
	URL                      string
	URLMeta                  urlmeta.URLMetadata
	Content                  fetcher.Content
	Result                   analyzer.AnalysisResult
	Candidates               []concept.CandidateConcept
	FetcherType              string
	RequestedKind            model.RequestedLibraryKind
	RequestedSource          model.RequestedLibraryKindSource
	// CurrentKind / CurrentLocked 是链接当前已落库的归属与锁定标志，供
	// decideFinalLibrary 让路给用户裁决。
	CurrentKind   model.LibraryKind
	CurrentLocked bool
	RuleMatch     *ClassificationRuleMatch
	RecordFail    func(stage string, err error)
}

func (p *ParsePipeline) collectionFinalizer() collectionFinalizer {
	if p == nil {
		return collectionFinalizer{}
	}
	return collectionFinalizer{
		tree:                          p.tree,
		tagCacheInvalidator:           p.tagCacheInvalidator,
		metrics:                       p.metrics,
		logger:                        p.logger,
		conceptAttacher:               p.conceptAttacher,
		contentEmbedder:               p.contentEmbedder,
		linkEmbeddingWriter:           p.linkEmbeddingWriter,
		siteCompleter:                 p.siteCompleter,
		readingCompleter:              p.readingCompleter,
		intentReader:                  p.links,
		disableSiteAutoClassification: p.disableSiteAutoClassification,
	}
}

func (f collectionFinalizer) Finalize(ctx context.Context, req collectionFinalizationRequest) error {
	result := req.Result
	result.Tags = f.routeTags(ctx, req.LinkID, result.Tags, req.Candidates)

	parentID, err := ensureParent(ctx, f.tree, f.metrics, req.URL)
	if err != nil {
		f.logWarn(ctx, "parent lookup unavailable; completing without parent",
			"link_id", req.LinkID.String(),
			"job_id", req.JobID.String(),
			"err", observability.SafeError(err),
		)
		parentID = nil
	}

	const maxIntentRecomputations = 4
	for attempt := 0; attempt < maxIntentRecomputations; attempt++ {
		latest := req
		if f.intentReader != nil {
			link, err := f.intentReader.GetParseInputByID(ctx, req.LinkID)
			if err != nil {
				return fmt.Errorf("reload requested library intent: %w", err)
			}
			if link == nil {
				return repository.ErrNotFound
			}
			latest.RequestedKind = requestedKindForLink(link)
			latest.RequestedSource = requestedKindSourceForLink(link)
			latest.CurrentKind = currentLibraryKind(link)
			latest.CurrentLocked = link.LibraryKindLocked
		}

		decision := decideFinalLibrary(finalLibraryInput{
			AnalyzedKind:                  result.LibraryKind,
			Confidence:                    result.ClassificationConfidence,
			Reason:                        result.ClassificationReason,
			Explanation:                   result.ClassificationExplanation,
			Requested:                     latest.RequestedKind,
			RequestedSource:               latest.RequestedSource,
			Rule:                          latest.RuleMatch,
			CurrentKind:                   latest.CurrentKind,
			CurrentlyLocked:               latest.CurrentLocked,
			DisableSiteAutoClassification: f.disableSiteAutoClassification,
		})
		if err := f.persist(ctx, latest, result, decision, parentID); !errors.Is(err, repository.ErrLibraryIntentChanged) {
			return err
		}
	}
	return fmt.Errorf("finalize library classification: %w after repeated concurrent updates", repository.ErrLibraryIntentChanged)
}

func (f collectionFinalizer) persist(
	ctx context.Context,
	req collectionFinalizationRequest,
	result analyzer.AnalysisResult,
	decision finalLibraryDecision,
	parentID *uuid.UUID,
) error {
	recordFail := req.RecordFail
	if recordFail == nil {
		recordFail = func(string, error) {}
	}
	result = fillProfileGaps(result, decision, req.Content)
	sourceTitle := normalizeAnalysisTitle(req.Content)
	titleReliable := textutil.IsConciseTitle(result.Title) || textutil.IsConciseTitle(sourceTitle)
	title := textutil.ChooseTitle(result.Title, sourceTitle, result.Summary, req.Content.URL)
	lowConfidenceReason := evaluateLowConfidence(req.Content, title, titleReliable)
	lowConfidence := lowConfidenceReason != nil
	final := repository.UpdateLinkAnalysisParams{
		ID:                       req.LinkID,
		ExpectedMetadataRevision: req.ExpectedMetadataRevision,
		Title:                    stringPtr(title),
		Summary:                  stringPtr(result.Summary),
		Tags:                     append([]string{}, result.Tags...),
		FetcherType:              stringPtr(req.FetcherType),
		IsLowConfidence:          lowConfidence,
		LowConfidenceReason:      lowConfidenceReason,
		Status:                   model.LinkStatusDone,
		ErrorMsg:                 nil,
		Domain:                   stringPtr(req.URLMeta.Domain),
		ContentType:              stringPtr(string(req.URLMeta.ContentType)),
		PathDepth:                intPtr(req.URLMeta.PathDepth),
		ParentPath:               stringPtr(req.URLMeta.ParentPath),
		ParentID:                 parentID,
	}
	if decision.Kind == model.LibraryKindSite {
		if f.siteCompleter == nil {
			return fmt.Errorf("site analysis result received without site completer")
		}
		site, err := siteAggregateParams(req.LinkID, req.Content.URL, result)
		if err != nil {
			return fmt.Errorf("derive site identity: %w", err)
		}
		aggregate, err := f.siteCompleter.CompleteSiteParse(ctx, repository.CompleteSiteParseParams{
			Analysis:                           final,
			Classification:                     finalClassificationParams(req.LinkID, decision),
			Site:                               site,
			ExpectedRequestedLibraryKind:       req.RequestedKind,
			ExpectedRequestedLibraryKindSource: req.RequestedSource,
		}, req.JobID)
		if err != nil {
			if errors.Is(err, repository.ErrLibraryIntentChanged) {
				return err
			}
			recordFail("persist_site", err)
			return err
		}
		f.recordLibraryClassification(req.RequestedKind, decision.Kind, decision.Source, decision.Confidence)
		f.recordSiteAggregation(aggregate, site.IdentityKey)
		f.recordRun("success", req.FetcherType, string(req.URLMeta.ContentType))
		// 站点分支同样把链接写成 status='done' 并写入 site_tags，三份聚合
		// （域名 / 全局标签 / scoped 标签）都被改变。此前这条分支直接
		// return，一份都不失效。
		f.invalidateAggregates(ctx)
		return nil
	}
	if f.readingCompleter == nil {
		return fmt.Errorf("reading analysis result received without reading completer")
	}
	completion, err := f.readingCompleter.CompleteReadingParse(ctx, repository.CompleteReadingParseParams{
		Analysis:                           final,
		Classification:                     finalClassificationParams(req.LinkID, decision),
		ExpectedRequestedLibraryKind:       req.RequestedKind,
		ExpectedRequestedLibraryKindSource: req.RequestedSource,
		DetachSiteEntry:                    req.CurrentKind == model.LibraryKindSite,
	}, req.JobID)
	if err != nil {
		if errors.Is(err, repository.ErrLibraryIntentChanged) {
			return err
		}
		recordFail("persist_reading", err)
		return err
	}
	f.finishReading(ctx, req, result, title, lowConfidence, lowConfidenceReason, completion)
	f.recordLibraryClassification(req.RequestedKind, decision.Kind, decision.Source, decision.Confidence)
	return nil
}

// finalClassificationParams 把决策结论翻译成仓储参数。它只做字段搬运——所有
// 判断都已在 decideFinalLibrary 里完成。
// fillProfileGaps 在归属与 analyzer 侧画像不匹配时互相兜底。
//
// 两个方向，成因相同——analyzer 只会产出与它自己判断相符的那套画像字段，而最终
// 归属可能来自别处（灰度降级、用户锁定）：
//
//   - 降级为 reading：走过的是 site 画像，reading 侧的标题/摘要为空 → 用
//     SiteName / SiteIntro 补。
//   - 锁定为 site 但 analyzer 判 reading：SiteName / EntryName 全为空，而
//     siteAggregateParams 会把它们透传给 ValidateAggregateSiteParams 并被拒绝，
//     整条解析以「aggregate site: name and entry name are required」失败。
//     可达路径无需并发：用户把阅读转成站点（写 locked=true）→ 点刷新 → 模型不
//     同意 → 解析任务落 failed。「用户锁 A、模型认为 B」正是这把锁存在的唯一
//     理由，不能让它反过来把解析打死。
func fillProfileGaps(result analyzer.AnalysisResult, decision finalLibraryDecision, content fetcher.Content) analyzer.AnalysisResult {
	if decision.Kind == model.LibraryKindReading && decision.PredictedKind == model.LibraryKindSite {
		result.Title = firstNonBlank(result.Title, result.SiteName)
		result.Summary = firstNonBlank(result.Summary, result.SiteIntro)
	}
	if decision.Kind == model.LibraryKindSite && strings.TrimSpace(result.SiteName) == "" {
		result.SiteName = firstNonBlank(result.Title, normalizeAnalysisTitle(content), content.URL)
		result.SiteIntro = firstNonBlank(result.SiteIntro, result.Summary)
		result.EntryName = firstNonBlank(result.EntryName, result.SiteName)
	}
	return result
}

func finalClassificationParams(linkID uuid.UUID, decision finalLibraryDecision) repository.UpdateLibraryClassificationParams {
	predicted := decision.PredictedKind
	confidence := decision.Confidence
	return repository.UpdateLibraryClassificationParams{
		ID:                linkID,
		Kind:              decision.Kind,
		Source:            decision.Source,
		Locked:            decision.Locked,
		PredictedKind:     &predicted,
		Confidence:        &confidence,
		Reason:            stringPtr(decision.Reason),
		Explanation:       stringPtr(decision.Explanation),
		ClassifierVersion: stringPtr(libraryClassifierVersion),
	}
}

// invalidateAggregates 在解析成功落库后失效本租户的聚合缓存（标签 + 域名）。
//
// **不带任何「标签是否变化」的守卫**，这一点是刻意的。此前这里写的是
// `!sameTagSet(req.PreviousTags, result.Tags)`，那个条件只对「标签计数」这一个
// 维度成立，而解析完成真正改变的是链接的 **status（pending → done）**——
// 域名聚合（tree_repo.go 的 `WHERE status='done' GROUP BY domain`）与全局标签
// 聚合（tag_repo.go 同样带 status 过滤）都按这个字段筛选成员。
//
// 具体的失败路径：用户点「重新解析」一条已有链接 → requeueExisting 先失效缓存
// 并把 status 置回 pending（此刻该链接不在任何聚合里，重建的快照少了它）→
// 解析完成，标签与上次完全相同 → sameTagSet 为真 → 不失效 → 这条链接在侧栏的
// 标签计数与域名计数里凭空消失，直到 TTL 到期（默认 5 分钟）。
func (f collectionFinalizer) invalidateAggregates(ctx context.Context) {
	if f.tagCacheInvalidator != nil {
		f.tagCacheInvalidator.Invalidate(ctx)
	}
}

func (f collectionFinalizer) finishReading(
	ctx context.Context,
	req collectionFinalizationRequest,
	result analyzer.AnalysisResult,
	title string,
	lowConfidence bool,
	lowConfidenceReason *string,
	completion repository.CompleteReadingParseResult,
) {
	f.invalidateAggregates(ctx)
	if completion.MetadataApplied {
		// Content vectorization (Phase 9) runs after the link is "done" so the
		// link's readable state is already correct. Best-effort / fail-soft: an
		// embedding outage or write error logs a WARN and leaves the vector NULL;
		// normal startup does not scan historical rows, and the parse still succeeds.
		f.writeContentEmbedding(ctx, req.LinkID, req.JobID, title, result.Summary, req.Content.Body)
		// Concept attachment receives the exact trigger-adjusted revision that
		// authorized this tuple. Its repository write rechecks both that revision
		// and the tag array under the Link row lock, fencing a CAS that commits
		// between this terminal transaction and delayed enrichment.
		f.attachConcepts(ctx, req.LinkID, completion.MetadataRevision, result.Tags)
	} else {
		// The terminal transaction intentionally completed the parse job and its
		// classification, but a user metadata replacement won ownership of the
		// tuple. Do not derive a vector or concept edges from stale analyzer data.
		f.logInfo(ctx, "skip stale parse enrichments",
			"link_id", req.LinkID.String(),
			"job_id", req.JobID.String(),
		)
	}
	runResult := "success"
	if lowConfidence {
		runResult = "low_confidence"
	}
	f.recordRun(runResult, req.FetcherType, string(req.URLMeta.ContentType))
	if lowConfidence {
		f.recordLowConfidence(req.FetcherType, *lowConfidenceReason)
		f.logInfo(ctx, "pipeline completed with low confidence",
			"link_id", req.LinkID.String(),
			"job_id", req.JobID.String(),
			"fetcher_type", normalizeMetricLabel(req.FetcherType),
			"low_confidence_reason", *lowConfidenceReason,
		)
	}
}

// writeContentEmbedding computes and persists the link's content vector
// (title + summary + body fragment) under the current embedding model. It is
// entirely fail-soft: a disabled/unwired embedder is a silent no-op, an empty
// input is skipped, and any embedding/write failure is logged at WARN and
// swallowed — the parse contract does not depend on the vector. Missing
// vectors remain missing until the link is parsed again. LinkBackfiller remains
// available as a reusable implementation, but normal startup performs no scan.
//
// 每条不产出向量的路径都记一次 enrichment_skipped_total：既然没有对账 worker，
// 缺口至少要可数。reason="failed" 直接等于「有多少链接永久缺了向量」。
func (f collectionFinalizer) writeContentEmbedding(ctx context.Context, linkID, jobID uuid.UUID, title, summary, body string) {
	if f.linkEmbeddingWriter == nil || f.contentEmbedder == nil {
		f.recordEnrichmentSkipped(enrichmentContentEmbedding, enrichmentNotWired)
		return
	}
	if !f.contentEmbedder.Enabled() {
		f.recordEnrichmentSkipped(enrichmentContentEmbedding, enrichmentDisabled)
		return
	}
	input := buildContentEmbeddingInput(title, summary, body)
	if strings.TrimSpace(input) == "" {
		f.recordEnrichmentSkipped(enrichmentContentEmbedding, enrichmentEmptyInput)
		return
	}

	vectors, err := f.contentEmbedder.Embed(ctx, []string{input})
	if err != nil || len(vectors) != 1 || len(vectors[0]) == 0 {
		f.recordEnrichmentSkipped(enrichmentContentEmbedding, enrichmentFailed)
		f.logWarn(ctx, "link content embedding failed; vector remains unavailable",
			"link_id", linkID.String(), "err", observability.SafeError(err))
		return
	}
	if err := f.linkEmbeddingWriter.UpdateLinkEmbeddingForParse(ctx, linkID, jobID, stringPtr(title), stringPtr(summary), vectors[0], f.contentEmbedder.Model()); err != nil {
		f.recordEnrichmentSkipped(enrichmentContentEmbedding, enrichmentFailed)
		f.logWarn(ctx, "link content embedding write failed; vector remains unavailable",
			"link_id", linkID.String(), "err", observability.SafeError(err))
	}
}

// attachConcepts resolves all surface tags for the link in a single batched
// call and writes the link_concept rows. Best-effort by design: errors are
// logged but never bubble up so an enrichment outage does not invalidate a
// parse run that already wrote the link to "done".
func (f collectionFinalizer) attachConcepts(ctx context.Context, linkID uuid.UUID, expectedMetadataRevision int64, tags []string) {
	if f.conceptAttacher == nil {
		f.recordEnrichmentSkipped(enrichmentConceptAttach, enrichmentNotWired)
		return
	}
	if len(tags) == 0 {
		f.recordEnrichmentSkipped(enrichmentConceptAttach, enrichmentEmptyInput)
		return
	}
	if _, err := f.conceptAttacher.ResolveAndAttachBatch(ctx, linkID, expectedMetadataRevision, tags); err != nil {
		f.recordEnrichmentSkipped(enrichmentConceptAttach, enrichmentFailed)
		f.logWarn(ctx, "concept batch attach failed",
			"link_id", linkID.String(),
			"tag_count", len(tags),
			"err", observability.SafeError(err),
		)
	}
}

// loadExistingTags returns the de-duplicated tag list the analyzer
// consumes as ExistingTags. When p.tagCache is wired (production wiring
// in deps.go), the call funnels through the shared TagCache.Get +
// singleflight path so concurrent parse runs collapse onto one DB
// load and steady-state runs hit the in-memory snapshot. When tagCache
// is nil (pipeline tests that pass options{} without it), it falls back
// to the legacy direct ListDistinct path so no fixture needs a stub
// cache.
//
// Projection [TagCount → []string]: TagCache stores aggregated counts
// because the read API (GET /api/tags) also needs the numbers; the
// analyzer only wants the names. The prompt receives at most the first 50
// names in repository/cache order, keeping prompt size bounded without adding
// an unstable secondary ordering.
func (p *ParsePipeline) loadExistingTags(ctx context.Context) ([]string, error) {
	if p.tagCache == nil {
		tags, err := p.tags.ListDistinct(ctx)
		if err != nil {
			return nil, err
		}
		if len(tags) > maxAnalyzerExistingTags {
			tags = tags[:maxAnalyzerExistingTags]
		}
		return tags, nil
	}

	counts, err := p.tagCache.Get(ctx, func(loadCtx context.Context) ([]TagCount, error) {
		rows, loadErr := p.tags.ListCounts(loadCtx)
		if loadErr != nil {
			return nil, loadErr
		}
		// repository.TagCount and service.TagCount are shape-identical
		// but distinct types so the cache contract stays internal to
		// the service layer. The conversion is allocated once per
		// cache miss; cache hits avoid this allocation entirely.
		return mapServiceTagCounts(rows), nil
	})
	if err != nil {
		// Existing tags only steer the prompt; stale suggestions are not worth
		// hiding a refresh failure. The caller logs the error and continues with
		// an empty set so this optional context never blocks parsing.
		return nil, err
	}

	capacity := len(counts)
	if capacity > maxAnalyzerExistingTags {
		capacity = maxAnalyzerExistingTags
	}
	tags := make([]string, 0, capacity)
	for _, row := range counts {
		if len(tags) == maxAnalyzerExistingTags {
			break
		}
		tags = append(tags, row.Tag)
	}
	return tags, nil
}
