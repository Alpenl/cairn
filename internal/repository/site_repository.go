package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/model"
)

const (
	findSiteIdentityForUpdateSQL = "SELECT site_id FROM site_identities WHERE identity_key = $1 FOR UPDATE"
	insertSiteSQL                = "INSERT INTO sites (site_key, name, intro, homepage_url, icon_url, first_collected_at, last_collected_at, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, NOW(), NOW(), NOW(), NOW()) ON CONFLICT (site_key) DO UPDATE SET last_collected_at = NOW(), updated_at = NOW() RETURNING id, (xmax = 0)"
	insertSiteIdentitySQL        = "INSERT INTO site_identities (identity_key, site_id, created_at, updated_at) VALUES ($1, $2, NOW(), NOW()) ON CONFLICT (identity_key) DO UPDATE SET updated_at = NOW() RETURNING site_id"
	deleteUnboundSiteSQL         = "DELETE FROM sites WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM site_identities WHERE site_id = $1) AND NOT EXISTS (SELECT 1 FROM site_entries WHERE site_id = $1)"
	insertSiteEntrySQL           = "INSERT INTO site_entries (site_id, link_id, entry_name, purpose, normalized_url, first_collected_at, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, NOW(), NOW(), NOW()) ON CONFLICT (link_id) DO UPDATE SET last_recollected_at = NOW(), updated_at = NOW() RETURNING id, site_id, (xmax = 0)"
	// site_entries 有两个唯一索引：(link_id) 与 (site_id, normalized_url)。
	// ON CONFLICT 只能推断其中一个，而两个 Link 完全可能在 urlidentity 下互不
	// 相同、在 siteidentity 下归一到同一个 URL（?q=hello+world 与
	// ?q=hello%20world）。那时插入带着新的 link_id，(link_id) 冲突不触发，
	// 直接撞上 idx_site_entries_site_url 抛 23505，回滚整个 CompleteSiteParse，
	// Link 永远卡在 processing 被 River 反复重试。先按第二个唯一键规避。
	findSiteEntryByNormalizedURLSQL = "SELECT id, site_id FROM site_entries WHERE site_id = $1 AND normalized_url = $2 AND link_id <> $3"
	touchSiteEntrySQL               = "UPDATE site_entries SET last_recollected_at = NOW(), updated_at = NOW() WHERE id = $1"
	setPrimarySiteEntrySQL          = "UPDATE sites SET primary_entry_id = COALESCE(primary_entry_id, $1), last_collected_at = NOW(), updated_at = NOW() WHERE id = $2"
	bumpSiteRevisionSQL             = "UPDATE sites SET revision = revision + 1, updated_at = NOW() WHERE id = $1"
	completeSiteLinkSQL             = `WITH prior AS (
		SELECT id,metadata_revision,parse_generation
		FROM links
		WHERE id=$1 AND deleted_at IS NULL
		FOR UPDATE
	)
	UPDATE links AS link SET
		title=CASE WHEN $16 > 0 AND prior.metadata_revision=$16 THEN $2 ELSE link.title END,
		summary=NULL,
		tags=CASE WHEN $16 > 0 AND prior.metadata_revision=$16 THEN COALESCE($3,'{}'::text[]) ELSE link.tags END,
		fetcher_type=$4,is_low_confidence=$5,low_confidence_reason=$6,status='done',error_msg=NULL,
		domain=$7,content_type=$8,path_depth=$9,parent_path=$10,parent_id=$11,
		library_kind='site',library_kind_locked=$12,
		content=NULL,content_cjk_chars=0,content_words=0,content_document=NULL,content_format='plain',content_source='fetched',
		content_revision=link.content_revision+1,input_text=NULL,input_html=NULL,input_images=NULL,source_metadata=NULL,
		payload_purge_due_at=NULL,payload_purged_at=NOW(),updated_at=NOW()
	FROM prior
	WHERE link.id=prior.id
	  AND link.library_kind IS NOT DISTINCT FROM $13 AND link.library_kind_locked=$14
	  AND $15 > 0 AND prior.parse_generation=$15
	  AND link.status IN ('pending','processing')`
	deleteSiteTranslationsSQL = "DELETE FROM link_translations WHERE link_id = $1"
)

// Aggregate creates/refreshes a site aggregate for one final site link. The
// identity lookup is locked before creating an aggregate so a pre-existing
// manual identity binding always wins over the automatic site_key candidate.
func (r *PGXLinkRepository) Aggregate(ctx context.Context, params AggregateSiteParams) (SiteAggregateResult, error) {
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return SiteAggregateResult{}, fmt.Errorf("begin site aggregate tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := aggregateSiteOn(ctx, tx, params)
	if err != nil {
		return SiteAggregateResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SiteAggregateResult{}, fmt.Errorf("commit site aggregate tx: %w", err)
	}
	return result, nil
}

// ValidateAggregateSiteParams 校验聚合参数。
//
// 它在 aggregateSiteOn 内执行而非在公开的 Aggregate 上——CompleteSiteParse 直接
// 调 aggregateSiteOn，若守卫只挂在 Aggregate 上，同一份 AggregateSiteParams 走
// 两个入口就会拿到两套校验强度，而 name / entry_name 的空值最终会撞上
// chk_sites_lengths / chk_site_entries_lengths 这两条 DB CHECK。
// ValidateAggregateSiteParams 导出，供 repotest 的 fake 直接复用。
//
// fake 此前靠「复刻」生产校验，复刻就会漂移：site fake 曾只抄了 name/entry_name
// 两条，漏掉 LinkID / IdentityKey / NormalizedURL。共用同一份实现后，漂移在结构
// 上不可能发生。
func ValidateAggregateSiteParams(params AggregateSiteParams) error {
	if params.LinkID == uuid.Nil || strings.TrimSpace(params.IdentityKey) == "" || strings.TrimSpace(params.NormalizedURL) == "" {
		return fmt.Errorf("aggregate site: link id, identity key, and normalized URL are required")
	}
	if strings.TrimSpace(params.Name) == "" || strings.TrimSpace(params.EntryName) == "" {
		return fmt.Errorf("aggregate site: name and entry name are required")
	}
	return nil
}

func aggregateSiteOn(ctx context.Context, tx pgx.Tx, params AggregateSiteParams) (SiteAggregateResult, error) {
	if err := ValidateAggregateSiteParams(params); err != nil {
		return SiteAggregateResult{}, err
	}
	siteID, created, err := aggregateSiteIdentity(ctx, tx, params)
	if err != nil {
		return SiteAggregateResult{}, err
	}
	var entryID, entrySiteID uuid.UUID
	var createdEntry bool
	// 同一站点下已有另一个 Link 占用了这个归一 URL：复用那条 entry，不要再插
	// 一行去撞 (site_id, normalized_url) 唯一索引。
	switch err := tx.QueryRow(ctx, findSiteEntryByNormalizedURLSQL, siteID, params.NormalizedURL, params.LinkID).
		Scan(&entryID, &entrySiteID); {
	case err == nil:
		if _, err := tx.Exec(ctx, touchSiteEntrySQL, entryID); err != nil {
			return SiteAggregateResult{}, fmt.Errorf("touch existing site entry: %w", err)
		}
	case errors.Is(err, pgx.ErrNoRows):
		if err := tx.QueryRow(ctx, insertSiteEntrySQL, siteID, params.LinkID, params.EntryName, params.Purpose, params.NormalizedURL).Scan(&entryID, &entrySiteID, &createdEntry); err != nil {
			return SiteAggregateResult{}, fmt.Errorf("insert site entry: %w", err)
		}
	default:
		return SiteAggregateResult{}, fmt.Errorf("find site entry by normalized url: %w", err)
	}
	if _, err := tx.Exec(ctx, setPrimarySiteEntrySQL, entryID, entrySiteID); err != nil {
		return SiteAggregateResult{}, fmt.Errorf("set site primary entry: %w", err)
	}
	if createdEntry && !created {
		if _, err := tx.Exec(ctx, bumpSiteRevisionSQL, entrySiteID); err != nil {
			return SiteAggregateResult{}, fmt.Errorf("bump site revision after entry insert: %w", err)
		}
	}
	return SiteAggregateResult{SiteID: entrySiteID, EntryID: entryID, CreatedSite: created, CreatedEntry: createdEntry}, nil
}

// CompleteSiteParse makes a link visible as a site only after all
// reading-only artifacts are removed and its SiteEntry is durable.
func (r *PGXLinkRepository) CompleteSiteParse(ctx context.Context, params CompleteSiteParseParams) (SiteAggregateResult, error) {
	if params.Classification.Kind != "site" {
		return SiteAggregateResult{}, fmt.Errorf("complete site parse: final site classification is required")
	}
	if params.Analysis.ID != params.Site.LinkID || params.Analysis.ID != params.Classification.ID {
		return SiteAggregateResult{}, fmt.Errorf("complete site parse: link ids must match")
	}
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return SiteAggregateResult{}, fmt.Errorf("begin complete site parse tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	analysis, classification := params.Analysis, params.Classification
	tag, err := tx.Exec(ctx, completeSiteLinkSQL, analysis.ID, analysis.Title, analysis.Tags, analysis.FetcherType, analysis.IsLowConfidence, analysis.LowConfidenceReason, analysis.Domain, analysis.ContentType, analysis.PathDepth, analysis.ParentPath, nullableUUIDValue(analysis.ParentID), classification.Locked, params.ExpectedLibraryKind, params.ExpectedLibraryKindLocked, analysis.ExpectedParseGeneration, analysis.ExpectedMetadataRevision)
	if err != nil {
		return SiteAggregateResult{}, fmt.Errorf("complete site link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if err := requireCurrentParseAttempt(ctx, tx, model.ParseAttempt{LinkID: analysis.ID, Generation: analysis.ExpectedParseGeneration}); err != nil {
			return SiteAggregateResult{}, err
		}
		return SiteAggregateResult{}, classifyLibrarySelectionMiss(ctx, tx, analysis.ID, params.ExpectedLibraryKind, params.ExpectedLibraryKindLocked)
	}
	if _, err := tx.Exec(ctx, deleteSiteTranslationsSQL, analysis.ID); err != nil {
		return SiteAggregateResult{}, fmt.Errorf("delete site translations: %w", err)
	}
	result, err := aggregateSiteOn(ctx, tx, params.Site)
	if err != nil {
		return SiteAggregateResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SiteAggregateResult{}, fmt.Errorf("commit complete site parse tx: %w", err)
	}
	return result, nil
}

func aggregateSiteIdentity(ctx context.Context, tx pgx.Tx, params AggregateSiteParams) (uuid.UUID, bool, error) {
	var siteID uuid.UUID
	err := tx.QueryRow(ctx, findSiteIdentityForUpdateSQL, params.IdentityKey).Scan(&siteID)
	if err == nil {
		return siteID, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, fmt.Errorf("find site identity: %w", err)
	}

	var (
		candidateID      uuid.UUID
		createdCandidate bool
	)
	if err := tx.QueryRow(ctx, insertSiteSQL, params.IdentityKey, params.Name, params.Intro, params.HomepageURL, params.IconURL).Scan(&candidateID, &createdCandidate); err != nil {
		return uuid.Nil, false, fmt.Errorf("insert site: %w", err)
	}
	if err := tx.QueryRow(ctx, insertSiteIdentitySQL, params.IdentityKey, candidateID).Scan(&siteID); err != nil {
		return uuid.Nil, false, fmt.Errorf("bind site identity: %w", err)
	}
	if siteID != candidateID && createdCandidate {
		if _, err := tx.Exec(ctx, deleteUnboundSiteSQL, candidateID); err != nil {
			return uuid.Nil, false, fmt.Errorf("delete unbound candidate site: %w", err)
		}
	}
	return siteID, createdCandidate && siteID == candidateID, nil
}

var _ SiteParseCompleter = (*PGXLinkRepository)(nil)
