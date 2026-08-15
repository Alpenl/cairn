package repository

import (
	"context"
	"encoding/json"
	"fmt"
)

// readerArchiveSectionSQL is deliberately a closed map. Section names are
// selected by the service from a fixed list and never interpolated from a
// request. Every query exports the complete single-install Reader dataset.
var readerArchiveSectionSQL = map[string]string{
	"feed_folders": `
		SELECT jsonb_build_object(
			'id',id,'name',name,'created_at',created_at,'updated_at',updated_at
		)
		FROM feed_folders
		ORDER BY created_at ASC,id ASC`,
	"feed_subscriptions": `
		SELECT jsonb_build_object(
			'id',id,'folder_id',folder_id,'url',url,'canonical_url',canonical_url,
			'site_url',site_url,'title',title,'description',description,'active',active,
			'created_at',created_at,'updated_at',updated_at
		)
		FROM feed_subscriptions
		ORDER BY created_at ASC,id ASC`,
	"feed_items": `
		SELECT jsonb_build_object(
			'id',id,'subscription_id',subscription_id,'external_id',external_id,
			'url',url,'title',title,'author',author,'summary',summary,
			'content_text',content_text,'content_html',content_html,
			'published_at',published_at,'read_at',read_at,'starred',starred,
			'read_later',read_later,'link_id',link_id,
			'created_at',created_at,'updated_at',updated_at
		)
		FROM feed_items
		ORDER BY created_at ASC,id ASC`,
	"feed_saves": `
		SELECT jsonb_build_object(
			'feed_item_id',feed_item_id,'link_id',link_id,
			'created_link',created_link,'created_at',created_at
		)
		FROM reader_feed_saves
		ORDER BY created_at ASC,feed_item_id ASC`,
	"thoughts": `
		SELECT jsonb_build_object(
			'contract_version',1,
			'id',CASE WHEN tombstone.thought_id IS NULL THEN thought.id ELSE tombstone.snapshot->>'id' END,
			'host_kind',CASE WHEN tombstone.thought_id IS NULL THEN thought.host_kind ELSE tombstone.snapshot->>'host_kind' END,
			'host_id',CASE WHEN tombstone.thought_id IS NULL THEN thought.host_id ELSE tombstone.snapshot->>'host_id' END,
			'link_id',CASE WHEN tombstone.thought_id IS NULL THEN to_jsonb(thought.link_id) ELSE tombstone.snapshot->'link_id' END,
			'target',CASE WHEN tombstone.thought_id IS NULL THEN thought.target ELSE tombstone.snapshot->'target' END,
			'quote',CASE WHEN tombstone.thought_id IS NULL THEN thought.quote ELSE tombstone.snapshot->'quote' END,
			'body',CASE WHEN tombstone.thought_id IS NULL THEN thought.body ELSE tombstone.snapshot->>'body' END,
			'source',CASE WHEN tombstone.thought_id IS NULL THEN thought.source ELSE tombstone.snapshot->>'source' END,
			'original_host_snapshot',tombstone.snapshot->'original_host_snapshot',
			'lifecycle_status',CASE WHEN tombstone.thought_id IS NULL THEN 'active' ELSE 'tombstone' END,
			'lifecycle_reason',tombstone.reason,'tombstoned_at',tombstone.created_at,
			'deleted',thought.deleted,'last_sequence',thought.last_sequence,
			'winner_key',jsonb_build_object(
				'logical_clock',thought.winner_logical_clock,'device_id',thought.winner_device_id,'op_id',thought.winner_op_id
			),
			'created_at',CASE WHEN tombstone.thought_id IS NULL THEN to_jsonb(thought.created_at) ELSE tombstone.snapshot->'created_at' END,
			'updated_at',CASE WHEN tombstone.thought_id IS NULL THEN to_jsonb(thought.updated_at) ELSE tombstone.snapshot->'updated_at' END
		), tombstone.snapshot, thought.id::text
		FROM reader_thoughts thought
		LEFT JOIN reader_thought_tombstones tombstone ON tombstone.thought_id=thought.id
		WHERE tombstone.thought_id IS NULL OR tombstone.reason <> 'user_deleted'
		ORDER BY thought.updated_at DESC,thought.id DESC`,
	"thought_ops": `
		SELECT jsonb_build_object(
			'contract_version',1,
			'sequence',sequence,'op_id',op_id,'device_id',device_id,
			'logical_clock',logical_clock,
			'operation_kind',operation_kind,'annotation_id',annotation_id,
			'host_kind',host_kind,'host_id',host_id,'target',target,
			'payload',payload,'recovery_of',recovery_of,
			'expected_current_winner_key',expected_winner_key,'created_at',created_at
		)
		FROM reader_thought_ops
		WHERE NOT EXISTS (
			SELECT 1 FROM reader_thought_tombstones marker
			WHERE marker.thought_id=reader_thought_ops.annotation_id AND marker.reason='user_deleted'
		)
		ORDER BY sequence ASC`,
	"thought_supersession_events": `
		SELECT jsonb_build_object(
			'sequence',sequence,'annotation_id',annotation_id,
			'loser',loser,'winner_at_detection',winner_at_detection,
			'created_at',created_at
		)
		FROM reader_thought_supersession_events
		ORDER BY sequence ASC`,
	"thought_tombstones": `
		SELECT jsonb_build_object(
			'thought_id',thought_id,'host_kind',host_kind,'host_id',host_id,
			'reason',reason,'snapshot',snapshot,'created_at',created_at
		)
		FROM reader_thought_tombstones
		WHERE reason <> 'user_deleted'
		ORDER BY created_at DESC,thought_id DESC`,
	"notes": `
		SELECT jsonb_build_object(
			'id',id,'title',title,'published_content',published_content,
			'published_revision',published_revision,'draft_content',draft_content,
			'draft_revision',draft_revision,'draft_updated_at',draft_updated_at,
			'deleted_at',deleted_at,'created_at',created_at,'updated_at',updated_at
		)
		FROM reader_notes
		ORDER BY updated_at DESC,id DESC`,
	"note_history": `
		SELECT jsonb_build_object(
			'id',id,'note_id',note_id,'revision',revision,'title',title,
			'content',content,'reanchor_ops',reanchor_ops,'created_at',created_at
		)
		FROM reader_note_history
		ORDER BY note_id ASC,revision ASC`,
	"inbox": `
		SELECT jsonb_build_object(
			'id',id,'url',url,'source_kind',source_kind,'title',title,'body',body,
			'summary',summary,'suggested_tags',suggested_tags,'tags',tags,
			'status',status,'metadata_revision',metadata_revision,'job_id',job_id,
			'expires_at',expires_at,'expired_at',expired_at,'deleted_at',deleted_at,
			'created_at',created_at,'updated_at',updated_at
		)
		FROM reader_inbox
		ORDER BY updated_at DESC,id DESC`,
	"categories": `
		SELECT jsonb_build_object('id',id,'name',name,'created_at',created_at)
		FROM reader_categories
		ORDER BY created_at ASC,id ASC`,
	"categorizables": `
		SELECT jsonb_build_object(
			'category_id',category_id,'host_kind',host_kind,'host_id',host_id
		)
		FROM reader_categorizables
		ORDER BY category_id ASC,host_kind ASC,host_id ASC`,
	"todos": `
		SELECT jsonb_build_object(
			'id',id,'text',text,'due_at',due_at,'done',done,
			'origin_kind',origin_kind,'origin_host_kind',origin_host_kind,
			'origin_host_id',origin_host_id,'origin_ref',origin_ref,
			'host_revision',host_revision,'completed_at',completed_at,
			'deleted_at',deleted_at,'created_at',created_at,'updated_at',updated_at
		)
		FROM reader_todos
		ORDER BY updated_at DESC,id DESC`,
	"engagement": `
		SELECT jsonb_build_object(
			'link_id',link_id,'read',read,'progress',progress,
			'read_later',read_later,'last_opened',last_opened,'updated_at',updated_at
		)
		FROM reader_engagement
		ORDER BY updated_at DESC,link_id ASC`,
	"feed_feedback": `
		SELECT jsonb_build_object(
			'item_key',item_key,'action',action,'created_at',created_at
		)
		FROM reader_feed_feedback
		ORDER BY created_at DESC,item_key ASC`,
	"feed_snapshots": `
		SELECT jsonb_build_object(
			'id',id,'mode',mode,'items',items,'created_at',created_at
		)
		FROM reader_feed_snapshots
		ORDER BY created_at DESC,id DESC`,
	"tag_activity": `
		SELECT jsonb_build_object('tag',tag,'last_at',last_at,'last_link_id',last_link_id)
		FROM reader_tag_activity
		ORDER BY last_at DESC,tag ASC`,
	"domain_activity": `
		SELECT jsonb_build_object('domain',domain,'last_at',last_at,'last_link_id',last_link_id)
		FROM reader_domain_activity
		ORDER BY last_at DESC,domain ASC`,
	"content_history": `
		SELECT jsonb_build_object(
			'id',id,'link_id',link_id,'revision',revision,'content',content,
			'content_document',content_document,'content_format',content_format,
			'content_source',content_source,'created_at',created_at
		)
		FROM reader_content_history
		ORDER BY link_id ASC,revision ASC`,
}

func validateReaderArchiveThoughtSnapshot(thoughtID string, snapshot []byte) error {
	if len(snapshot) == 0 {
		return nil
	}
	return validateReaderThoughtReplaySnapshot(thoughtID, snapshot)
}

func validateReaderArchiveThoughtTombstoneSnapshot(raw []byte) error {
	var item struct {
		ThoughtID string          `json:"thought_id"`
		Snapshot  json.RawMessage `json:"snapshot"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return ErrInvalidReaderThought
	}
	return validateReaderThoughtReplaySnapshot(item.ThoughtID, item.Snapshot)
}

// StreamReaderArchiveSection emits one JSON object for each row in a known
// Reader section. The callback runs while rows are open, so callers
// can write a response incrementally and cancel the database query through ctx.
func (r *PGXReaderVNextRepository) StreamReaderArchiveSection(
	ctx context.Context,
	section string,
	yield func([]byte) error,
) error {
	sql, ok := readerArchiveSectionSQL[section]
	if !ok || yield == nil {
		return fmt.Errorf("unknown reader archive section %q", section)
	}
	rows, err := r.db.Query(ctx, sql)
	if err != nil {
		return fmt.Errorf("stream reader archive %s: %w", section, err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var snapshot []byte
		var thoughtID string
		var err error
		switch section {
		case "thoughts":
			err = rows.Scan(&raw, &snapshot, &thoughtID)
		default:
			err = rows.Scan(&raw)
		}
		if err != nil {
			return fmt.Errorf("scan reader archive %s: %w", section, err)
		}
		if !json.Valid(raw) {
			return fmt.Errorf("reader archive %s returned invalid JSON", section)
		}
		switch section {
		case "thoughts":
			if err := validateReaderArchiveThoughtSnapshot(thoughtID, snapshot); err != nil {
				return fmt.Errorf("validate reader archive thought snapshot: %w", err)
			}
		case "thought_tombstones":
			if err := validateReaderArchiveThoughtTombstoneSnapshot(raw); err != nil {
				return fmt.Errorf("validate reader archive thought tombstone snapshot: %w", err)
			}
		}
		if err := yield(append([]byte(nil), raw...)); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read reader archive %s: %w", section, err)
	}
	return nil
}

var _ ReaderArchiveReader = (*PGXReaderVNextRepository)(nil)
