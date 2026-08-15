package repository

import (
	"context"
	"fmt"
)

const updateSiteProfileSQL = `UPDATE sites
SET name = COALESCE($1, name),
    name_source = CASE WHEN $1 IS NULL THEN name_source ELSE 'user' END,
    intro = COALESCE($2, intro),
    intro_source = CASE WHEN $2 IS NULL THEN intro_source ELSE 'user' END,
    homepage_url = COALESCE($3, homepage_url),
    homepage_source = CASE WHEN $3 IS NULL THEN homepage_source ELSE 'user' END,
    icon_url = COALESCE($4, icon_url),
    icon_source = CASE WHEN $4 IS NULL THEN icon_source ELSE 'user' END,
    user_note = COALESCE($5, user_note),
    pinned = COALESCE($6, pinned),
	embedding = NULL,
	embedding_model = NULL,
    revision = revision + 1,
    updated_at = now()
WHERE id = $7 AND revision = $8`

const (
	deleteUserSiteTagsSQL = "DELETE FROM site_tags WHERE site_id = $1 AND source = 'user' AND normalized_tag = ANY($2::text[])"
	upsertUserSiteTagSQL  = "INSERT INTO site_tags (site_id, tag, normalized_tag, source, created_at, updated_at) VALUES ($1, $2, $3, 'user', NOW(), NOW()) ON CONFLICT (site_id, normalized_tag) DO UPDATE SET tag = EXCLUDED.tag, source = 'user', updated_at = NOW()"
)

func (r *PGXSiteRepository) UpdateSiteProfile(ctx context.Context, params UpdateSiteProfileParams) (bool, error) {
	result, err := r.db.Exec(ctx, updateSiteProfileSQL, params.Name, params.Intro, params.HomepageURL, params.IconURL, params.UserNote, params.Pinned, params.ID, params.Revision)
	if err != nil {
		return false, fmt.Errorf("update site profile: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

func (r *PGXSiteRepository) UpdateSiteProfileAndTags(ctx context.Context, params UpdateSiteProfileParams) (bool, error) {
	tx, err := r.beginManagementTx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prelockRepresentationWriteGateShared(ctx, tx); err != nil {
		return false, err
	}
	result, err := tx.Exec(ctx, updateSiteProfileSQL, params.Name, params.Intro, params.HomepageURL, params.IconURL, params.UserNote, params.Pinned, params.ID, params.Revision)
	if err != nil {
		return false, fmt.Errorf("update site profile with tags: %w", err)
	}
	if result.RowsAffected() == 0 {
		return false, nil
	}
	if len(params.TagRemovals) > 0 {
		if _, err := tx.Exec(ctx, deleteUserSiteTagsSQL, params.ID, params.TagRemovals); err != nil {
			return false, fmt.Errorf("remove user site tags: %w", err)
		}
	}
	for _, tag := range params.TagAdds {
		if _, err := tx.Exec(ctx, upsertUserSiteTagSQL, params.ID, tag.Tag, tag.NormalizedTag); err != nil {
			return false, fmt.Errorf("upsert user site tag: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit site profile tag update: %w", err)
	}
	return true, nil
}

var _ SiteProfileWriter = (*PGXSiteRepository)(nil)
var _ SiteProfileTagWriter = (*PGXSiteRepository)(nil)
