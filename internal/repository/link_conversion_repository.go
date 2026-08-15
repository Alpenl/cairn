package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/model"
	"webtag/internal/siteidentity"
)

const (
	lockConvertibleLinkSQL = `SELECT url, COALESCE(title, ''), status, library_kind, content_revision
FROM links WHERE id = $1 FOR UPDATE`
	lockConvertibleLinkWithSummarySQL = `SELECT url, COALESCE(title, ''), COALESCE(summary, ''), status, library_kind, content_revision
FROM links WHERE id = $1 FOR UPDATE`
	deleteConversionContentSQL = `DELETE FROM link_translations WHERE link_id = $1`
	convertReadingToSiteSQL    = `UPDATE links SET requested_library_kind='site', requested_library_kind_source='user', library_kind='site', library_kind_source='user', library_kind_locked=true,
summary=NULL, content=NULL, content_cjk_chars=0, content_words=0, content_document=NULL, content_format='plain', content_source='fetched', input_text=NULL, input_html=NULL, input_images=NULL,
source_metadata=NULL, embedding=NULL, embedding_model=NULL, payload_purge_due_at=NULL, payload_purged_at=NOW(),
content_revision=content_revision+1, updated_at=NOW() WHERE id=$1`
	convertSiteToReadingSQL = `UPDATE links SET requested_library_kind='reading', requested_library_kind_source='user', library_kind='reading', library_kind_source='user', library_kind_locked=true,
content=NULL, content_cjk_chars=0, content_words=0, content_document=NULL, content_format='plain', content_source='fetched', embedding=NULL, embedding_model=NULL,
status='pending', error_msg=NULL, payload_purge_due_at=NULL, payload_purged_at=NOW(),
content_revision=content_revision+1, updated_at=NOW() WHERE id=$1`
	lockEntryByLinkSQL          = "SELECT site_id, id FROM site_entries WHERE link_id=$1 FOR UPDATE"
	deleteEntryByLinkSQL        = "DELETE FROM site_entries WHERE link_id=$1"
	advanceSiteForConversionSQL = "UPDATE sites SET embedding=NULL, embedding_model=NULL, revision=revision+1, updated_at=NOW() WHERE id=$1 AND revision=$2"
	appendConversionNoteSQL     = "UPDATE sites SET user_note = CASE WHEN $1 IS NULL OR $1 = '' THEN user_note WHEN user_note = '' THEN $1 ELSE user_note || E'\\n\\n' || $1 END, embedding=NULL, embedding_model=NULL, revision=revision+1, updated_at=NOW() WHERE id=$2 AND revision=$3"
	appendNewConversionNoteSQL  = "UPDATE sites SET user_note = CASE WHEN user_note = '' THEN $1 ELSE user_note || E'\\n\\n' || $1 END, embedding=NULL, embedding_model=NULL, revision=revision+1, updated_at=NOW() WHERE id=$2"
	copyLinkTagsToSiteSQL       = "INSERT INTO site_tags (site_id, tag, normalized_tag, source, created_at, updated_at) SELECT $1, tag, lower(tag), 'auto', NOW(), NOW() FROM unnest((SELECT tags FROM links WHERE id=$2)) AS tag WHERE btrim(tag) <> '' ON CONFLICT (site_id, normalized_tag) DO NOTHING"
)

// ConvertLink performs both collection transitions in one repository-owned
// transaction. Product callers use durablework.LinkCommands, which invokes
// ConvertLinkTx and inserts any resulting River job before committing.
func (r *PGXLinkRepository) ConvertLink(ctx context.Context, params ConvertLinkParams) (ConvertLinkResult, error) {
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return ConvertLinkResult{}, fmt.Errorf("begin link conversion transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := r.ConvertLinkTx(ctx, tx, params)
	if err != nil {
		return ConvertLinkResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ConvertLinkResult{}, fmt.Errorf("commit link conversion transaction: %w", err)
	}
	return result, nil
}

// ConvertLinkTx is the transaction-bound conversion implementation. The
// initial link lock repeats all service checks so a preview cannot race a
// refresh, delete, or another conversion. It never commits and does not know
// about River; a non-nil ParseJobID tells the durable adapter what to enqueue.
func (r *PGXLinkRepository) ConvertLinkTx(ctx context.Context, tx pgx.Tx, params ConvertLinkParams) (ConvertLinkResult, error) {
	if params.LinkID == uuid.Nil {
		return ConvertLinkResult{}, fmt.Errorf("convert link: link id is required")
	}
	if params.TargetKind != model.LibraryKindReading && params.TargetKind != model.LibraryKindSite {
		return ConvertLinkResult{}, fmt.Errorf("convert link: invalid target kind")
	}
	if err := prelockRepresentationWriteGateShared(ctx, tx); err != nil {
		return ConvertLinkResult{}, err
	}
	var url, title, summary, status, kind string
	var revision int64
	if err := tx.QueryRow(ctx, lockConvertibleLinkWithSummarySQL, params.LinkID).Scan(&url, &title, &summary, &status, &kind, &revision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ConvertLinkResult{}, ErrNotFound
		}
		return ConvertLinkResult{}, fmt.Errorf("lock conversion link: %w", err)
	}
	if status != string(model.LinkStatusDone) || kind == "" || revision != params.ExpectedContentRevision {
		return ConvertLinkResult{}, ErrRevisionConflict
	}
	if kind == string(params.TargetKind) {
		return ConvertLinkResult{}, ErrRevisionConflict
	}

	var (
		result ConvertLinkResult
		err    error
	)
	if params.TargetKind == model.LibraryKindSite {
		result, err = convertReadingToSiteOn(ctx, tx, params, url, title, summary)
	} else {
		result, err = convertSiteToReadingOn(ctx, tx, params)
	}
	if err != nil {
		return ConvertLinkResult{}, err
	}
	return result, nil
}

func convertReadingToSiteOn(ctx context.Context, tx pgx.Tx, params ConvertLinkParams, rawURL, title, intro string) (ConvertLinkResult, error) { //nolint:gocyclo // 同 convertSiteToReadingOn
	identity, err := siteidentity.FromURL(rawURL)
	if err != nil {
		return ConvertLinkResult{}, fmt.Errorf("derive site identity: %w", err)
	}
	// Freeze active Thoughts while the Link row is locked and before conversion
	// clears the saved document. Later sync may advance winner metadata, but the
	// historical presentation remains authoritative from this snapshot.
	if err := tombstoneReadingThoughtsForSiteConversion(ctx, tx, params.LinkID); err != nil {
		return ConvertLinkResult{}, err
	}
	if _, err := tx.Exec(ctx, deleteConversionContentSQL, params.LinkID); err != nil {
		return ConvertLinkResult{}, fmt.Errorf("delete conversion translations: %w", err)
	}
	if _, err := tx.Exec(ctx, convertReadingToSiteSQL, params.LinkID); err != nil {
		return ConvertLinkResult{}, fmt.Errorf("convert reading link to site: %w", err)
	}

	name := strings.TrimSpace(title)
	if name == "" {
		name = identity.Host
	}
	var siteID uuid.UUID
	var revision int64
	if params.TargetSiteID != nil {
		if err := tx.QueryRow(ctx, lockSiteForManagementSQL, *params.TargetSiteID).Scan(&revision, new(pgtype.UUID)); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ConvertLinkResult{}, ErrNotFound
			}
			return ConvertLinkResult{}, fmt.Errorf("lock target site: %w", err)
		}
		if params.ExpectedSiteRevision == nil || revision != *params.ExpectedSiteRevision {
			return ConvertLinkResult{}, ErrRevisionConflict
		}
		siteID = *params.TargetSiteID
	} else {
		aggregate, aggregateErr := aggregateSiteOn(ctx, tx, AggregateSiteParams{LinkID: params.LinkID, IdentityKey: identity.Key, NormalizedURL: identity.NormalizedURL, Name: name, Intro: intro, EntryName: name})
		if aggregateErr != nil {
			return ConvertLinkResult{}, aggregateErr
		}
		siteID = aggregate.SiteID
		if err := tx.QueryRow(ctx, "SELECT revision FROM sites WHERE id=$1", siteID).Scan(&revision); err != nil {
			return ConvertLinkResult{}, fmt.Errorf("read created site revision: %w", err)
		}
	}
	var entryID uuid.UUID
	if params.TargetSiteID != nil {
		// 同 aggregateSiteOn：site_entries 的第二个唯一索引 (site_id,
		// normalized_url) 不参与 ON CONFLICT 推断，先按它规避再插入。
		switch err := tx.QueryRow(ctx, findSiteEntryByNormalizedURLSQL, siteID, identity.NormalizedURL, params.LinkID).
			Scan(&entryID, &siteID); {
		case err == nil:
			if _, err := tx.Exec(ctx, touchSiteEntrySQL, entryID); err != nil {
				return ConvertLinkResult{}, fmt.Errorf("touch converted site entry: %w", err)
			}
		case errors.Is(err, pgx.ErrNoRows):
			if err := tx.QueryRow(ctx, insertSiteEntrySQL, siteID, params.LinkID, name, "", identity.NormalizedURL).Scan(&entryID, &siteID, new(bool)); err != nil {
				return ConvertLinkResult{}, fmt.Errorf("insert converted site entry: %w", err)
			}
		default:
			return ConvertLinkResult{}, fmt.Errorf("find converted site entry by normalized url: %w", err)
		}
		if _, err := tx.Exec(ctx, setPrimarySiteEntrySQL, entryID, siteID); err != nil {
			return ConvertLinkResult{}, fmt.Errorf("set converted site primary entry: %w", err)
		}
		if params.PreservedUserNote != nil {
			if tag, err := tx.Exec(ctx, appendConversionNoteSQL, *params.PreservedUserNote, siteID, revision); err != nil {
				return ConvertLinkResult{}, fmt.Errorf("append conversion note: %w", err)
			} else if tag.RowsAffected() == 0 {
				return ConvertLinkResult{}, ErrRevisionConflict
			}
		} else if tag, err := tx.Exec(ctx, advanceSiteForConversionSQL, siteID, revision); err != nil {
			return ConvertLinkResult{}, fmt.Errorf("advance target site revision: %w", err)
		} else if tag.RowsAffected() == 0 {
			return ConvertLinkResult{}, ErrRevisionConflict
		}
		revision++
	} else {
		if err := tx.QueryRow(ctx, "SELECT id FROM site_entries WHERE link_id=$1", params.LinkID).Scan(&entryID); err != nil {
			return ConvertLinkResult{}, fmt.Errorf("read created site entry: %w", err)
		}
		if params.PreservedUserNote != nil {
			if _, err := tx.Exec(ctx, appendNewConversionNoteSQL, *params.PreservedUserNote, siteID); err != nil {
				return ConvertLinkResult{}, fmt.Errorf("append new conversion note: %w", err)
			}
			revision++
		}
	}
	if _, err := tx.Exec(ctx, copyLinkTagsToSiteSQL, siteID, params.LinkID); err != nil {
		return ConvertLinkResult{}, fmt.Errorf("copy link tags to site: %w", err)
	}
	return ConvertLinkResult{LinkID: params.LinkID, Kind: model.LibraryKindSite, ContentRevision: params.ExpectedContentRevision + 1, Status: model.LinkStatusDone, SiteID: &siteID, SiteRevision: &revision, EntryID: &entryID}, nil
}

func tombstoneReadingThoughtsForSiteConversion(ctx context.Context, tx pgx.Tx, linkID uuid.UUID) error {
	// Keep the selection separate from the lifecycle write. A conversion must
	// not append another operation for a Thought that already has an immutable
	// tombstone for a different lifecycle reason.
	rows, err := tx.Query(ctx, `
		SELECT thought.id
		FROM reader_thoughts thought
		WHERE thought.host_kind='link' AND thought.host_id=$1
			AND thought.deleted=false
			AND NOT EXISTS (
				SELECT 1 FROM reader_thought_tombstones tombstone
				WHERE tombstone.thought_id=thought.id)
		ORDER BY thought.id
		FOR UPDATE`, linkID.String())
	if err != nil {
		return fmt.Errorf("lock conversion thoughts: %w", err)
	}
	thoughtIDs := make([]string, 0)
	for rows.Next() {
		var thoughtID string
		if err := rows.Scan(&thoughtID); err != nil {
			rows.Close()
			return fmt.Errorf("scan conversion thought: %w", err)
		}
		thoughtIDs = append(thoughtIDs, thoughtID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read conversion thoughts: %w", err)
	}

	// The lifecycle writer appends a deterministic reader-lifecycle operation,
	// materializes its next sync sequence, and freezes the complete tombstone
	// snapshot using this same conversion transaction.
	reader := NewPGXReaderVNextRepository(tx)
	for _, thoughtID := range thoughtIDs {
		if err := reader.markThoughtTombstoneOn(ctx, tx, thoughtID, "link_converted_to_site"); err != nil {
			return fmt.Errorf("tombstone converted link thought: %w", err)
		}
	}
	return nil
}

func convertSiteToReadingOn(ctx context.Context, tx pgx.Tx, params ConvertLinkParams) (ConvertLinkResult, error) { //nolint:gocyclo // 转换事务：清理派生数据 + 改写归属 + 重排解析，分支即清理项
	if params.ExpectedSiteRevision == nil {
		return ConvertLinkResult{}, ErrRevisionConflict
	}
	if err := detachSiteEntryForReadingOn(ctx, tx, params.LinkID, params.ExpectedSiteRevision, true); err != nil {
		return ConvertLinkResult{}, err
	}
	if _, err := tx.Exec(ctx, convertSiteToReadingSQL, params.LinkID); err != nil {
		return ConvertLinkResult{}, fmt.Errorf("convert site link to reading: %w", err)
	}
	row := tx.QueryRow(ctx, insertJobSQL, params.LinkID, parseJobsPerLinkRetention)
	job, err := scanJob(row)
	if err != nil {
		return ConvertLinkResult{}, fmt.Errorf("create conversion parse job: %w", err)
	}
	return ConvertLinkResult{LinkID: params.LinkID, Kind: model.LibraryKindReading, ContentRevision: params.ExpectedContentRevision + 1, Status: model.LinkStatusPending, ParseJobID: &job.ID}, nil
}

// detachSiteEntryForReadingOn removes the site aggregate edge before a link is
// exposed as reading. Manual conversion supplies an expected site revision;
// parse completion already serializes on the link/intent and therefore uses
// the current locked site revision.
func detachSiteEntryForReadingOn(
	ctx context.Context,
	tx pgx.Tx,
	linkID uuid.UUID,
	expectedRevision *int64,
	requireEntry bool,
) error {
	siteID, entryID, found, err := lockSiteEntryForReadingOn(ctx, tx, linkID, requireEntry)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	revision, primary, err := lockManagedSite(ctx, tx, siteID)
	if err != nil {
		return err
	}
	if expectedRevision != nil && revision != *expectedRevision {
		return ErrRevisionConflict
	}
	count, err := advanceSiteAfterEntryDetach(ctx, tx, siteID, entryID, revision, primary)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, deleteEntryByLinkSQL, linkID); err != nil {
		return fmt.Errorf("delete source site entry: %w", err)
	}
	if count == 1 {
		if _, err := tx.Exec(ctx, deleteManagedSiteSQL, siteID); err != nil {
			return fmt.Errorf("delete empty source site: %w", err)
		}
	}
	return nil
}

func lockSiteEntryForReadingOn(ctx context.Context, tx pgx.Tx, linkID uuid.UUID, requireEntry bool) (uuid.UUID, uuid.UUID, bool, error) {
	var siteID, entryID uuid.UUID
	err := tx.QueryRow(ctx, lockEntryByLinkSQL, linkID).Scan(&siteID, &entryID)
	if errors.Is(err, pgx.ErrNoRows) {
		if requireEntry {
			return uuid.Nil, uuid.Nil, false, ErrNotFound
		}
		return uuid.Nil, uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, false, fmt.Errorf("lock source site entry: %w", err)
	}
	return siteID, entryID, true, nil
}

func advanceSiteAfterEntryDetach(
	ctx context.Context,
	tx pgx.Tx,
	siteID, entryID uuid.UUID,
	revision int64,
	primary *uuid.UUID,
) (int, error) {
	var count int
	if err := tx.QueryRow(ctx, countSiteEntriesSQL, siteID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count source site entries: %w", err)
	}
	if count > 1 && primary != nil && *primary == entryID {
		var fallback uuid.UUID
		if err := tx.QueryRow(ctx, findFallbackPrimaryEntrySQL, siteID, entryID).Scan(&fallback); err != nil {
			return 0, fmt.Errorf("find conversion fallback primary: %w", err)
		}
		if tag, err := tx.Exec(ctx, clearManagedPrimaryEntrySQL, fallback, siteID, revision); err != nil {
			return 0, fmt.Errorf("set conversion fallback primary: %w", err)
		} else if tag.RowsAffected() == 0 {
			return 0, ErrRevisionConflict
		}
	} else if count == 1 {
		if tag, err := tx.Exec(ctx, clearManagedPrimaryEntrySQL, nil, siteID, revision); err != nil {
			return 0, fmt.Errorf("clear conversion primary: %w", err)
		} else if tag.RowsAffected() == 0 {
			return 0, ErrRevisionConflict
		}
	} else if tag, err := tx.Exec(ctx, advanceSiteForConversionSQL, siteID, revision); err != nil {
		return 0, fmt.Errorf("advance source site revision: %w", err)
	} else if tag.RowsAffected() == 0 {
		return 0, ErrRevisionConflict
	}
	return count, nil
}
