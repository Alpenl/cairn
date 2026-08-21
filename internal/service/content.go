package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"webtag/internal/contentdoc"
	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/problem"
	"webtag/internal/repository"
)

// ContentLinkStore 是「保存原文」需要的 link 读写子集：取 link（校验存在 + 状态）、
// 写入/读取已保存原文。生产实现是 *repository.PGXLinkRepository。
type ContentLinkStore interface {
	GetParseInputByID(ctx context.Context, id uuid.UUID) (*repository.LinkParseInput, error)
	// 写方法返回 (新的 content_revision, 是否写入成功, error)。代次要一路
	// 交到响应里，客户端才不用等列表刷新就知道正文换了代。
	UpdateContentIfCurrent(ctx context.Context, id uuid.UUID, expectedUpdatedAt time.Time, content model.SavedContent) (int64, bool, error)
	ReplaceContentIfCurrentWithRevision(ctx context.Context, id uuid.UUID, expectedUpdatedAt time.Time, expectedContentRevision int64, content model.SavedContent) (int64, bool, error)
	EditContentIfRevision(ctx context.Context, id uuid.UUID, expectedRevision int64, content model.SavedContent) (int64, bool, error)
	GetContent(ctx context.Context, id uuid.UUID) (*model.SavedContent, error)
}

// ContentService implements original-content snapshots independently from
// enrichment: Save reuses an existing snapshot, while Replace explicitly
// resolves the current capture/URL again. Structured HTML is normalized to
// safe Markdown and every snapshot retains a canonical plain projection.
type ContentService struct {
	links   ContentLinkStore
	fetcher ContentFetcher
	logger  *slog.Logger
}

// NewContentService 装配「保存原文」服务。需要远程正文但 fetcher 为 nil 时，Save
// 会返回明确错误而非 panic；已有 content 和 ingest 路径不依赖 fetcher。
func NewContentService(links ContentLinkStore, fetcher ContentFetcher, logger *slog.Logger) *ContentService {
	return &ContentService{links: links, fetcher: fetcher, logger: logger}
}

// Save stores canonical content for a completed link. Existing content is
// returned idempotently. Ingest sources prefer captured HTML and fall back to
// text; plain URL sources use ContentFetcher. Fetch failures return 502.
func (s *ContentService) Save(ctx context.Context, linkID string) (dto.LinkContentResponse, error) {
	return s.persist(ctx, linkID, false)
}

// Get returns the already-saved original without ever fetching the page. It
// is the read half of the "save original" feature: the reader pulls the text
// only when the user expands the collapsed original section, so opening an
// article no longer ships the whole body with the link detail.
// 404 when the link is unknown, and when nothing has been saved yet — an
// absent snapshot is not an error state, it is "nothing to show".
func (s *ContentService) Get(ctx context.Context, linkID string) (dto.LinkContentResponse, error) {
	id, err := uuid.Parse(strings.TrimSpace(linkID))
	if err != nil {
		return dto.LinkContentResponse{}, problem.NewWithCode(problem.Malformed, problem.CodeInvalidLinkID, "invalid link id")
	}
	stored, err := s.loadStoredContent(ctx, id)
	if err != nil {
		return dto.LinkContentResponse{}, err
	}
	if stored == nil {
		return dto.LinkContentResponse{}, problem.NewWithCode(problem.NotFound, problem.CodeLinkNotFound, "saved content not found")
	}
	return *stored, nil
}

// Replace explicitly resolves and overwrites the saved original for the
// current completed source revision. It is intentionally separate from Save
// so retries from older clients remain idempotent.
func (s *ContentService) Replace(ctx context.Context, linkID string) (dto.LinkContentResponse, error) {
	return s.persist(ctx, linkID, true)
}

// Edit replaces the currently saved snapshot from caller-provided source
// text. It never invokes the fetcher. The stored format selects the parser so
// Markdown edits receive the same sanitizer and canonicalization as fetched
// documents while plain edits remain plain text.
func (s *ContentService) Edit(ctx context.Context, linkID string, request dto.ContentEditRequest) (dto.LinkContentResponse, error) {
	id, err := validateContentEditRequest(linkID, request)
	if err != nil {
		return dto.LinkContentResponse{}, err
	}
	link, stored, authoritativeRevision, err := s.loadEditableContent(ctx, id)
	if err != nil {
		return dto.LinkContentResponse{}, err
	}
	if request.ExpectedContentRevision != authoritativeRevision {
		return dto.LinkContentResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeContentRevisionConflict, "saved content changed; reload before editing")
	}

	edited, err := buildEditedContent(request.Content, link.URL, stored.Format)
	if err != nil {
		return dto.LinkContentResponse{}, err
	}

	revision, written, err := s.links.EditContentIfRevision(ctx, id, request.ExpectedContentRevision, edited)
	if err != nil {
		return dto.LinkContentResponse{}, err
	}
	if !written {
		return s.classifyEditMiss(ctx, id, request.ExpectedContentRevision)
	}
	edited.Revision = revision
	return contentResponse(id, edited, "stored"), nil
}

func validateContentEditRequest(linkID string, request dto.ContentEditRequest) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(linkID))
	if err != nil {
		return uuid.Nil, problem.NewWithCode(problem.Malformed, problem.CodeInvalidLinkID, "invalid link id")
	}
	if request.ExpectedContentRevision <= 0 {
		return uuid.Nil, problem.NewWithCode(problem.Invalid, "invalid_content_revision", "expected_content_revision must be positive")
	}
	if len([]byte(request.Content)) > 2<<20 {
		return uuid.Nil, problem.NewWithCode(problem.TooLarge, problem.CodeContentTooLarge, "saved content exceeds the 2 MiB UTF-8 limit")
	}
	return id, nil
}

func (s *ContentService) loadEditableContent(ctx context.Context, id uuid.UUID) (*repository.LinkParseInput, model.SavedContent, int64, error) {
	link, err := s.links.GetParseInputByID(ctx, id)
	if err != nil {
		return nil, model.SavedContent{}, 0, err
	}
	if link == nil {
		return nil, model.SavedContent{}, 0, problem.NewWithCode(problem.NotFound, problem.CodeLinkNotFound, "link not found")
	}
	if link.LibraryKind != nil && *link.LibraryKind == model.LibraryKindSite {
		return nil, model.SavedContent{}, 0, problem.NewWithCode(problem.Conflict, problem.CodeSiteOriginalContentForbidden, "website entries cannot edit original content")
	}
	if link.Status != model.LinkStatusDone {
		return nil, model.SavedContent{}, 0, problem.NewWithCode(problem.Conflict, problem.CodeLinkNotReady, "link is not parsed yet")
	}

	stored, err := s.links.GetContent(ctx, id)
	if err != nil {
		return nil, model.SavedContent{}, 0, err
	}
	if stored == nil || strings.TrimSpace(stored.Text) == "" {
		return nil, model.SavedContent{}, 0, problem.NewWithCode(problem.NotFound, problem.CodeLinkNotFound, "saved content not found")
	}
	revision := stored.Revision
	if revision <= 0 {
		revision = link.ContentRevision
	}
	return link, normalizeStoredContentForEdit(link.URL, *stored), revision, nil
}

func buildEditedContent(source, baseURL string, format model.ContentFormat) (model.SavedContent, error) {
	var (
		edited model.SavedContent
		err    error
	)
	if format == model.ContentFormatPlain {
		edited = contentdoc.Plain(source)
	} else {
		edited, err = contentdoc.FromMarkdown(source, baseURL)
		if err != nil {
			return model.SavedContent{}, problem.NewWithCode(problem.Malformed, problem.CodeContentEmpty, "content could not be normalized")
		}
	}
	if strings.TrimSpace(edited.Text) == "" {
		return model.SavedContent{}, problem.NewWithCode(problem.Malformed, problem.CodeContentEmpty, "content must not be empty")
	}
	edited.Source = model.ContentSourceUser
	edited.CJKChars, edited.Words = countReadingUnits(edited.Text)
	return edited, nil
}

func (s *ContentService) classifyEditMiss(ctx context.Context, id uuid.UUID, expectedRevision int64) (dto.LinkContentResponse, error) {
	_, _, currentRevision, err := s.loadEditableContent(ctx, id)
	if err != nil {
		return dto.LinkContentResponse{}, err
	}
	if currentRevision != expectedRevision {
		return dto.LinkContentResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeContentRevisionConflict, "saved content changed; reload before editing")
	}
	return dto.LinkContentResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeLinkNotReady, "saved content changed while editing; retry after reload")
}

func (s *ContentService) persist(ctx context.Context, linkID string, replace bool) (dto.LinkContentResponse, error) { //nolint:gocyclo // 保存正文需按格式与来源分派，并处理各自的冲突路径
	id, err := uuid.Parse(strings.TrimSpace(linkID))
	if err != nil {
		return dto.LinkContentResponse{}, problem.NewWithCode(problem.Malformed, problem.CodeInvalidLinkID, "invalid link id")
	}

	link, err := s.links.GetParseInputByID(ctx, id)
	if err != nil {
		return dto.LinkContentResponse{}, err
	}
	if link == nil {
		return dto.LinkContentResponse{}, problem.NewWithCode(problem.NotFound, problem.CodeLinkNotFound, "link not found")
	}
	if link.LibraryKind != nil && *link.LibraryKind == model.LibraryKindSite {
		return dto.LinkContentResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeSiteOriginalContentForbidden, "website entries cannot store original content")
	}

	if !replace {
		stored, loadErr := s.loadStoredContent(ctx, id)
		if loadErr != nil {
			return dto.LinkContentResponse{}, loadErr
		}
		if stored != nil {
			return *stored, nil
		}
	}
	// 仅解析完成后允许保存原文：与前端「done 之后才出现按钮」一致，也避免对
	// pending/failed 的 link 做无意义抓取。
	if link.Status != model.LinkStatusDone {
		return dto.LinkContentResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeLinkNotReady, "link not parsed yet; save original after parsing completes")
	}

	content, fetcherType, err := s.resolveContent(ctx, id, link, replace)
	if err != nil {
		return dto.LinkContentResponse{}, err
	}

	// 两项阅读计数必须在这里算：它们是与正文一起落库的派生值，而计数公式
	// （countReadingUnits）只有 Go 里这一份。漏了这一步的后果不是报错，是
	// 每一次保存都往库里写 0/0 —— 折叠态的阅读时长因此恒显示「1 分钟」，
	// 展开原文后按真实文本重算，数字当场跳变。
	content.CJKChars, content.Words = countReadingUnits(content.Text)

	var (
		storedCurrent bool
		revision      int64
	)
	if replace {
		revision, storedCurrent, err = s.links.ReplaceContentIfCurrentWithRevision(ctx, id, link.UpdatedAt, link.ContentRevision, content)
	} else {
		revision, storedCurrent, err = s.links.UpdateContentIfCurrent(ctx, id, link.UpdatedAt, content)
	}
	if err != nil {
		return dto.LinkContentResponse{}, err
	}
	if !storedCurrent {
		// Another Save may have won the same revision. Reuse its canonical
		// content; otherwise a requeue/parse transition changed the link while
		// content was being resolved and this stale body must not be written.
		if !replace {
			stored, loadErr := s.loadStoredContent(ctx, id)
			if loadErr != nil {
				return dto.LinkContentResponse{}, loadErr
			}
			if stored != nil {
				return *stored, nil
			}
		}
		current, currentErr := s.links.GetParseInputByID(ctx, id)
		if currentErr != nil {
			return dto.LinkContentResponse{}, currentErr
		}
		if current == nil {
			return dto.LinkContentResponse{}, problem.NewWithCode(
				problem.NotFound,
				problem.CodeLinkNotFound,
				"link not found",
			)
		}
		return dto.LinkContentResponse{}, problem.NewWithCode(
			problem.Conflict,
			problem.CodeLinkNotReady,
			"link changed while content was being saved; retry after parsing completes",
		)
	}

	content.Revision = revision
	return contentResponse(id, content, fetcherType), nil
}

// loadStoredContent makes Save idempotent. Once canonical content exists, it
// is returned without fetching a page that may have changed or disappeared.
func (s *ContentService) loadStoredContent(ctx context.Context, id uuid.UUID) (*dto.LinkContentResponse, error) {
	stored, err := s.links.GetContent(ctx, id)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, nil
	}
	content := normalizeSavedContent(*stored)
	if content.Text == "" {
		return nil, nil
	}
	response := contentResponse(id, content, "stored")
	return &response, nil
}

func (s *ContentService) resolveContent(ctx context.Context, id uuid.UUID, link *repository.LinkParseInput, replace bool) (model.SavedContent, string, error) {
	if isIngestSourceFields(link.SourceKind, link.InputText, link.InputHTML) {
		if link.InputHTML != nil {
			if rawHTML := strings.TrimSpace(*link.InputHTML); rawHTML != "" {
				document, err := contentdoc.FromHTML(rawHTML, link.URL)
				if err == nil && document.Text != "" {
					return document, strings.ToLower(strings.TrimSpace(link.SourceKind)), nil
				}
				if s.logger != nil && err != nil {
					s.logger.Warn("save-content captured html conversion failed", "link_id", id.String(), "err", observability.SafeError(err))
				}
			}
		}

		// Explicit replacement is also the upgrade path for historic browser
		// captures that only persisted flattened text. Try the live URL first;
		// if it is unavailable, retain the captured text as an honest plain
		// snapshot rather than inventing structure.
		if replace && isRemoteContentURL(link.URL) && s.fetcher != nil {
			if content, fetcherType, err := s.fetchRemoteContent(ctx, id, link.URL); err == nil {
				return content, fetcherType, nil
			}
		}
		if link.InputText != nil {
			if content := contentdoc.Plain(*link.InputText); content.Text != "" {
				return content, strings.ToLower(strings.TrimSpace(link.SourceKind)), nil
			}
		}
		return model.SavedContent{}, "", problem.NewWithCode(
			problem.Conflict,
			problem.CodeLinkContentUnavailable,
			"saved source has no readable text content",
		)
	}
	return s.fetchRemoteContent(ctx, id, link.URL)
}

func (s *ContentService) fetchRemoteContent(ctx context.Context, id uuid.UUID, rawURL string) (model.SavedContent, string, error) {
	if s.fetcher == nil {
		return model.SavedContent{}, "", problem.New(problem.Unavailable, "content fetching is not configured")
	}

	content, err := s.fetcher.Fetch(ctx, rawURL)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("save-content fetch failed", "link_id", id.String(), "err", observability.SafeError(err))
		}
		return model.SavedContent{}, "", problem.New(problem.Upstream, "failed to fetch original content")
	}
	body := strings.TrimSpace(content.Body)
	if body == "" {
		return model.SavedContent{}, "", problem.New(problem.Upstream, "fetched content is empty")
	}
	// Search summaries are discovery hints, not the source's canonical text.
	if isSearchSummaryFetcher(content.FetcherType) {
		if s.logger != nil {
			s.logger.Warn("save-content rejected search-summary fallback (not original content)",
				"link_id", id.String(), "fetcher_type", content.FetcherType)
		}
		return model.SavedContent{}, "", problem.New(problem.Upstream, "could not fetch original content (only a search summary was available)")
	}

	document := model.SavedContent{}
	if rawHTML := strings.TrimSpace(content.HTML); rawHTML != "" {
		if converted, convertErr := contentdoc.FromHTML(rawHTML, rawURL); convertErr == nil {
			document = converted
		} else if s.logger != nil {
			s.logger.Warn("save-content fetched html conversion failed", "link_id", id.String(), "err", observability.SafeError(convertErr))
		}
	} else if isMarkdownContentFetcher(content.FetcherType) {
		if converted, convertErr := contentdoc.FromMarkdown(body, rawURL); convertErr == nil {
			document = converted
		} else if s.logger != nil {
			s.logger.Warn("save-content fetched markdown conversion failed", "link_id", id.String(), "err", observability.SafeError(convertErr))
		}
	}
	if document.Text == "" {
		document = contentdoc.Plain(body)
	}
	return document, strings.TrimSpace(content.FetcherType), nil
}

func contentResponse(id uuid.UUID, content model.SavedContent, fetcherType string) dto.LinkContentResponse {
	content = normalizeSavedContent(content)
	return dto.LinkContentResponse{
		LinkID:          id.String(),
		Content:         content.Text,
		ContentDocument: content.Document,
		ContentFormat:   string(content.Format),
		FetcherType:     fetcherType,
		ContentSource:   string(content.Source),
		ContentRevision: content.Revision,
	}
}

func normalizeSavedContent(content model.SavedContent) model.SavedContent {
	content.Text = strings.TrimSpace(content.Text)
	if content.Source != model.ContentSourceUser && content.Source != model.ContentSourceFetched {
		content.Source = model.ContentSourceFetched
	}
	if content.Document != nil {
		trimmed := strings.TrimSpace(*content.Document)
		if trimmed == "" {
			content.Document = nil
		} else {
			content.Document = &trimmed
		}
	}
	switch content.Format {
	case model.ContentFormatMarkdown, model.ContentFormatHTML:
		if content.Document != nil {
			return content
		}
	}
	content.Document = nil
	content.Format = model.ContentFormatPlain
	return content
}

func normalizeStoredContentForEdit(baseURL string, content model.SavedContent) model.SavedContent {
	content = normalizeSavedContent(content)
	if content.Format != model.ContentFormatHTML || content.Document == nil {
		return content
	}
	// Older rows may have stored raw HTML under the html enum. Normalize those
	// rows before exposing them to the Markdown editor. Rows whose document is
	// already Markdown are kept as-is; this covers the historical transition
	// where the enum was retained after the capture pipeline changed.
	raw := strings.TrimSpace(*content.Document)
	if !strings.Contains(raw, "<") {
		content.Format = model.ContentFormatMarkdown
		return content
	}
	converted, err := contentdoc.FromHTML(raw, baseURL)
	if err == nil && strings.TrimSpace(converted.Text) != "" {
		converted.Source = content.Source
		converted.Revision = content.Revision
		return converted
	}
	return content
}

func isMarkdownContentFetcher(fetcherType string) bool {
	base := strings.ToLower(strings.TrimSpace(fetcherType))
	if idx := strings.IndexByte(base, '+'); idx >= 0 {
		base = base[:idx]
	}
	return base == "jina" || base == "github"
}

func isRemoteContentURL(raw string) bool {
	lowered := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasPrefix(lowered, "https://") || strings.HasPrefix(lowered, "http://")
}

// isSearchSummaryFetcher 判断 fetcher_type 是否为搜索摘要降级结果。Manager 给搜索
// 回退打的标记是 "search" 或在主类型后追加 "+search"（如 "basic+search"），两种形态都算。
func isSearchSummaryFetcher(fetcherType string) bool {
	ft := strings.TrimSpace(fetcherType)
	return ft == "search" || strings.Contains(ft, "+search")
}
