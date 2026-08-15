package service

import (
	"context"
	"net/http"
	"time"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/repository"
)

// TreeReadService 提供 /api/tree 树状视图：把仓储返回的真实链接列表组装成
// 父子嵌套结构，附带域名过滤、软上限截断观测。
type TreeReadService struct {
	tree    repository.TreeStore
	metrics *observability.Metrics
	// domainCache 缓存 /api/tree?view=domains 的域名聚合。nil 表示不缓存
	// （测试装配常如此），行为退化为每次直查。
	domainCache *SnapshotCache[dto.DomainTreeSummaryEnvelope]
}

// NewTreeReadService accepts an optional *observability.Metrics so the
// truncation counter can be incremented when the soft cap is hit. nil
// metrics keeps the service silent (production wiring always supplies
// one; tests may not).
func NewTreeReadService(tree repository.TreeStore, metrics *observability.Metrics) *TreeReadService {
	return &TreeReadService{tree: tree, metrics: metrics}
}

// DomainSummaryCache 是域名摘要缓存的具体类型，便于装配层持有同一个实例并
// 同时接给读服务与写路径的失效器。
type DomainSummaryCache = SnapshotCache[dto.DomainTreeSummaryEnvelope]

// NewDomainSummaryCache 构造域名摘要缓存；ttl <= 0 返回 nil（不缓存）。
func NewDomainSummaryCache(ttl time.Duration, now func() time.Time) *DomainSummaryCache {
	if ttl <= 0 {
		return nil
	}
	return NewSnapshotCache(ttl, now, cloneDomainSummaryEnvelope)
}

// WithDomainCache 挂上域名摘要缓存并返回自身，便于装配层链式配置。
// cache 为 nil 表示不缓存，行为退化为每次直查。
func (s *TreeReadService) WithDomainCache(cache *DomainSummaryCache) *TreeReadService {
	s.domainCache = cache
	return s
}

// DomainCacheInvalidator 暴露域名摘要缓存的失效入口，供装配层接进写路径。
// 未启用缓存时返回 nil（调用方用 MultiCacheInvalidator 过滤）。
//
// 域名聚合与标签聚合由同一批事件改变（链接的增、删、重新解析），因此两者
// 必须一起失效——只失效其中一个，侧栏的两组数字就会在 TTL 窗口内互相矛盾。
func (s *TreeReadService) DomainCacheInvalidator() CacheInvalidator {
	if s.domainCache == nil {
		return nil
	}
	return s.domainCache
}

// cloneDomainSummaryEnvelope 深拷贝信封里的切片，避免调用方就地改写快照。
func cloneDomainSummaryEnvelope(value dto.DomainTreeSummaryEnvelope) dto.DomainTreeSummaryEnvelope {
	if value.Domains == nil {
		return value
	}
	cloned := make([]dto.DomainTreeSummaryResponse, len(value.Domains))
	copy(cloned, value.Domains)
	value.Domains = cloned
	return value
}

// Get 按可选 domain 过滤返回完整树。命中仓储软上限时记一次截断指标并 Warn
// 日志，方便运维识别响应被裁剪过、需要让前端按域名收窄查询。
func (s *TreeReadService) Get(ctx context.Context, domain string) (dto.TreeResponse, error) {
	filter := stringPtr(domain)
	links, err := s.tree.ListVisible(ctx, filter)
	if err != nil {
		return dto.TreeResponse{}, err
	}

	if len(links) >= treeListVisibleSoftCap {
		if logger := observability.FromContext(ctx); logger != nil {
			logger.Warn("tree response hit soft cap; result is truncated",
				"cap", treeListVisibleSoftCap,
				"domain_filter", domain,
			)
		}
		if s.metrics != nil {
			label := "all"
			if filter != nil {
				// Bucket per-domain truncation under one label so the
				// cardinality of /metrics stays bounded; the actual
				// domain is in the slog Warn line above for
				// debugging.
				label = "filtered"
			}
			s.metrics.TreeResponseTruncatedTotal.WithLabelValues(label).Inc()
		}
	}

	nodes, total := BuildTree(links)
	return dto.TreeResponse{
		Nodes: nodes,
		Total: total,
	}, nil
}

// ListDomains returns the lightweight domain summary used by the tree landing
// page before the client drills into a specific domain tree.
func (s *TreeReadService) ListDomains(ctx context.Context) (dto.DomainTreeSummaryEnvelope, error) {
	if s.domainCache == nil {
		return s.loadDomains(ctx)
	}
	return s.domainCache.Get(ctx, "", s.loadDomains)
}

// ListDomainsScoped returns the domain aggregate for one final library kind.
// An omitted scope deliberately delegates to the legacy aggregate.
func (s *TreeReadService) ListDomainsScoped(ctx context.Context, rawScope string) (dto.DomainTreeSummaryEnvelope, error) {
	kind, valid := model.NormalizeOptionalLibraryKind(rawScope)
	if !valid {
		return dto.DomainTreeSummaryEnvelope{}, httperr.NewWithCode(
			http.StatusUnprocessableEntity,
			httperr.CodeInvalidRequestedLibraryKind,
			"library_kind must be reading or site",
		)
	}
	if kind == "" {
		return s.ListDomains(ctx)
	}
	scope := string(kind)
	loader := func(loadCtx context.Context) (dto.DomainTreeSummaryEnvelope, error) {
		set, err := s.tree.ListDomainsScoped(loadCtx, kind)
		if err != nil {
			return dto.DomainTreeSummaryEnvelope{}, err
		}
		return mapDomainSummaryEnvelope(set, scope), nil
	}
	if s.domainCache == nil {
		return loader(ctx)
	}
	return s.domainCache.Get(ctx, scope, loader)
}

// loadDomains 是回源路径：一次全表 `GROUP BY domain`（repository 的
// listTreeDomainsSQL）。改造前 /api/tree?view=domains 每次调用都跑一遍它，
// 而 Reader 每次进主界面与每次点同步都会请求一次。
func (s *TreeReadService) loadDomains(ctx context.Context) (dto.DomainTreeSummaryEnvelope, error) {
	set, err := s.tree.ListDomains(ctx)
	if err != nil {
		return dto.DomainTreeSummaryEnvelope{}, err
	}
	return mapDomainSummaryEnvelope(set, ""), nil
}

func mapDomainSummaryEnvelope(set repository.DomainTreeSummarySet, scope string) dto.DomainTreeSummaryEnvelope {
	out := make([]dto.DomainTreeSummaryResponse, 0, len(set.Domains))
	for _, row := range set.Domains {
		out = append(out, dto.DomainTreeSummaryResponse{
			Domain: row.Domain,
			Count:  row.Count,
		})
	}
	envelope := dto.DomainTreeSummaryEnvelope{Domains: out, Total: set.Total}
	if scope != "" {
		envelope.LibraryKind = &scope
	}
	return envelope
}
