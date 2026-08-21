package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/database"
	"webtag/internal/model"
)

const siteListSelect = `s.id, s.site_key, s.name, s.intro, s.homepage_url, s.icon_url, s.pinned, s.revision, s.first_collected_at, s.last_collected_at, s.primary_entry_id, pe.normalized_url, COUNT(DISTINCT e.id), COALESCE(array_agg(DISTINCT st.tag) FILTER (WHERE st.tag IS NOT NULL), '{}')`

type PGXSiteRepository struct{ db database.Querier }

const relatedReadingsSQL = `SELECT l.id, COALESCE(l.title, l.url), l.url, l.created_at
FROM links l
WHERE l.library_kind='reading' AND l.status='done'
  AND NOT EXISTS (SELECT 1 FROM site_entries se WHERE se.link_id=l.id)
  AND ((cardinality($1::text[]) > 0 AND lower(COALESCE(l.domain,''))=ANY($1::text[])) OR (cardinality($2::text[]) > 0 AND l.tags && $2::text[]))
ORDER BY CASE WHEN cardinality($1::text[]) > 0 AND lower(COALESCE(l.domain,''))=ANY($1::text[]) THEN 0 ELSE 1 END, l.created_at DESC, l.id DESC
LIMIT $3`

func NewPGXSiteRepository(db database.Querier) *PGXSiteRepository { return &PGXSiteRepository{db: db} }

func (r *PGXSiteRepository) ListSites(ctx context.Context, filter SiteListFilter) ([]SiteListItem, int, error) {
	where := []string{"TRUE"}
	args := []any{}
	pos := 1
	switch filter.View {
	case "pinned":
		where = append(where, "s.pinned = true")
	case "recent":
		if filter.RecentCutoff == nil {
			return nil, 0, fmt.Errorf("recent site list requires a cutoff")
		}
		where = append(where, fmt.Sprintf("s.last_collected_at >= $%d", pos))
		args = append(args, filter.RecentCutoff.UTC())
		pos++
	}
	if len(filter.Tags) > 0 {
		where = append(where, fmt.Sprintf("EXISTS (SELECT 1 FROM site_tags sf WHERE sf.site_id = s.id AND sf.normalized_tag = ANY($%d::text[]))", pos))
		args = append(args, filter.Tags)
		pos++
	}
	clause := strings.Join(where, " AND ")
	countSQL := "SELECT count(*) FROM sites s WHERE " + clause
	var total int
	if err := r.db.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count sites: %w", err)
	}
	order := "s.last_collected_at DESC, s.id DESC"
	if filter.View == "pinned" {
		order = "s.pinned DESC, s.updated_at DESC, s.id DESC"
	}
	query := "SELECT " + siteListSelect + " FROM sites s LEFT JOIN site_entries e ON e.site_id=s.id LEFT JOIN site_entries pe ON pe.id=s.primary_entry_id LEFT JOIN site_tags st ON st.site_id=s.id WHERE " + clause + " GROUP BY s.id, pe.normalized_url ORDER BY " + order + fmt.Sprintf(" LIMIT $%d OFFSET $%d", pos, pos+1)
	args = append(args, filter.Limit, filter.Offset)
	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list sites: %w", err)
	}
	defer rows.Close()
	items := []SiteListItem{}
	for rows.Next() {
		item, err := scanSiteListItem(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate sites: %w", err)
	}
	return items, total, nil
}

func (r *PGXSiteRepository) GetSite(ctx context.Context, id uuid.UUID) (*SiteDetail, error) {
	query := "SELECT " + siteListSelect + " FROM sites s LEFT JOIN site_entries e ON e.site_id=s.id LEFT JOIN site_entries pe ON pe.id=s.primary_entry_id LEFT JOIN site_tags st ON st.site_id=s.id WHERE s.id=$1 GROUP BY s.id, pe.normalized_url"
	item, err := scanSiteListItem(r.db.QueryRow(ctx, query, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	detail := &SiteDetail{SiteListItem: item}
	tagRows, err := r.db.Query(ctx, "SELECT site_id, tag, normalized_tag, created_at, updated_at FROM site_tags WHERE site_id=$1 ORDER BY normalized_tag", id)
	if err != nil {
		return nil, fmt.Errorf("list site tags: %w", err)
	}
	defer tagRows.Close()
	for tagRows.Next() {
		var tag model.SiteTag
		if err := tagRows.Scan(&tag.SiteID, &tag.Tag, &tag.NormalizedTag, &tag.CreatedAt, &tag.UpdatedAt); err != nil {
			return nil, err
		}
		detail.Tags = append(detail.Tags, tag)
	}
	if err := tagRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate site tags: %w", err)
	}
	entryRows, err := r.db.Query(ctx, "SELECT id, site_id, link_id, entry_name, purpose, normalized_url, first_collected_at, last_recollected_at, created_at, updated_at FROM site_entries WHERE site_id=$1 ORDER BY first_collected_at, id", id)
	if err != nil {
		return nil, fmt.Errorf("list site entries: %w", err)
	}
	defer entryRows.Close()
	for entryRows.Next() {
		var e model.SiteEntry
		var recollected pgtype.Timestamptz
		if err := entryRows.Scan(&e.ID, &e.SiteID, &e.LinkID, &e.EntryName, &e.Purpose, &e.NormalizedURL, &e.FirstCollectedAt, &recollected, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.LastRecollectedAt = timePointer(recollected)
		detail.Entries = append(detail.Entries, e)
	}
	if err := entryRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate site entries: %w", err)
	}
	identityRows, err := r.db.Query(ctx, "SELECT identity_key, site_id, created_at, updated_at FROM site_identities WHERE site_id=$1 ORDER BY identity_key", id)
	if err != nil {
		return nil, fmt.Errorf("list site identities: %w", err)
	}
	defer identityRows.Close()
	for identityRows.Next() {
		var identity model.SiteIdentity
		if err := identityRows.Scan(&identity.IdentityKey, &identity.SiteID, &identity.CreatedAt, &identity.UpdatedAt); err != nil {
			return nil, err
		}
		detail.Identities = append(detail.Identities, identity)
	}
	if err := identityRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate site identities: %w", err)
	}
	return detail, nil
}

func (r *PGXSiteRepository) FindByIdentityKey(ctx context.Context, identityKey string) (*SiteConversionCandidate, error) {
	var candidate SiteConversionCandidate
	err := r.db.QueryRow(ctx, `SELECT s.id, s.name, s.revision
FROM site_identities si JOIN sites s ON s.id=si.site_id
WHERE si.identity_key=$1`, identityKey).Scan(&candidate.ID, &candidate.Name, &candidate.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find site by identity key: %w", err)
	}
	return &candidate, nil
}

// ListRelatedReadings ranks concrete same-host readings first and tag-overlap
// candidates second. It intentionally never joins a site's entries as result
// rows: a reading remains independent even when it is related to a website.
func (r *PGXSiteRepository) ListRelatedReadings(ctx context.Context, hosts, tags []string, limit int) ([]RelatedReading, error) {
	if limit < 1 {
		limit = 6
	}
	if limit > 20 {
		limit = 20
	}
	if len(hosts) == 0 && len(tags) == 0 {
		return []RelatedReading{}, nil
	}
	rows, err := r.db.Query(ctx, relatedReadingsSQL, hosts, tags, limit)
	if err != nil {
		return nil, fmt.Errorf("list related readings: %w", err)
	}
	defer rows.Close()
	out := []RelatedReading{}
	for rows.Next() {
		var item RelatedReading
		if err := rows.Scan(&item.ID, &item.Title, &item.URL, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan related reading: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate related readings: %w", err)
	}
	return out, nil
}

func (r *PGXSiteRepository) SearchSites(ctx context.Context, query string, limit int) ([]SiteSearchMatch, int, error) {
	query = "%" + strings.ToLower(strings.TrimSpace(query)) + "%"
	if limit < 1 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	var total int
	if err := r.db.QueryRow(ctx, `SELECT count(DISTINCT s.id) FROM sites s WHERE lower(s.name) LIKE $1 OR lower(s.intro) LIKE $1 OR EXISTS (SELECT 1 FROM site_tags st WHERE st.site_id=s.id AND lower(st.tag) LIKE $1) OR EXISTS (SELECT 1 FROM site_entries se WHERE se.site_id=s.id AND (lower(se.entry_name) LIKE $1 OR lower(se.purpose) LIKE $1 OR lower(se.normalized_url) LIKE $1))`, query).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count site search: %w", err)
	}
	rows, err := r.db.Query(ctx, `WITH matching_sites AS (
	SELECT s.id, s.name, s.last_collected_at FROM sites s
	WHERE lower(s.name) LIKE $1 OR lower(s.intro) LIKE $1
		OR EXISTS (SELECT 1 FROM site_tags st WHERE st.site_id=s.id AND lower(st.tag) LIKE $1)
		OR EXISTS (SELECT 1 FROM site_entries se WHERE se.site_id=s.id AND (lower(se.entry_name) LIKE $1 OR lower(se.purpose) LIKE $1 OR lower(se.normalized_url) LIKE $1))
	ORDER BY s.last_collected_at DESC, s.id DESC
	LIMIT $2
)
SELECT ms.id, ms.name, se.id, se.entry_name, se.purpose, se.normalized_url
FROM matching_sites ms
LEFT JOIN site_entries se ON se.site_id=ms.id
ORDER BY ms.last_collected_at DESC, ms.id DESC, se.first_collected_at ASC`, query, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("search sites: %w", err)
	}
	defer rows.Close()
	byID := make(map[uuid.UUID]*SiteSearchMatch)
	order := make([]uuid.UUID, 0)
	for rows.Next() {
		var siteID uuid.UUID
		var entryID pgtype.UUID
		var name, entryName, purpose, url pgtype.Text
		if err := rows.Scan(&siteID, &name, &entryID, &entryName, &purpose, &url); err != nil {
			return nil, 0, fmt.Errorf("scan site search: %w", err)
		}
		match := byID[siteID]
		if match == nil {
			if len(order) >= limit {
				continue
			}
			match = &SiteSearchMatch{SiteID: siteID, SiteName: name.String}
			byID[siteID] = match
			order = append(order, siteID)
		}
		needle := strings.Trim(query, "%")
		if entryID.Valid && (strings.Contains(strings.ToLower(entryName.String), needle) || strings.Contains(strings.ToLower(purpose.String), needle) || strings.Contains(strings.ToLower(url.String), needle)) {
			match.MatchedEntries = append(match.MatchedEntries, SiteSearchEntry{ID: uuid.UUID(entryID.Bytes), Name: entryName.String, URL: url.String})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate site search: %w", err)
	}
	out := make([]SiteSearchMatch, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, total, nil
}

func scanSiteListItem(row interface{ Scan(...any) error }) (SiteListItem, error) {
	var item SiteListItem
	var home, icon, primary pgtype.Text
	if err := row.Scan(&item.ID, &item.SiteKey, &item.Name, &item.Intro, &home, &icon, &item.Pinned, &item.Revision, &item.FirstCollectedAt, &item.LastCollectedAt, &item.PrimaryEntryID, &primary, &item.EntryCount, &item.Tags); err != nil {
		return item, err
	}
	item.HomepageURL = textPointer(home)
	item.IconURL = textPointer(icon)
	item.PrimaryEntryURL = textPointer(primary)
	item.DisplayHost = siteHost(item.SiteKey)
	return item, nil
}
func siteHost(key string) string {
	const prefix = "v1:host:"
	if strings.HasPrefix(key, prefix) {
		return strings.TrimPrefix(key, prefix)
	}
	parts := strings.Split(key, ":")
	if len(parts) > 2 {
		return parts[1] + ":" + parts[2]
	}
	return key
}
