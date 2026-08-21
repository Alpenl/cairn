package service

import (
	"context"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/problem"
	"webtag/internal/repository"
)

// TreeReadService 提供 /api/tree 树状视图：把仓储返回的真实链接列表组装成
// 父子嵌套结构，附带域名过滤、软上限截断观测。
type TreeReadService struct {
	tree repository.TreeStore
}

func NewTreeReadService(tree repository.TreeStore) *TreeReadService {
	return &TreeReadService{tree: tree}
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
	return s.loadDomains(ctx)
}

// ListDomainsScoped returns the domain aggregate for one final library kind.
// An omitted scope deliberately delegates to the legacy aggregate.
func (s *TreeReadService) ListDomainsScoped(ctx context.Context, rawScope string) (dto.DomainTreeSummaryEnvelope, error) {
	kind, valid := model.NormalizeOptionalLibraryKind(rawScope)
	if !valid {
		return dto.DomainTreeSummaryEnvelope{}, problem.NewWithCode(
			problem.Invalid,
			problem.CodeInvalidRequestedLibraryKind,
			"library_kind must be reading or site",
		)
	}
	if kind == "" {
		return s.ListDomains(ctx)
	}
	scope := string(kind)
	set, err := s.tree.ListDomainsScoped(ctx, kind)
	if err != nil {
		return dto.DomainTreeSummaryEnvelope{}, err
	}
	return mapDomainSummaryEnvelope(set, scope), nil
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
