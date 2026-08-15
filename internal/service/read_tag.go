package service

import (
	"context"
	"net/http"
	"strings"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/repository"
)

// TagCountCache 是 TagReadService 依赖的缓存能力。生产由 *TagCache 实现。
type TagCountCache interface {
	Get(context.Context, TagLoader) ([]TagCount, error)
	GetScoped(context.Context, string, TagLoader) ([]TagCount, error)
}

// TagReadService 提供 /api/tags 标签聚合读取，cache 非空时所有读穿过缓存，
// 多个并发请求合并到一次 DB 加载。缓存按租户分键。
type TagReadService struct {
	tags  repository.TagStore
	cache TagCountCache
}

// NormalizeTagLibraryKind returns the canonical query variant shared by the
// service and conditional route policy.
func NormalizeTagLibraryKind(raw string) (string, bool) {
	scope := strings.ToLower(strings.TrimSpace(raw))
	switch scope {
	case "", "reading", "site", "all":
		return scope, true
	default:
		return "", false
	}
}

// ListScoped returns collection-aware counts. The legacy omitted scope still
// routes through List so existing cache semantics remain byte-for-byte stable.
func (s *TagReadService) ListScoped(ctx context.Context, rawScope string) ([]dto.TagCountResponse, error) {
	scope, valid := NormalizeTagLibraryKind(rawScope)
	if !valid {
		return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidRequestedLibraryKind, "library_kind must be reading, site, or all")
	}
	if scope == "" {
		return s.List(ctx)
	}
	store, ok := s.tags.(repository.ScopedTagStore)
	if !ok {
		return nil, httperr.NewWithCode(http.StatusServiceUnavailable, "site_library_unavailable", "collection tag counts are not configured")
	}

	// 这条路径改造前完全绕过缓存直落 DB，而其中 scope=all 是两个聚合的
	// FULL OUTER JOIN（repository/tag_repo.go）。按 scope 分切面缓存，
	// 与无 scope 路径共用同一个 SnapshotCache 实例，因此同租户的失效会一次
	// 打掉全部切面。
	loader := func(loadCtx context.Context) ([]TagCount, error) {
		counts, loadErr := store.ListScopedCounts(loadCtx, scope)
		if loadErr != nil {
			return nil, loadErr
		}
		return mapScopedTagCounts(counts), nil
	}

	var counts []TagCount
	var err error
	if s.cache == nil {
		counts, err = loader(ctx)
	} else {
		counts, err = s.cache.GetScoped(ctx, scope, loader)
	}
	if err != nil && counts == nil {
		return nil, err
	}

	out := make([]dto.TagCountResponse, 0, len(counts))
	for _, item := range counts {
		reading, site := item.ReadingCount, item.SiteCount
		out = append(out, dto.TagCountResponse{Tag: item.Tag, Count: item.Count, ReadingCount: &reading, SiteCount: &site})
	}
	return out, nil
}

// NewTagReadService 装配 TagReadService；cache 为 nil 时退化为每次直查 DB，
// 通常仅在测试场景下出现。
func NewTagReadService(tags repository.TagStore, cache TagCountCache) *TagReadService {
	return &TagReadService{
		tags:  tags,
		cache: cache,
	}
}

// List 返回全量标签计数。命中缓存即 O(1) 返回；缓存未命中由 singleflight
// 合并并发加载。Get 返回错误且无可用快照时直接上抛，否则按 stale-on-error
// 策略继续返回上一次成功结果。
func (s *TagReadService) List(ctx context.Context) ([]dto.TagCountResponse, error) {
	if s.cache == nil {
		counts, err := s.tags.ListCounts(ctx)
		if err != nil {
			return nil, err
		}
		return mapRepositoryTagCounts(counts), nil
	}

	counts, err := s.cache.Get(ctx, func(loadCtx context.Context) ([]TagCount, error) {
		rows, loadErr := s.tags.ListCounts(loadCtx)
		if loadErr != nil {
			return nil, loadErr
		}
		return mapServiceTagCounts(rows), nil
	})
	if err != nil && counts == nil {
		return nil, err
	}

	return mapTagResponses(counts), nil
}
