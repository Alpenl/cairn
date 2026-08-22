package repository

import (
	"context"
	"fmt"
)

const updateSiteProfileSQL = `UPDATE sites
SET name = COALESCE($1, name),
    intro = COALESCE($2, intro),
    homepage_url = COALESCE($3, homepage_url),
    icon_url = COALESCE($4, icon_url),
    user_note = COALESCE($5, user_note),
    pinned = COALESCE($6, pinned),
    revision = revision + 1,
    updated_at = now()
WHERE id = $7 AND revision = $8`

const (
	deleteSiteTagsSQL = "DELETE FROM site_tags WHERE site_id = $1 AND normalized_tag = ANY($2::text[])"
	upsertSiteTagSQL  = "INSERT INTO site_tags (site_id, tag, normalized_tag, created_at, updated_at) VALUES ($1, $2, $3, NOW(), NOW()) ON CONFLICT (site_id, normalized_tag) DO UPDATE SET tag = EXCLUDED.tag, updated_at = NOW()"
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
	result, err := tx.Exec(ctx, updateSiteProfileSQL, params.Name, params.Intro, params.HomepageURL, params.IconURL, params.UserNote, params.Pinned, params.ID, params.Revision)
	if err != nil {
		return false, fmt.Errorf("update site profile with tags: %w", err)
	}
	if result.RowsAffected() == 0 {
		return false, nil
	}
	if len(params.TagRemovals) > 0 {
		if _, err := tx.Exec(ctx, deleteSiteTagsSQL, params.ID, params.TagRemovals); err != nil {
			return false, fmt.Errorf("remove user site tags: %w", err)
		}
	}
	for _, tag := range params.TagAdds {
		if _, err := tx.Exec(ctx, upsertSiteTagSQL, params.ID, tag.Tag, tag.NormalizedTag); err != nil {
			return false, fmt.Errorf("upsert user site tag: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit site profile tag update: %w", err)
	}
	return true, nil
}
