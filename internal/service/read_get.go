package service

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/representation"
)

// Get 返回单条链接详情（含已保存原文正文），等价于 GetWithContent(true)。
func (s *LinkReadService) Get(ctx context.Context, linkID string) (dto.LinkResponse, error) {
	return s.GetWithContent(ctx, linkID, true)
}

// GetWithContent 返回单条链接详情，按需附加 concept 显示名与已保存原文。
// includeContent=false 时只给 has_content 标记、不带正文本身——Reader 的原文
// 是默认折叠的，展开时才走 GET /api/links/{id}/content 单独取，打开一篇文章
// 不该顺带把整篇正文拖过网络。
// linkID 校验失败返回 400，未命中返回 404。
func (s *LinkReadService) GetWithContent(ctx context.Context, linkID string, includeContent bool) (dto.LinkResponse, error) {
	id, err := uuid.Parse(strings.TrimSpace(linkID))
	if err != nil {
		// 带 slug：客户端可以按 error_code=invalid_link_id 直接区分"链接不
		// 存在"和"链接 ID 格式错误"，不必再解析 message。
		return dto.LinkResponse{}, httperr.NewWithCode(http.StatusBadRequest, httperr.CodeInvalidLinkID, "invalid link id")
	}

	link, err := s.links.GetDetailByID(ctx, id)
	if err != nil {
		return dto.LinkResponse{}, err
	}
	if link == nil {
		return dto.LinkResponse{}, httperr.NewWithCode(http.StatusNotFound, httperr.CodeLinkNotFound, "link not found")
	}

	resp := linkDetailToResponse(*link)
	resp.ContentSource = stringPtr(string(canonicalContentSource(link.ContentSource)))
	if names := s.fetchDisplayNamesByIDs(ctx, []uuid.UUID{link.ID}); len(names[link.ID]) > 0 {
		resp.Tags = names[link.ID]
	}
	// has_content 与两项计数现在直接来自列（PF6），**不再为了这三个数字把
	// 整篇正文读出来再扔掉**——那是详情端此前每次都在做的事。
	//
	// 正文本身只在调用方明确要求时才读：Reader 的原文是折叠的，展开时才单独取。
	s.attachSavedContent(ctx, link.ID, includeContent, &resp)
	return resp, nil
}

func canonicalContentSource(source model.ContentSource) model.ContentSource {
	if source == model.ContentSourceUser || source == model.ContentSourceFetched {
		return source
	}
	return model.ContentSourceFetched
}

func (s *LinkReadService) attachSavedContent(ctx context.Context, linkID uuid.UUID, includeContent bool, resp *dto.LinkResponse) {
	if !includeContent || !resp.HasContent {
		return
	}
	if s.contentReader == nil {
		representation.MarkResponseNonCacheable(ctx)
		return
	}
	content, err := s.contentReader.GetContent(ctx, linkID)
	if err != nil || content == nil || content.Text == "" {
		representation.MarkResponseNonCacheable(ctx)
		return
	}

	resp.Content = stringPtr(content.Text)
	resp.ContentDocument = content.Document
	resp.ContentFormat = stringPtr(string(content.Format))
	resp.ContentSource = stringPtr(string(canonicalContentSource(content.Source)))
	if content.Revision > 0 {
		resp.ContentRevision = content.Revision
	}
	// 存量数据的懒回填：PF6 之前保存的原文没有计数列。这里顺手补上，
	// 避免为历史数据单独跑一次全表回填（正文都已经在手里了，零额外 I/O）。
	if resp.ContentCJKChars == 0 && resp.ContentWords == 0 {
		resp.ContentCJKChars, resp.ContentWords = countReadingUnits(content.Text)
	}
}

// Delete 删除一条链接并主动失效标签缓存。这里清缓存是必须的——否则
// 已删除链接的标签会在 TTL 内继续出现在 /api/tags 聚合结果里。
func (s *LinkReadService) Delete(ctx context.Context, linkID string) error {
	id, err := uuid.Parse(strings.TrimSpace(linkID))
	if err != nil {
		return httperr.NewWithCode(http.StatusBadRequest, httperr.CodeInvalidLinkID, "invalid link id")
	}

	link, err := s.links.GetLifecycleByID(ctx, id)
	if err != nil {
		return err
	}
	if link == nil {
		return httperr.NewWithCode(http.StatusNotFound, httperr.CodeLinkNotFound, "link not found")
	}

	err = s.mutationLocker.WithURL(ctx, link.URL, func(lockCtx context.Context) error {
		if s.deleteCommands == nil {
			return errors.New("delete link: durable commands are not configured")
		}
		return s.deleteCommands.DeleteLink(lockCtx, DeleteLinkCommand{LinkID: id})
	})
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return httperr.NewWithCode(http.StatusNotFound, httperr.CodeLinkNotFound, "link not found")
		}
		return err
	}

	if s.tagInvalidator != nil {
		s.tagInvalidator.Invalidate(ctx)
	}

	return nil
}

// readingCJK / readingWord 与 Reader 的 DetailPane 用的是同一对模式：CJK 逐字
// 计数，西文按词计数（撇号与连字符不断词）。两边必须同源，否则同一篇文章在
// 「折叠时按计数估」和「展开后按正文算」之间会给出两个数字——那正是这两个
// 字段要消灭的东西。read_get_test.go 用同一段样本钉住两侧的一致性。
var (
	readingCJK  = regexp.MustCompile(`[\x{3400}-\x{9fff}]`)
	readingWord = regexp.MustCompile(`[A-Za-z0-9]+(?:['\x{2019}-][A-Za-z0-9]+)*`)
)

// countReadingUnits 返回正文的 CJK 字符数与西文词数，供客户端估算阅读时长。
func countReadingUnits(text string) (cjk int, words int) {
	return len(readingCJK.FindAllString(text, -1)), len(readingWord.FindAllString(text, -1))
}
