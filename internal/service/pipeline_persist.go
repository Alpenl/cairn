package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

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
// reading-vs-site decisions, parent lookup, terminal writes, and best-effort
// enrichment stay behind it.
type collectionFinalizer struct {
	tree             AncestorLinkLookup
	logger           *slog.Logger
	siteCompleter    repository.SiteParseCompleter
	readingCompleter repository.ReadingParseCompleter
	linkReader       finalizationLinkReader
}

type finalizationLinkReader interface {
	GetParseInputByID(context.Context, uuid.UUID) (*repository.LinkParseInput, error)
}

type collectionFinalizationRequest struct {
	LinkID      uuid.UUID
	Attempt     model.ParseAttempt
	URL         string
	URLMeta     urlmeta.URLMetadata
	Content     fetcher.Content
	Result      analyzer.AnalysisResult
	FetcherType string
	// CurrentKind / CurrentLocked are the Link's current collection selection.
	// Completion compares the same pair in PostgreSQL before committing.
	CurrentKind   *model.LibraryKind
	CurrentLocked bool
}

func (p *ParsePipeline) collectionFinalizer() collectionFinalizer {
	if p == nil {
		return collectionFinalizer{}
	}
	return collectionFinalizer{
		tree:             p.tree,
		logger:           p.logger,
		siteCompleter:    p.siteCompleter,
		readingCompleter: p.readingCompleter,
		linkReader:       p.links,
	}
}

func (f collectionFinalizer) Finalize(ctx context.Context, req collectionFinalizationRequest) error {
	result := req.Result
	parentID, err := ensureParent(ctx, f.tree, req.URL)
	if err != nil {
		f.logWarn(ctx, "parent lookup unavailable; completing without parent",
			"link_id", req.LinkID.String(),
			"parse_generation", req.Attempt.Generation,
			"err", observability.SafeError(err),
		)
		parentID = nil
	}

	const maxSelectionRecomputations = 4
	for attempt := 0; attempt < maxSelectionRecomputations; attempt++ {
		latest := req
		if f.linkReader != nil {
			link, err := f.linkReader.GetParseInputByID(ctx, req.LinkID)
			if err != nil {
				return fmt.Errorf("reload library selection: %w", err)
			}
			if link == nil {
				return repository.ErrNotFound
			}
			if link.ParseGeneration != req.Attempt.Generation {
				return repository.ErrParseAttemptNotRunnable
			}
			latest.CurrentKind = link.LibraryKind
			latest.CurrentLocked = link.LibraryKindLocked
		}

		decision := decideFinalLibrary(result.LibraryKind, latest.CurrentKind, latest.CurrentLocked)
		if err := f.persist(ctx, latest, result, decision, parentID); !errors.Is(err, repository.ErrLibrarySelectionChanged) {
			return err
		}
	}
	return fmt.Errorf("finalize library classification: %w after repeated concurrent updates", repository.ErrLibrarySelectionChanged)
}

func (f collectionFinalizer) persist(
	ctx context.Context,
	req collectionFinalizationRequest,
	result analyzer.AnalysisResult,
	decision finalLibraryDecision,
	parentID *uuid.UUID,
) error {
	result = fillProfileGaps(result, decision, req.Content)
	sourceTitle := normalizeAnalysisTitle(req.Content)
	titleReliable := textutil.IsConciseTitle(result.Title) || textutil.IsConciseTitle(sourceTitle)
	title := textutil.ChooseTitle(result.Title, sourceTitle, result.Summary, req.Content.URL)
	lowConfidenceReason := evaluateLowConfidence(req.Content, title, titleReliable)
	lowConfidence := lowConfidenceReason != nil
	final := repository.UpdateLinkAnalysisParams{
		ID:                       req.LinkID,
		ExpectedParseGeneration:  req.Attempt.Generation,
		ExpectedMetadataRevision: req.Attempt.ExpectedMetadataRevision,
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
		_, err = f.siteCompleter.CompleteSiteParse(ctx, repository.CompleteSiteParseParams{
			Analysis:                  final,
			Classification:            finalClassificationParams(req.LinkID, decision),
			Site:                      site,
			ExpectedLibraryKind:       req.CurrentKind,
			ExpectedLibraryKindLocked: req.CurrentLocked,
		})
		if err != nil {
			if errors.Is(err, repository.ErrLibrarySelectionChanged) {
				return err
			}
			return err
		}
		return nil
	}
	if f.readingCompleter == nil {
		return fmt.Errorf("reading analysis result received without reading completer")
	}
	_, err := f.readingCompleter.CompleteReadingParse(ctx, repository.CompleteReadingParseParams{
		Analysis:                  final,
		Classification:            finalClassificationParams(req.LinkID, decision),
		ExpectedLibraryKind:       req.CurrentKind,
		ExpectedLibraryKindLocked: req.CurrentLocked,
	})
	if err != nil {
		if errors.Is(err, repository.ErrLibrarySelectionChanged) {
			return err
		}
		return err
	}
	f.finishReading(ctx, req, lowConfidence, lowConfidenceReason)
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
	if decision.Kind == model.LibraryKindReading && result.LibraryKind == model.LibraryKindSite {
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
	return repository.UpdateLibraryClassificationParams{
		ID:     linkID,
		Kind:   decision.Kind,
		Locked: decision.Locked,
	}
}

func (f collectionFinalizer) finishReading(
	ctx context.Context,
	req collectionFinalizationRequest,
	lowConfidence bool,
	lowConfidenceReason *string,
) {
	if lowConfidence {
		f.logInfo(ctx, "pipeline completed with low confidence",
			"link_id", req.LinkID.String(),
			"parse_generation", req.Attempt.Generation,
			"fetcher_type", normalizeMetricLabel(req.FetcherType),
			"low_confidence_reason", *lowConfidenceReason,
		)
	}
}

// loadExistingTags returns the bounded de-duplicated tag list the analyzer
// consumes as ExistingTags.
func (p *ParsePipeline) loadExistingTags(ctx context.Context) ([]string, error) {
	tags, err := p.tags.ListDistinct(ctx)
	if err != nil {
		return nil, err
	}
	if len(tags) > maxAnalyzerExistingTags {
		tags = tags[:maxAnalyzerExistingTags]
	}
	return tags, nil
}
