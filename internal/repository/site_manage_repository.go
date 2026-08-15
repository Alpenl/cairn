package repository

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	lockSiteForManagementSQL = "SELECT revision, primary_entry_id FROM sites WHERE id = $1 FOR UPDATE"
	lockSiteEntrySQL         = "SELECT link_id FROM site_entries WHERE site_id = $1 AND id = $2 FOR UPDATE"
	updateSiteEntrySQL       = `UPDATE site_entries
SET entry_name = COALESCE($1, entry_name),
    entry_name_source = CASE WHEN $1 IS NULL THEN entry_name_source ELSE 'user' END,
    purpose = COALESCE($2, purpose),
    purpose_source = CASE WHEN $2 IS NULL THEN purpose_source ELSE 'user' END,
    updated_at = NOW()
WHERE site_id = $3 AND id = $4`
	updateSiteManagementRevisionSQL = "UPDATE sites SET embedding = NULL, embedding_model = NULL, revision = revision + 1, updated_at = NOW() WHERE id = $1 AND revision = $2"
	setManagedPrimaryEntrySQL       = "UPDATE sites SET primary_entry_id = $1, primary_source = 'user', revision = revision + 1, updated_at = NOW() WHERE id = $2 AND revision = $3"
	findFallbackPrimaryEntrySQL     = "SELECT id FROM site_entries WHERE site_id = $1 AND id <> $2 ORDER BY first_collected_at, id LIMIT 1"
	countSiteEntriesSQL             = "SELECT count(*) FROM site_entries WHERE site_id = $1"
	clearManagedPrimaryEntrySQL     = "UPDATE sites SET primary_entry_id = $1, primary_source = 'user', embedding = NULL, embedding_model = NULL, revision = revision + 1, updated_at = NOW() WHERE id = $2 AND revision = $3"
	deleteManagedLinkSQL            = "DELETE FROM links WHERE id = $1"
	deleteManagedSiteSQL            = "DELETE FROM sites WHERE id = $1"
	deleteManagedSiteLinksSQL       = "DELETE FROM links WHERE id IN (SELECT link_id FROM site_entries WHERE site_id = $1)"
	mergeSourceEntriesSQL           = "SELECT id, link_id, normalized_url FROM site_entries WHERE site_id=$1 FOR UPDATE"
	mergeTargetURLExistsSQL         = "SELECT EXISTS(SELECT 1 FROM site_entries WHERE site_id=$1 AND normalized_url=$2)"
	mergeMoveEntrySQL               = "UPDATE site_entries SET site_id=$1, updated_at=NOW() WHERE id=$2"
	mergeUserTagsSQL                = "INSERT INTO site_tags (site_id, tag, normalized_tag, source, concept_id, created_at, updated_at) SELECT $1, tag, normalized_tag, 'user', concept_id, NOW(), NOW() FROM site_tags WHERE site_id=$2 AND source='user' ON CONFLICT (site_id, normalized_tag) DO UPDATE SET source='user', updated_at=NOW()"
	mergeIdentitiesSQL              = "UPDATE site_identities SET site_id=$1, source='manual_merge', locked=true, updated_at=NOW() WHERE site_id=$2"
	mergeTargetSQL                  = "UPDATE sites SET name=COALESCE($1,name), name_source=CASE WHEN $1 IS NULL THEN name_source ELSE 'user' END, intro=COALESCE($2,intro), intro_source=CASE WHEN $2 IS NULL THEN intro_source ELSE 'user' END, homepage_url=COALESCE($3,homepage_url), homepage_source=CASE WHEN $3 IS NULL THEN homepage_source ELSE 'user' END, icon_url=COALESCE($4,icon_url), icon_source=CASE WHEN $4 IS NULL THEN icon_source ELSE 'user' END, user_note=COALESCE($5,user_note), grouping_locked=true, embedding=NULL, embedding_model=NULL, revision=revision+1, updated_at=NOW(), last_collected_at=NOW() WHERE id=$6 AND revision=$7"
	splitCountEntriesSQL            = "SELECT count(*) FROM site_entries WHERE site_id=$1"
	splitLockSelectedEntriesSQL     = "SELECT id FROM site_entries WHERE site_id=$1 AND id=ANY($2::uuid[]) FOR UPDATE"
	splitInsertSiteSQL              = "INSERT INTO sites (site_key, name, name_source, intro, intro_source, homepage_url, homepage_source, icon_url, icon_source, user_note, primary_entry_id, primary_source, grouping_locked, first_collected_at, last_collected_at, created_at, updated_at) VALUES ($1,$2,'user',COALESCE($3,''),'user',$4,CASE WHEN $4 IS NULL THEN NULL ELSE 'user' END,$5,CASE WHEN $5 IS NULL THEN NULL ELSE 'user' END,COALESCE($6,''),NULL,'user',true,NOW(),NOW(),NOW(),NOW()) RETURNING id"
	splitMoveEntriesSQL             = "UPDATE site_entries SET site_id=$1, updated_at=NOW() WHERE site_id=$2 AND id=ANY($3::uuid[])"
	splitSetPrimarySQL              = "UPDATE sites SET primary_entry_id=$1, primary_source='user', revision=revision+1, updated_at=NOW() WHERE id=$2"
	splitUpdateSourceSQL            = "UPDATE sites SET primary_entry_id=CASE WHEN primary_entry_id=ANY($1::uuid[]) THEN (SELECT id FROM site_entries WHERE site_id=$2 ORDER BY first_collected_at,id LIMIT 1) ELSE primary_entry_id END, grouping_locked=true, embedding=NULL, embedding_model=NULL, revision=revision+1, updated_at=NOW() WHERE id=$2 AND revision=$3"
	splitCopyUserTagsSQL            = "INSERT INTO site_tags (site_id,tag,normalized_tag,source,concept_id,created_at,updated_at) SELECT $1,tag,normalized_tag,'user',concept_id,NOW(),NOW() FROM site_tags WHERE site_id=$2 AND source='user' ON CONFLICT (site_id,normalized_tag) DO NOTHING"
	splitMoveIdentitySQL            = "UPDATE site_identities SET site_id=$1, source='manual_split', locked=true, updated_at=NOW() WHERE site_id=$2 AND identity_key=$3"
)

func (r *PGXSiteRepository) beginManagementTx(ctx context.Context) (pgx.Tx, error) {
	beginner, ok := r.db.(txBeginner)
	if !ok {
		return nil, fmt.Errorf("site management requires transactional database")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin site management transaction: %w", err)
	}
	return tx, nil
}

func lockManagedSite(ctx context.Context, tx pgx.Tx, siteID uuid.UUID) (int64, *uuid.UUID, error) {
	var revision int64
	var primary pgtype.UUID
	if err := tx.QueryRow(ctx, lockSiteForManagementSQL, siteID).Scan(&revision, &primary); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil, ErrNotFound
		}
		return 0, nil, fmt.Errorf("lock site for management: %w", err)
	}
	return revision, uuidPointer(primary), nil
}

func ensureManagedRevision(actual, expected int64) error {
	if actual != expected {
		return ErrRevisionConflict
	}
	return nil
}

func (r *PGXSiteRepository) UpdateSiteEntry(ctx context.Context, params UpdateSiteEntryParams) (bool, error) {
	tx, err := r.beginManagementTx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prelockRepresentationWriteGateShared(ctx, tx); err != nil {
		return false, err
	}
	revision, _, err := lockManagedSite(ctx, tx, params.SiteID)
	if err != nil {
		return false, err
	}
	if err := ensureManagedRevision(revision, params.Revision); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, updateSiteEntrySQL, params.Name, params.Purpose, params.SiteID, params.EntryID)
	if err != nil {
		return false, fmt.Errorf("update site entry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, ErrSiteEntryNotFound
	}
	if _, err := tx.Exec(ctx, updateSiteManagementRevisionSQL, params.SiteID, params.Revision); err != nil {
		return false, fmt.Errorf("advance site revision after entry update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit site entry update: %w", err)
	}
	return true, nil
}

func (r *PGXSiteRepository) SetSitePrimaryEntry(ctx context.Context, params SetSitePrimaryEntryParams) (bool, error) {
	tx, err := r.beginManagementTx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prelockRepresentationWriteGateShared(ctx, tx); err != nil {
		return false, err
	}
	revision, _, err := lockManagedSite(ctx, tx, params.SiteID)
	if err != nil {
		return false, err
	}
	if err := ensureManagedRevision(revision, params.Revision); err != nil {
		return false, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM site_entries WHERE site_id = $1 AND id = $2)", params.SiteID, params.EntryID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check site primary entry: %w", err)
	}
	if !exists {
		return false, ErrSiteEntryNotFound
	}
	tag, err := tx.Exec(ctx, setManagedPrimaryEntrySQL, params.EntryID, params.SiteID, params.Revision)
	if err != nil {
		return false, fmt.Errorf("set managed primary entry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, ErrRevisionConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit primary entry update: %w", err)
	}
	return true, nil
}

func (r *PGXSiteRepository) DeleteSiteEntry(ctx context.Context, params DeleteSiteEntryParams) (SiteEntryDeleteResult, error) { //nolint:gocyclo // 删除条目需级联处理主条目改选与空站点回收
	tx, err := r.beginManagementTx(ctx)
	if err != nil {
		return SiteEntryDeleteResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prelockLibraryFeedRevisions(ctx, tx); err != nil {
		return SiteEntryDeleteResult{}, err
	}
	revision, primary, err := lockManagedSite(ctx, tx, params.SiteID)
	if err != nil {
		return SiteEntryDeleteResult{}, err
	}
	if err := ensureManagedRevision(revision, params.Revision); err != nil {
		return SiteEntryDeleteResult{}, err
	}
	var linkID uuid.UUID
	if err := tx.QueryRow(ctx, lockSiteEntrySQL, params.SiteID, params.EntryID).Scan(&linkID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SiteEntryDeleteResult{}, ErrSiteEntryNotFound
		}
		return SiteEntryDeleteResult{}, fmt.Errorf("lock site entry for deletion: %w", err)
	}
	var remaining int
	if err := tx.QueryRow(ctx, countSiteEntriesSQL, params.SiteID).Scan(&remaining); err != nil {
		return SiteEntryDeleteResult{}, fmt.Errorf("count site entries before deletion: %w", err)
	}
	if remaining > 1 && primary != nil && *primary == params.EntryID {
		var fallback uuid.UUID
		if err := tx.QueryRow(ctx, findFallbackPrimaryEntrySQL, params.SiteID, params.EntryID).Scan(&fallback); err != nil {
			return SiteEntryDeleteResult{}, fmt.Errorf("find fallback primary entry: %w", err)
		}
		if _, err := tx.Exec(ctx, clearManagedPrimaryEntrySQL, fallback, params.SiteID, params.Revision); err != nil {
			return SiteEntryDeleteResult{}, fmt.Errorf("set fallback primary entry: %w", err)
		}
	} else if remaining == 1 {
		// Clear the deferred FK before the link deletion cascades the entry.
		if _, err := tx.Exec(ctx, clearManagedPrimaryEntrySQL, nil, params.SiteID, params.Revision); err != nil {
			return SiteEntryDeleteResult{}, fmt.Errorf("clear final primary entry: %w", err)
		}
	} else if _, err := tx.Exec(ctx, updateSiteManagementRevisionSQL, params.SiteID, params.Revision); err != nil {
		return SiteEntryDeleteResult{}, fmt.Errorf("advance site revision before entry deletion: %w", err)
	}
	if tag, err := tx.Exec(ctx, deleteManagedLinkSQL, linkID); err != nil {
		return SiteEntryDeleteResult{}, fmt.Errorf("delete entry link: %w", err)
	} else if tag.RowsAffected() == 0 {
		return SiteEntryDeleteResult{}, ErrSiteEntryNotFound
	}
	result := SiteEntryDeleteResult{}
	if remaining == 1 {
		if tag, err := tx.Exec(ctx, deleteManagedSiteSQL, params.SiteID); err != nil {
			return result, fmt.Errorf("delete empty site: %w", err)
		} else if tag.RowsAffected() == 0 {
			return result, ErrNotFound
		}
		result.DeletedSite = true
	}
	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("commit site entry deletion: %w", err)
	}
	return result, nil
}

func (r *PGXSiteRepository) DeleteSite(ctx context.Context, params DeleteSiteParams) (bool, error) {
	tx, err := r.beginManagementTx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prelockLibraryFeedRevisions(ctx, tx); err != nil {
		return false, err
	}
	revision, _, err := lockManagedSite(ctx, tx, params.ID)
	if err != nil {
		return false, err
	}
	if err := ensureManagedRevision(revision, params.Revision); err != nil {
		return false, err
	}
	var count int
	if err := tx.QueryRow(ctx, countSiteEntriesSQL, params.ID).Scan(&count); err != nil {
		return false, fmt.Errorf("count site entries for deletion: %w", err)
	}
	if count != params.ConfirmEntryCount {
		return false, ErrRevisionConflict
	}
	if _, err := tx.Exec(ctx, deleteManagedSiteLinksSQL, params.ID); err != nil {
		return false, fmt.Errorf("delete site links: %w", err)
	}
	tag, err := tx.Exec(ctx, deleteManagedSiteSQL, params.ID)
	if err != nil {
		return false, fmt.Errorf("delete site: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit site deletion: %w", err)
	}
	return true, nil
}

// ExecuteSiteMerge performs the destructive half of a merge only after the
// service has validated the preview resolutions. Every aggregate is locked in
// UUID order, then revision-checked before entries, tags, or identities move.
func (r *PGXSiteRepository) ExecuteSiteMerge(ctx context.Context, params ExecuteSiteMergeParams) (SiteMergeResult, error) { //nolint:gocyclo // 跨表事务：逐表搬迁 + 冲突裁决 + 版本校验，拆开会破坏事务边界的可读性
	if params.TargetID == uuid.Nil || params.TargetRevision < 1 || len(params.Sources) == 0 {
		return SiteMergeResult{}, fmt.Errorf("invalid site merge parameters")
	}
	tx, err := r.beginManagementTx(ctx)
	if err != nil {
		return SiteMergeResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prelockLibraryFeedRevisions(ctx, tx); err != nil {
		return SiteMergeResult{}, err
	}
	expected := map[uuid.UUID]int64{params.TargetID: params.TargetRevision}
	ids := make([]uuid.UUID, 0, len(params.Sources)+1)
	ids = append(ids, params.TargetID)
	for _, source := range params.Sources {
		if source.ID == uuid.Nil || source.ID == params.TargetID || source.Revision < 1 {
			return SiteMergeResult{}, ErrRevisionConflict
		}
		if _, exists := expected[source.ID]; exists {
			return SiteMergeResult{}, ErrRevisionConflict
		}
		expected[source.ID] = source.Revision
		ids = append(ids, source.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	for _, id := range ids {
		actual, _, lockErr := lockManagedSite(ctx, tx, id)
		if lockErr != nil {
			return SiteMergeResult{}, lockErr
		}
		if actual != expected[id] {
			return SiteMergeResult{}, ErrRevisionConflict
		}
	}

	result := SiteMergeResult{SiteID: params.TargetID, Revision: params.TargetRevision + 1}
	for _, source := range params.Sources {
		rows, queryErr := tx.Query(ctx, mergeSourceEntriesSQL, source.ID)
		if queryErr != nil {
			return SiteMergeResult{}, fmt.Errorf("lock merge source entries: %w", queryErr)
		}
		type mergeEntry struct {
			id, linkID    uuid.UUID
			normalizedURL string
		}
		entries := []mergeEntry{}
		for rows.Next() {
			var entryID, linkID uuid.UUID
			var normalizedURL string
			if scanErr := rows.Scan(&entryID, &linkID, &normalizedURL); scanErr != nil {
				rows.Close()
				return SiteMergeResult{}, fmt.Errorf("scan merge source entry: %w", scanErr)
			}
			entries = append(entries, mergeEntry{id: entryID, linkID: linkID, normalizedURL: normalizedURL})
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			rows.Close()
			return SiteMergeResult{}, fmt.Errorf("iterate merge source entries: %w", rowsErr)
		}
		rows.Close()
		for _, entry := range entries {
			var duplicate bool
			if existsErr := tx.QueryRow(ctx, mergeTargetURLExistsSQL, params.TargetID, entry.normalizedURL).Scan(&duplicate); existsErr != nil {
				return SiteMergeResult{}, fmt.Errorf("check merge duplicate entry: %w", existsErr)
			}
			if duplicate {
				tag, deleteErr := tx.Exec(ctx, deleteManagedLinkSQL, entry.linkID)
				if deleteErr != nil {
					return SiteMergeResult{}, fmt.Errorf("delete duplicate merge link: %w", deleteErr)
				}
				if tag.RowsAffected() != 1 {
					return SiteMergeResult{}, ErrRevisionConflict
				}
				result.DeletedLinks++
				continue
			}
			if _, moveErr := tx.Exec(ctx, mergeMoveEntrySQL, params.TargetID, entry.id); moveErr != nil {
				return SiteMergeResult{}, fmt.Errorf("move merge entry: %w", moveErr)
			}
			result.MovedEntries++
		}
		if _, tagErr := tx.Exec(ctx, mergeUserTagsSQL, params.TargetID, source.ID); tagErr != nil {
			return SiteMergeResult{}, fmt.Errorf("merge user tags: %w", tagErr)
		}
		if _, identityErr := tx.Exec(ctx, mergeIdentitiesSQL, params.TargetID, source.ID); identityErr != nil {
			return SiteMergeResult{}, fmt.Errorf("move merge identities: %w", identityErr)
		}
	}
	tag, updateErr := tx.Exec(ctx, mergeTargetSQL, params.Name, params.Intro, params.HomepageURL, params.IconURL, params.UserNote, params.TargetID, params.TargetRevision)
	if updateErr != nil {
		return SiteMergeResult{}, fmt.Errorf("update merge target: %w", updateErr)
	}
	if tag.RowsAffected() != 1 {
		return SiteMergeResult{}, ErrRevisionConflict
	}
	for _, source := range params.Sources {
		tag, deleteErr := tx.Exec(ctx, deleteManagedSiteSQL, source.ID)
		if deleteErr != nil {
			return SiteMergeResult{}, fmt.Errorf("delete merged source site: %w", deleteErr)
		}
		if tag.RowsAffected() != 1 {
			return SiteMergeResult{}, ErrRevisionConflict
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SiteMergeResult{}, fmt.Errorf("commit site merge: %w", err)
	}
	return result, nil
}

func (r *PGXSiteRepository) ExecuteSiteSplit(ctx context.Context, params ExecuteSiteSplitParams) (SiteSplitResult, error) { //nolint:gocyclo // 同 ExecuteSiteMerge，跨表事务
	if params.SourceID == uuid.Nil || params.SourceRevision < 1 || len(params.EntryIDs) == 0 || params.PrimaryEntryID == uuid.Nil || strings.TrimSpace(params.Name) == "" {
		return SiteSplitResult{}, fmt.Errorf("invalid site split parameters")
	}
	tx, err := r.beginManagementTx(ctx)
	if err != nil {
		return SiteSplitResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prelockRepresentationWriteGateShared(ctx, tx); err != nil {
		return SiteSplitResult{}, err
	}
	revision, _, err := lockManagedSite(ctx, tx, params.SourceID)
	if err != nil {
		return SiteSplitResult{}, err
	}
	if revision != params.SourceRevision {
		return SiteSplitResult{}, ErrRevisionConflict
	}
	var total int
	if err := tx.QueryRow(ctx, splitCountEntriesSQL, params.SourceID).Scan(&total); err != nil {
		return SiteSplitResult{}, fmt.Errorf("count split source entries: %w", err)
	}
	rows, err := tx.Query(ctx, splitLockSelectedEntriesSQL, params.SourceID, params.EntryIDs)
	if err != nil {
		return SiteSplitResult{}, fmt.Errorf("lock split entries: %w", err)
	}
	selected := 0
	for rows.Next() {
		selected++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return SiteSplitResult{}, fmt.Errorf("iterate split entries: %w", err)
	}
	rows.Close()
	if selected != len(params.EntryIDs) || selected >= total {
		return SiteSplitResult{}, ErrRevisionConflict
	}
	var newID uuid.UUID
	if err := tx.QueryRow(ctx, splitInsertSiteSQL, "manual-split:"+uuid.NewString(), params.Name, params.Intro, params.HomepageURL, params.IconURL, params.UserNote).Scan(&newID); err != nil {
		return SiteSplitResult{}, fmt.Errorf("create split site: %w", err)
	}
	if tag, err := tx.Exec(ctx, splitMoveEntriesSQL, newID, params.SourceID, params.EntryIDs); err != nil || tag.RowsAffected() != int64(len(params.EntryIDs)) {
		if err != nil {
			return SiteSplitResult{}, fmt.Errorf("move split entries: %w", err)
		}
		return SiteSplitResult{}, ErrRevisionConflict
	}
	if _, err := tx.Exec(ctx, splitSetPrimarySQL, params.PrimaryEntryID, newID); err != nil {
		return SiteSplitResult{}, fmt.Errorf("set split primary entry: %w", err)
	}
	if tag, err := tx.Exec(ctx, splitUpdateSourceSQL, params.EntryIDs, params.SourceID, params.SourceRevision); err != nil || tag.RowsAffected() != 1 {
		if err != nil {
			return SiteSplitResult{}, fmt.Errorf("update split source: %w", err)
		}
		return SiteSplitResult{}, ErrRevisionConflict
	}
	if _, err := tx.Exec(ctx, splitCopyUserTagsSQL, newID, params.SourceID); err != nil {
		return SiteSplitResult{}, fmt.Errorf("copy split tags: %w", err)
	}
	if params.IdentityKeyForNewSite != nil {
		tag, err := tx.Exec(ctx, splitMoveIdentitySQL, newID, params.SourceID, *params.IdentityKeyForNewSite)
		if err != nil {
			return SiteSplitResult{}, fmt.Errorf("move split identity: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return SiteSplitResult{}, ErrRevisionConflict
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SiteSplitResult{}, fmt.Errorf("commit site split: %w", err)
	}
	return SiteSplitResult{SourceID: params.SourceID, SourceRevision: params.SourceRevision + 1, NewSiteID: newID, NewRevision: 1, MovedEntries: len(params.EntryIDs)}, nil
}

var _ SiteManagementWriter = (*PGXSiteRepository)(nil)
var _ SiteMergeWriter = (*PGXSiteRepository)(nil)
var _ SiteSplitWriter = (*PGXSiteRepository)(nil)
