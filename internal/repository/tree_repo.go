package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/database"
	"webtag/internal/model"
)

const (
	lookupTreeLinksByURLsSQL = "SELECT " + linkSelectColumns + " FROM links WHERE url = ANY($1::text[]) AND status = 'done' AND deleted_at IS NULL"
	// listVisibleTreeSQL caps the response with LIMIT 5000 (inlined
	// inside ListVisible) so a 50k+ row links table cannot OOM the
	// API process. The cap is well above any healthy navigation tree
	// (the endpoint serves user-facing tree views, not full-corpus
	// exports); the service layer (read.go) compares len() to the
	// same constant and emits a Warn log when truncation kicks in.
	// Uses linkListColumns (not linkSelectColumns) because the tree
	// response never carries input_html / input_text / input_images /
	// source_metadata — pulling them would multiply the per-row
	// payload by 100x for browser-capture rows.
	listVisibleTreeSQL       = "SELECT " + linkListColumns + " FROM links WHERE status = 'done' AND deleted_at IS NULL"
	listTreeDomainsSQL       = "SELECT domain, count(*) AS count FROM links WHERE status = 'done' AND deleted_at IS NULL GROUP BY domain ORDER BY count DESC, domain ASC"
	listTreeDomainsScopedSQL = "SELECT domain, count(*) AS count FROM links WHERE status = 'done' AND deleted_at IS NULL AND library_kind = $1 GROUP BY domain ORDER BY count DESC, domain ASC"
)

// PGXTreeRepository 是 TreeStore 的 PG 实现，提供链接树视图所需的查询操作。
type PGXTreeRepository struct {
	db database.Querier
}

// NewPGXTreeRepository 用给定的 Querier 构造 PGXTreeRepository。
func NewPGXTreeRepository(db database.Querier) *PGXTreeRepository {
	return &PGXTreeRepository{db: db}
}

// LookupByURLs returns a map keyed by URL containing every existing done link
// row that matches one of the supplied URLs.
func (r *PGXTreeRepository) LookupByURLs(ctx context.Context, urls []string) (map[string]*model.Link, error) {
	out := make(map[string]*model.Link, len(urls))
	if len(urls) == 0 {
		return out, nil
	}

	rows, err := r.db.Query(ctx, lookupTreeLinksByURLsSQL, urls)
	if err != nil {
		return nil, fmt.Errorf("lookup tree links by urls: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		link, err := scanLink(rows)
		if err != nil {
			return nil, fmt.Errorf("scan tree link row: %w", err)
		}
		// Copy `link` into a new variable before taking its address.
		// The for-loop's body returns a fresh model.Link from scanLink
		// each iteration, but Go's loop-variable lifetime rules pre-1.22
		// would let `&link` alias across iterations; the explicit copy
		// is the conservative form and survives any future scanLink
		// refactor that decides to reuse a buffer.
		copyLink := link
		out[copyLink.URL] = &copyLink
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tree links by urls: %w", err)
	}
	return out, nil
}

// ListVisible 返回树视图中的真实 done 链接列表（可选 domain 过滤），按
// path_depth/created_at 排序，上限 5000 行避免大库下整树拉取打爆 API 进程。
func (r *PGXTreeRepository) ListVisible(ctx context.Context, domain *string) ([]model.Link, error) {
	query := listVisibleTreeSQL
	args := make([]any, 0, 1)
	if domain != nil && *domain != "" {
		query += " AND domain = $1"
		args = append(args, *domain)
	}
	// Cap is a constant int (not user input) so concatenation is safe.
	query += " ORDER BY path_depth ASC NULLS FIRST, created_at DESC LIMIT 5000"

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list visible tree links: %w", err)
	}
	defer rows.Close()

	links, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Link, error) {
		return scanLinkList(row)
	})
	if err != nil {
		return nil, fmt.Errorf("collect visible tree links: %w", err)
	}

	return links, nil
}

// ListDomains returns the domain-level summary used by GET /api/tree?view=domains.
func (r *PGXTreeRepository) ListDomains(ctx context.Context) (DomainTreeSummarySet, error) {
	return r.listDomains(ctx, listTreeDomainsSQL)
}

// ListDomainsScoped returns the same domain summary restricted to one final
// library partition. Empty domains still contribute to Total but never become
// selectable buckets.
func (r *PGXTreeRepository) ListDomainsScoped(ctx context.Context, kind model.LibraryKind) (DomainTreeSummarySet, error) {
	return r.listDomains(ctx, listTreeDomainsScopedSQL, kind)
}

func (r *PGXTreeRepository) listDomains(ctx context.Context, query string, args ...any) (DomainTreeSummarySet, error) {
	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return DomainTreeSummarySet{}, fmt.Errorf("list tree domains: %w", err)
	}
	defer rows.Close()

	result := DomainTreeSummarySet{Domains: []DomainTreeSummary{}}
	for rows.Next() {
		var domain pgtype.Text
		var count int
		if err := rows.Scan(&domain, &count); err != nil {
			return DomainTreeSummarySet{}, fmt.Errorf("scan tree domain: %w", err)
		}
		result.Total += count
		if domain.Valid && domain.String != "" {
			result.Domains = append(result.Domains, DomainTreeSummary{
				Domain: domain.String,
				Count:  count,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return DomainTreeSummarySet{}, fmt.Errorf("iterate tree domains: %w", err)
	}
	return result, nil
}
