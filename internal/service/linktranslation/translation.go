package linktranslation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"webtag/internal/contentdoc"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service/translator"
)

// Scheduler is the application-facing durable command port. The concrete
// adapter owns the database transaction, source CAS, and River
// insertion; no transaction type crosses this service boundary.
type Scheduler interface {
	Schedule(context.Context, uuid.UUID, model.TranslationRequest, time.Duration) (*model.LinkTranslation, error)
}

type TranslationListReader interface {
	ReadListSnapshot(context.Context, uuid.UUID) (*repository.TranslationListSnapshot, error)
}

type ServiceOptions struct {
	Translations TranslationListReader
	Scheduler    Scheduler
	// RequestTimeout 是单次上游翻译请求的超时（AI_REQUEST_TIMEOUT_MS）。
	// 停滞阈值由它推导，必须与装配 River 队列时用的是同一个值。
	RequestTimeout time.Duration
}

type Service struct {
	translations   TranslationListReader
	scheduler      Scheduler
	requestTimeout time.Duration
}

func NewService(opts ServiceOptions) *Service {
	return &Service{
		translations:   opts.Translations,
		scheduler:      opts.Scheduler,
		requestTimeout: opts.RequestTimeout,
	}
}

func (s *Service) Create(ctx context.Context, linkID uuid.UUID, req model.TranslationRequest) (*model.LinkTranslation, error) {
	if s.scheduler == nil {
		return nil, errors.New("translation scheduler is not configured")
	}
	return s.scheduler.Schedule(ctx, linkID, req, translationStallAfter(s.requestTimeout))
}

// translationStallAfter 由请求超时推导出「在途翻译多久算停滞」。
//
// 必须显著大于 translator.DefaultJobTimeout（River 给单条翻译 job 的执行上限），
// 否则正常的长文翻译会在跑到一半时被误判成停滞、重复入队。留两倍余量覆盖
// River 的 rescue 重跑——一条 job 可能被 rescue 后重跑，端到端耗时可以超过单次
// JobTimeout。
//
// 参数化而非写死默认值：生产的 job 超时是
// DefaultJobTimeout(cfg.Analyzer.RequestTimeoutMS) 且随配置线性缩放
// （= 40·rt + 60s），而 config 只校验 rt > 0、无上限。若阈值固定按默认 60s 推导，
// rt 超过约 121s 时阈值就会低于 job 超时，正常长文翻译被误判为停滞——慢推理
// 模型下这个配置完全正常。
func translationStallAfter(requestTimeout time.Duration) time.Duration {
	return 2 * translator.DefaultJobTimeout(requestTimeout)
}

func (s *Service) List(ctx context.Context, linkID uuid.UUID) (model.TranslationList, error) {
	snapshot, err := s.translations.ReadListSnapshot(ctx, linkID)
	if err != nil {
		return model.TranslationList{}, err
	}
	if snapshot == nil {
		return model.TranslationList{}, httperr.NewWithCode(
			http.StatusNotFound,
			httperr.CodeLinkNotFound,
			"link not found",
		)
	}
	source := snapshot.Source
	if source.LibraryKind != nil && *source.LibraryKind == model.LibraryKindSite {
		return model.TranslationList{}, httperr.NewWithCode(
			http.StatusConflict,
			httperr.CodeSiteOriginalContentForbidden,
			"website entries cannot be translated",
		)
	}
	items := append([]model.LinkTranslation(nil), snapshot.Items...)
	currentFullHash := translationListFullSourceHash(source)
	currentSummarySourceHash, currentSummaryIdentity, err := translationListSummaryIdentities(source.Summary)
	if err != nil {
		return model.TranslationList{}, err
	}
	var currentSummarySourceHashPointer *string
	if currentSummarySourceHash != "" {
		currentSummarySourceHashPointer = &currentSummarySourceHash
	}
	for i := range items {
		revisionStale := items[i].SourceContentRevision != nil &&
			*items[i].SourceContentRevision != source.ContentRevision
		fullHashStale := items[i].Scope == model.TranslationScopeFull &&
			items[i].SourceHash != currentFullHash
		summaryHashStale := summaryTranslationStale(items[i], source.Summary, currentSummaryIdentity)
		items[i].Stale = revisionStale || fullHashStale || summaryHashStale
	}
	return model.TranslationList{
		CurrentContentRevision:   source.ContentRevision,
		CurrentSummarySourceHash: currentSummarySourceHashPointer,
		Items:                    items,
	}, nil
}

func translationListFullSourceHash(source repository.TranslationSourceSnapshot) string {
	documentAvailable := source.ContentFormat == model.ContentFormatMarkdown &&
		source.ContentDocument != nil && strings.TrimSpace(*source.ContentDocument) != ""
	if source.Content == nil && !documentAvailable {
		return ""
	}
	content := model.SavedContent{Document: source.ContentDocument, Format: source.ContentFormat}
	if source.Content != nil {
		content.Text = *source.Content
	}
	text, _ := fullTranslationSource(content)
	return hashTranslationSource(text)
}

func translationListSummaryIdentities(summary *string) (string, string, error) {
	if summary == nil || strings.TrimSpace(*summary) == "" {
		return "", "", nil
	}
	projection, err := contentdoc.RenderedBlockProjection(model.ContentFormatMarkdown, *summary)
	if err != nil {
		return "", "", fmt.Errorf("project current translation summary: %w", err)
	}
	return hashTranslationSource(projection),
		contentdoc.RenderedSourceBlockPersistenceHash("summary", projection),
		nil
}

func summaryTranslationStale(item model.LinkTranslation, summary *string, currentBlockIdentity string) bool {
	if item.BlockKey != "summary" || item.SourceHash == currentBlockIdentity {
		return false
	}
	// Historical and compat summary selections persist hash(source_text). Keep
	// those rows current only while their exact UTF-16 range still matches the
	// canonical rendered summary. Verified rows use a domain-separated block
	// identity, so any block change makes them stale even when the selected text
	// survives or covered the entire old block.
	if item.SourceHash != hashTranslationSource(item.SourceText) || summary == nil {
		return true
	}
	return contentdoc.ValidateRenderedSelection(
		model.ContentFormatMarkdown,
		*summary,
		item.StartOffset,
		item.EndOffset,
		item.SourceText,
	) != nil
}

func fullTranslationSource(content model.SavedContent) (string, model.TranslationFormat) {
	if content.Format == model.ContentFormatMarkdown && content.Document != nil && strings.TrimSpace(*content.Document) != "" {
		return *content.Document, model.TranslationFormatMarkdown
	}
	return content.Text, model.TranslationFormatPlain
}

func hashTranslationSource(text string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
}
