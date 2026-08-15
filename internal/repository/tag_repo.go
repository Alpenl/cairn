package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
)

// tagListResponseCap caps the row count returned by /api/tags. Tag
// counts are populated by unnest()ing the tags array of every done
// link; without a cap a 100k-row corpus emits ~400k expansions per
// cache miss. Cache invalidation fires on every Delete and every
// successful parse, so a cap is operational hygiene rather than a
// hard correctness requirement. 1000 covers the long tail of any
// realistic UI (no human navigates a thousand-tag dropdown) while
// keeping the worst-case rendered response bounded.
const tagListResponseCap = 1000

// listDistinctTagsSQL / listTagCountsSQL share the tagListResponseCap
// LIMIT so a future cap bump lands in one constant. fmt.Sprintf at
// package init keeps the cap as the single source of truth without
// reaching for runtime parameters (the cap is fixed at build time).
var (
	listDistinctTagsSQL = fmt.Sprintf(
		"SELECT DISTINCT unnest(tags) AS tag FROM links WHERE status = 'done' AND deleted_at IS NULL AND tags IS NOT NULL ORDER BY tag LIMIT %d",
		tagListResponseCap,
	)
	listTagCountsSQL = fmt.Sprintf(
		"SELECT unnest(tags) AS tag, count(*) AS count FROM links WHERE status = 'done' AND deleted_at IS NULL AND tags IS NOT NULL GROUP BY tag ORDER BY count DESC LIMIT %d",
		tagListResponseCap,
	)
)

var scopedTagCountsSQL = map[string]string{
	"reading": fmt.Sprintf("SELECT unnest(tags) AS tag, count(*) AS count, count(*) AS reading_count, 0 AS site_count FROM links WHERE status='done' AND library_kind='reading' AND deleted_at IS NULL AND tags IS NOT NULL GROUP BY tag ORDER BY count DESC, tag LIMIT %d", tagListResponseCap),
	"site":    fmt.Sprintf("SELECT normalized_tag AS tag, count(DISTINCT site_id) AS count, 0 AS reading_count, count(DISTINCT site_id) AS site_count FROM site_tags GROUP BY normalized_tag ORDER BY count DESC, tag LIMIT %d", tagListResponseCap),
	// 连接键必须归一化：links.tags 保留用户原样大小写，site_tags.normalized_tag
	// 恒为 lower(tag)。直接 USING (tag) 会把同一个标签劈成两行（例如 AI 与 ai
	// 各自带一半计数）。这里统一按 lower 连接，展示名优先取 reading 侧原样。
	"all": fmt.Sprintf(`WITH reading AS (SELECT lower(tag) AS tag_key, min(tag) AS display, count(*) AS count FROM (SELECT unnest(tags) AS tag FROM links WHERE status='done' AND library_kind='reading' AND deleted_at IS NULL AND tags IS NOT NULL) AS expanded GROUP BY lower(tag)), sites AS (SELECT normalized_tag AS tag_key, min(normalized_tag) AS display, count(DISTINCT site_id) AS count FROM site_tags GROUP BY normalized_tag) SELECT COALESCE(reading.display, sites.display) AS tag, COALESCE(reading.count,0)+COALESCE(sites.count,0) AS count, COALESCE(reading.count,0) AS reading_count, COALESCE(sites.count,0) AS site_count FROM reading FULL OUTER JOIN sites USING (tag_key) ORDER BY count DESC, tag LIMIT %d`, tagListResponseCap),
}

// PGXTagRepository 是 TagStore 的 PG 实现，基于 links.tags（text[]）做 unnest 聚合。
type PGXTagRepository struct {
	db database.Querier
}

// NewPGXTagRepository 用给定的 Querier 构造 PGXTagRepository。
func NewPGXTagRepository(db database.Querier) *PGXTagRepository {
	return &PGXTagRepository{db: db}
}

// ListDistinct 返回所有 done 状态链接上出现过的去重标签（按字典序），上限由 tagListResponseCap 控制。
func (r *PGXTagRepository) ListDistinct(ctx context.Context) ([]string, error) {
	rows, err := r.db.Query(ctx, listDistinctTagsSQL)
	if err != nil {
		return nil, fmt.Errorf("list distinct tags: %w", err)
	}
	defer rows.Close()

	tags, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var tag string
		err := row.Scan(&tag)
		return tag, err
	})
	if err != nil {
		return nil, fmt.Errorf("collect distinct tags: %w", err)
	}

	return tags, nil
}

// ListCounts 返回 done 链接上各 tag 出现次数（按 count 降序），上限同样为 tagListResponseCap。
func (r *PGXTagRepository) ListCounts(ctx context.Context) ([]TagCount, error) {
	rows, err := r.db.Query(ctx, listTagCountsSQL)
	if err != nil {
		return nil, fmt.Errorf("list tag counts: %w", err)
	}
	defer rows.Close()

	counts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TagCount, error) {
		var count TagCount
		err := row.Scan(&count.Tag, &count.Count)
		return count, err
	})
	if err != nil {
		return nil, fmt.Errorf("collect tag counts: %w", err)
	}

	return counts, nil
}

func (r *PGXTagRepository) ListScopedCounts(ctx context.Context, scope string) ([]ScopedTagCount, error) {
	query, ok := scopedTagCountsSQL[scope]
	if !ok {
		return nil, fmt.Errorf("unsupported tag scope %q", scope)
	}
	rows, err := r.db.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list %s tag counts: %w", scope, err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ScopedTagCount, error) {
		var out ScopedTagCount
		err := row.Scan(&out.Tag, &out.Count, &out.ReadingCount, &out.SiteCount)
		return out, err
	})
}

var _ ScopedTagStore = (*PGXTagRepository)(nil)
