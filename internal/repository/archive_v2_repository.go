package repository

import (
	"context"
	"fmt"
)

// archiveV2SiteSectionSQL is the wire projection for the non-Reader archive
// relations. Do not use to_jsonb(table) here: adding a storage column must not
// silently expand the frozen browser archive contract or expose storage-only
// fields and embeddings.
var archiveV2SiteSectionSQL = map[string]string{
	"sites": `SELECT jsonb_build_object(
		'id',s.id,'site_key',s.site_key,'name',s.name,'name_source',s.name_source,
		'intro',s.intro,'intro_source',s.intro_source,
		'homepage_url',s.homepage_url,'homepage_source',s.homepage_source,
		'icon_url',s.icon_url,'icon_source',s.icon_source,
		'user_note',s.user_note,'pinned',s.pinned,'primary_entry_id',s.primary_entry_id,
		'primary_source',s.primary_source,'grouping_locked',s.grouping_locked,
		'needs_review',s.needs_review,'revision',s.revision,
		'first_collected_at',s.first_collected_at,'last_collected_at',s.last_collected_at,
		'created_at',s.created_at,'updated_at',s.updated_at
		)::text FROM sites s ORDER BY s.created_at,s.id`,
	"site_entries": `SELECT jsonb_build_object(
		'id',se.id,'site_id',se.site_id,'link_id',se.link_id,
		'entry_name',se.entry_name,'entry_name_source',se.entry_name_source,
		'purpose',se.purpose,'purpose_source',se.purpose_source,
		'normalized_url',se.normalized_url,'first_collected_at',se.first_collected_at,
		'last_recollected_at',se.last_recollected_at,'created_at',se.created_at,
		'updated_at',se.updated_at
		)::text FROM site_entries se ORDER BY se.created_at,se.id`,
	"site_tags": `SELECT jsonb_build_object(
		'site_id',st.site_id,'tag',st.tag,'normalized_tag',st.normalized_tag,
		'source',st.source,'concept_id',st.concept_id,'created_at',st.created_at,
		'updated_at',st.updated_at
		)::text FROM site_tags st ORDER BY st.created_at,st.site_id,st.normalized_tag`,
	"site_identities": `SELECT jsonb_build_object(
		'identity_key',si.identity_key,'site_id',si.site_id,'source',si.source,
		'locked',si.locked,'created_at',si.created_at,'updated_at',si.updated_at
		)::text FROM site_identities si ORDER BY si.created_at,si.identity_key`,
}

// StreamArchiveV2Section streams normalized JSON rows without accumulating a
// relation in memory. Each selected column is part of the frozen v2 wire
// projection and intentionally excludes embeddings and other storage-only data.
func (r *PGXSiteRepository) StreamArchiveV2Section(ctx context.Context, section string, yield func([]byte) error) error {
	query, ok := archiveV2SiteSectionSQL[section]
	if !ok {
		return fmt.Errorf("unsupported archive section %q", section)
	}
	rows, err := r.db.Query(ctx, query)
	if err != nil {
		return fmt.Errorf("stream archive %s: %w", section, err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return fmt.Errorf("scan archive %s: %w", section, err)
		}
		if err := yield([]byte(raw)); err != nil {
			return err
		}
	}
	return rows.Err()
}
