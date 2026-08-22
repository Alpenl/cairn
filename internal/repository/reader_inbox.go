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

	"webtag/internal/database"
	"webtag/internal/model"
)

func scanReaderInbox(row readerScanner) (*model.ReaderInbox, error) {
	var out model.ReaderInbox
	var title, summary, bodyDocument, bodyFormat pgtype.Text
	var expiresAt, deletedAt pgtype.Timestamptz
	if err := row.Scan(&out.ID, &out.URL, &out.IdentityKey, &out.SourceKind, &title, &out.Body, &bodyDocument, &bodyFormat, &out.Note, &summary, &out.SuggestedTags, &out.ProposalStatus, &out.Tags, &out.Status, &out.MetadataRevision, &expiresAt, &out.Expired, &deletedAt, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return nil, err
	}
	if bodyDocument.Valid {
		value := bodyDocument.String
		out.BodyDocument = &value
	}
	// A NULL/empty format reads as plain: rows written before the capture
	// document existed carry a flattened body and no structure.
	out.BodyFormat = model.ContentFormatPlain
	if bodyFormat.Valid && bodyFormat.String != "" {
		out.BodyFormat = model.ContentFormat(bodyFormat.String)
	}
	if title.Valid {
		value := title.String
		out.Title = &value
	}
	if summary.Valid {
		value := summary.String
		out.Summary = &value
	}
	if expiresAt.Valid {
		value := expiresAt.Time
		out.ExpiresAt = &value
	}
	if deletedAt.Valid {
		value := deletedAt.Time
		out.DeletedAt = &value
	}
	return &out, nil
}

// readerInboxPreviewLimit bounds the queue card text in runes. The card clamps
// to a few lines; anything past this is invisible in the UI and would only be
// list payload.
const readerInboxPreviewLimit = 280

// readerInboxPreviewSourceLimit is the character cut applied inside PostgreSQL
// so a 4 MiB body never crosses the wire on the list path. It is larger than
// the rendered bound because the raw prefix may be mostly whitespace.
const readerInboxPreviewSourceLimit = 2048

func scanReaderInboxListItem(row readerScanner) (*model.ReaderInboxListItem, error) {
	var out model.ReaderInboxListItem
	var title, preview pgtype.Text
	if err := row.Scan(&out.ID, &out.URL, &out.SourceKind, &title, &preview, &out.Tags, &out.Status, &out.MetadataRevision, &out.Expired, &out.UpdatedAt); err != nil {
		return nil, err
	}
	if title.Valid {
		value := title.String
		out.Title = &value
	}
	if preview.Valid {
		out.Preview = readerInboxPreview(preview.String)
	}
	return &out, nil
}

const readerInboxColumns = `id, url, identity_key, source_kind, title, body, body_document, body_format, note, summary, suggested_tags, proposal_status, tags, status, metadata_revision, expires_at, (expires_at IS NOT NULL AND expires_at <= NOW()) AS expired, deleted_at, created_at, updated_at`

const readerInboxColumnsQualified = `inbox.id, inbox.url, inbox.identity_key, inbox.source_kind, inbox.title, inbox.body, inbox.body_document, inbox.body_format, inbox.note, inbox.summary, inbox.suggested_tags, inbox.proposal_status, inbox.tags, inbox.status, inbox.metadata_revision, inbox.expires_at, (inbox.expires_at IS NOT NULL AND inbox.expires_at <= NOW()) AS expired, inbox.deleted_at, inbox.created_at, inbox.updated_at`

// readerInboxListColumns is the queue projection. It never selects body, note,
// suggested_tags: the card cannot render them, and selecting them made every
// Inbox open pay for a multi-megabyte transfer. The preview is cut inside
// PostgreSQL so the oversized column never leaves the server.
var readerInboxListColumns = fmt.Sprintf(
	`id, url, source_kind, title, left(COALESCE(NULLIF(btrim(summary), ''), NULLIF(btrim(note), ''), body), %d) AS preview, tags, status, metadata_revision, (expires_at IS NOT NULL AND expires_at <= NOW()) AS expired, updated_at`,
	readerInboxPreviewSourceLimit,
)

func (r *PGXReaderVNextRepository) CreateInbox(ctx context.Context, item model.ReaderInbox) (*model.ReaderInbox, error) {
	return r.createInboxOn(ctx, r.db, item)
}

// CreateInboxTx is the transaction-bound product write used by the durable
// Inbox command. It never commits independently of the proposal and River job.
func (r *PGXReaderVNextRepository) CreateInboxTx(ctx context.Context, tx pgx.Tx, item model.ReaderInbox) (*model.ReaderInbox, error) {
	return r.createInboxOn(ctx, tx, item)
}

func (r *PGXReaderVNextRepository) createInboxOn(ctx context.Context, db database.Querier, item model.ReaderInbox) (*model.ReaderInbox, error) {
	created, err := scanReaderInbox(db.QueryRow(ctx, `
		INSERT INTO reader_inbox (url,identity_key,source_kind,title,body,body_document,body_format,note,summary,suggested_tags,proposal_status,tags)
		VALUES ($1,$2,COALESCE(NULLIF($3,''),'url'),$4,$5,NULLIF($11::text,''),
			CASE WHEN NULLIF($11::text,'') IS NULL THEN 'plain' ELSE COALESCE(NULLIF($12::text,''),'plain') END,
			$6,$7,COALESCE($8::text[],'{}'::text[]),COALESCE(NULLIF($9,''),'idle'),COALESCE($10::text[],'{}'::text[]))
		RETURNING `+readerInboxColumns, item.URL, item.IdentityKey, item.SourceKind, item.Title, item.Body, item.Note, item.Summary, item.SuggestedTags, item.ProposalStatus, item.Tags, item.BodyDocument, string(item.BodyFormat)))
	if err != nil {
		return nil, fmt.Errorf("create inbox item: %w", err)
	}
	return created, nil
}

// ListInbox returns the narrow queue projection, not the detail record. The
// list is the Inbox first-screen read; the detail contract lives on
// GET /api/inbox/{id} so one oversized capture cannot make the queue expensive
// for every other row.
func (r *PGXReaderVNextRepository) ListInbox(ctx context.Context, partition model.ReaderInboxPartition, after string, limit int) ([]model.ReaderInboxListItem, int, int, string, error) {
	if !partition.Valid() {
		return nil, 0, 0, "", ErrReaderInboxStateConflict
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	at, id, err := parseReaderCursor(after)
	if err != nil {
		return nil, 0, 0, "", err
	}
	args := []any{}
	sql := `SELECT ` + readerInboxListColumns + ` FROM reader_inbox WHERE status='pending' AND deleted_at IS NULL`
	if partition == model.ReaderInboxPartitionActive {
		sql += ` AND (expires_at IS NULL OR expires_at > NOW())`
	} else {
		sql += ` AND expires_at IS NOT NULL AND expires_at <= NOW()`
	}
	if !at.IsZero() {
		parsed, parseErr := uuid.Parse(id)
		if parseErr != nil {
			return nil, 0, 0, "", fmt.Errorf("%w: invalid inbox cursor", ErrInvalidReaderCursor)
		}
		sql += fmt.Sprintf(` AND (updated_at < $%d OR (updated_at = $%d AND id < $%d))`, len(args)+1, len(args)+1, len(args)+2)
		args = append(args, at, parsed)
	}
	sql += fmt.Sprintf(` ORDER BY updated_at DESC,id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, 0, "", fmt.Errorf("list inbox: %w", err)
	}
	defer rows.Close()
	items := make([]model.ReaderInboxListItem, 0)
	for rows.Next() {
		item, err := scanReaderInboxListItem(rows)
		if err != nil {
			return nil, 0, 0, "", err
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, "", err
	}
	var activeCount, expiredCount int
	if err := r.db.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE expires_at IS NULL OR expires_at > NOW())::int,
			count(*) FILTER (WHERE expires_at IS NOT NULL AND expires_at <= NOW())::int
		FROM reader_inbox
		WHERE status='pending' AND deleted_at IS NULL`).Scan(&activeCount, &expiredCount); err != nil {
		return nil, 0, 0, "", fmt.Errorf("count inbox partitions: %w", err)
	}
	if len(items) == limit {
		last := items[len(items)-1]
		return items, activeCount, expiredCount, readerCursor(last.UpdatedAt, last.ID.String()), nil
	}
	return items, activeCount, expiredCount, "", nil
}

func (r *PGXReaderVNextRepository) GetInbox(ctx context.Context, id uuid.UUID) (*model.ReaderInbox, error) {
	item, err := scanReaderInbox(r.db.QueryRow(ctx, `SELECT `+readerInboxColumns+` FROM reader_inbox WHERE id=$1 AND deleted_at IS NULL`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get inbox: %w", err)
	}
	return item, nil
}

// GetInboxByURL provides the identity-keyed idempotency read used by non-library capture
// destinations. Trashed captures are intentionally excluded so a user can
// capture the same URL again after explicitly removing the old inbox item.
func (r *PGXReaderVNextRepository) GetInboxByURL(ctx context.Context, identityURL string) (*model.ReaderInbox, error) {
	item, err := scanReaderInbox(r.db.QueryRow(ctx, `
		SELECT `+readerInboxColumns+`
		FROM reader_inbox
		WHERE identity_key=$1
			AND deleted_at IS NULL
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, identityURL))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get inbox by url: %w", err)
	}
	return item, nil
}

func (r *PGXReaderVNextRepository) PatchInbox(ctx context.Context, patch model.ReaderInboxPatch) (*model.ReaderInbox, error) {
	item, err := scanReaderInbox(r.db.QueryRow(ctx, `
		UPDATE reader_inbox
		SET title=COALESCE($1,title),body=COALESCE($2,body),
			body_document=CASE WHEN $2::text IS NULL THEN body_document ELSE NULL END,
			body_format=CASE WHEN $2::text IS NULL THEN body_format ELSE 'plain' END,
			note=COALESCE($3,note),summary=COALESCE($4,summary),tags=COALESCE($5::text[],tags),
			proposal_status='idle',metadata_revision=metadata_revision+1,updated_at=NOW()
		WHERE id=$6 AND status='pending' AND deleted_at IS NULL AND metadata_revision=$7
		RETURNING `+readerInboxColumns, patch.Title, patch.Body, patch.Note, patch.Summary, patch.Tags, patch.ID, patch.ExpectedRevision))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRevisionConflict
	}
	if err != nil {
		return nil, fmt.Errorf("patch inbox: %w", err)
	}
	return item, nil
}

func (r *PGXReaderVNextRepository) DiscardInbox(ctx context.Context, id uuid.UUID) error {
	return r.withTx(ctx, func(db database.Querier) error {
		return r.discardInboxOn(ctx, db, id)
	})
}

// RestoreInbox restores either a trashed Inbox row or a pending row whose
// authoritative deadline has passed. Expiry restoration is Inbox-specific:
// the generic host lifecycle only knows deleted_at, while a user revival must
// establish a new 30-day deadline.
// It does not touch content, category membership, thoughts, or AI proposal
// fields. Retrying after the first successful restore is a no-op.
func (r *PGXReaderVNextRepository) RestoreInbox(ctx context.Context, id uuid.UUID) error {
	return r.withTx(ctx, func(db database.Querier) error {
		item, err := scanReaderInbox(db.QueryRow(ctx, `
			SELECT `+readerInboxColumns+`
			FROM reader_inbox
			WHERE id=$1
			FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock inbox for restore: %w", err)
		}

		switch item.Status {
		case "pending", "confirmed":
		default:
			return ErrReaderInboxStateConflict
		}
		// A confirmed capture may be restored from Trash, but confirmation does
		// not reopen its expired partition or change its saved-link ownership.
		renewExpiry := item.Status == "pending" && item.Expired
		needsTrashRestore := item.DeletedAt != nil
		if !renewExpiry && !needsTrashRestore {
			return nil
		}
		updated, err := scanReaderInbox(db.QueryRow(ctx, `
			UPDATE reader_inbox
			SET deleted_at=NULL,
				expires_at=CASE WHEN $2 THEN NOW() + INTERVAL '30 days' ELSE expires_at END,
				updated_at=NOW()
			WHERE id=$1
			RETURNING `+readerInboxColumns, id, renewExpiry))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("restore inbox: %w", err)
		}

		// Expiry does not tombstone thoughts. Only a previous trash transition
		// needs the normal host-lifecycle reattachment work.
		if item.DeletedAt != nil {
			if err := r.restoreReaderHostThoughts(ctx, db, model.ReaderHostInbox, id, updated.Body, updated.MetadataRevision); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *PGXReaderVNextRepository) discardInboxOn(ctx context.Context, db database.Querier, id uuid.UUID) error {
	item, err := scanReaderInbox(db.QueryRow(ctx, `SELECT `+readerInboxColumns+` FROM reader_inbox WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read inbox for discard: %w", err)
	}
	if item.Status != "pending" {
		return ErrReaderInboxStateConflict
	}
	if item.DeletedAt != nil {
		return nil
	}
	if err := updateReaderHostDeletedAt(ctx, db, model.ReaderHostInbox, id, true); err != nil {
		return err
	}
	if err := r.markThoughtHostTombstonesOn(ctx, db, "inbox", id.String(), readerHostTombstoneReason(model.ReaderHostInbox)); err != nil {
		return err
	}
	return nil
}

func (r *PGXReaderVNextRepository) ConfirmInbox(ctx context.Context, id uuid.UUID, expectedRevision *int64) (uuid.UUID, error) {
	var linkID uuid.UUID
	err := r.withTx(ctx, func(db database.Querier) error {
		result, err := r.confirmInboxOn(ctx, db, id, expectedRevision)
		if err == nil && result.LinkID != nil {
			linkID = *result.LinkID
		}
		return err
	})
	return linkID, err
}

func (r *PGXReaderVNextRepository) confirmInboxOn(ctx context.Context, db database.Querier, id uuid.UUID, expectedRevision *int64) (model.ReaderInboxBulkResult, error) {
	item, err := lockInboxForConfirmation(ctx, db, id, expectedRevision)
	if err != nil {
		return model.ReaderInboxBulkResult{}, err
	}
	identityURL, err := inboxConfirmationIdentity(*item)
	if err != nil {
		return model.ReaderInboxBulkResult{}, err
	}
	linkID, err := r.resolveInboxConfirmationLink(ctx, db, *item, identityURL)
	if err != nil {
		return model.ReaderInboxBulkResult{}, err
	}
	if err := finalizeInboxConfirmation(ctx, db, *item, linkID); err != nil {
		return model.ReaderInboxBulkResult{}, err
	}
	return model.ReaderInboxBulkResult{ID: id, Status: "confirmed", LinkID: linkID}, nil
}

func lockInboxForConfirmation(ctx context.Context, db database.Querier, id uuid.UUID, expectedRevision *int64) (*model.ReaderInbox, error) {
	item, err := scanReaderInbox(db.QueryRow(ctx, `SELECT `+readerInboxColumns+` FROM reader_inbox WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read inbox for confirmation: %w", err)
	}
	if item.Status != "pending" && item.Status != "confirmed" {
		return nil, ErrReaderInboxStateConflict
	}
	if expectedRevision != nil && item.MetadataRevision != *expectedRevision {
		return nil, ErrRevisionConflict
	}
	if item.Status == "pending" && (item.Title == nil || strings.TrimSpace(*item.Title) == "") {
		return nil, ErrReaderInboxTitleRequired
	}
	return item, nil
}

func inboxConfirmationIdentity(item model.ReaderInbox) (string, error) {
	identityURL := strings.TrimSpace(item.IdentityKey)
	if identityURL == "" {
		return "", fmt.Errorf("%w: inbox identity is empty", ErrReaderInboxStateConflict)
	}
	return identityURL, nil
}

func (r *PGXReaderVNextRepository) resolveInboxConfirmationLink(ctx context.Context, db database.Querier, item model.ReaderInbox, identityURL string) (*uuid.UUID, error) {
	if err := lockCanonicalLinkIdentity(ctx, db, identityURL); err != nil {
		return nil, err
	}
	matched, err := findCanonicalLink(ctx, db, identityURL)
	if err != nil {
		return nil, err
	}
	var (
		linkID   *uuid.UUID
		inserted bool
	)
	if matched == nil {
		linkID, inserted, err = insertInboxSavedLink(ctx, db, item, identityURL)
		if err != nil {
			return nil, err
		}
	} else {
		linkID = matched
	}
	if !inserted {
		if _, err := r.restoreLinkLifecycleOn(ctx, db, *linkID); err != nil {
			return nil, err
		}
	}
	// Confirmation is an explicit Library adoption. It permanently removes
	// Feed-exclusive lifecycle ownership even when the canonical Link was live.
	if _, err := db.Exec(ctx, `UPDATE links SET feed_managed=false,updated_at=NOW() WHERE id=$1 AND feed_managed=true`, *linkID); err != nil {
		return nil, fmt.Errorf("adopt confirmed inbox link: %w", err)
	}
	if !inserted && item.Status == "pending" {
		if err := mergeInboxDraftIntoLink(ctx, db, item, *linkID); err != nil {
			return nil, err
		}
	}
	return linkID, nil
}

func finalizeInboxConfirmation(ctx context.Context, db database.Querier, item model.ReaderInbox, linkID *uuid.UUID) error {
	if item.Status == "pending" {
		if _, err := db.Exec(ctx, `UPDATE reader_inbox SET status='confirmed',updated_at=NOW() WHERE id=$1 AND status='pending' AND deleted_at IS NULL`, item.ID); err != nil {
			return fmt.Errorf("confirm inbox: %w", err)
		}
	}
	return nil
}

// mergeInboxDraftIntoLink carries only user-owned draft data into an existing
// canonical link. AI proposal fields remain attached to the Inbox record and
// cannot replace library metadata after a late worker completion.
func mergeInboxDraftIntoLink(ctx context.Context, db database.Querier, item model.ReaderInbox, linkID uuid.UUID) error {
	// The body carries its structure with it. Replacing content alone would
	// leave the link's content_document and content_format describing the
	// previous body, and would move the text out from under a content_revision
	// the reader caches and anchors annotations against — so the whole content
	// triple is written together and the revision advances with it.
	_, err := db.Exec(ctx, `
		UPDATE links
		SET input_title=COALESCE(NULLIF($2,''),input_title),
			title=COALESCE(NULLIF($2,''),title),
			input_text=CASE WHEN $3 <> '' THEN $3 ELSE input_text END,
			content=CASE WHEN $3 <> '' THEN $3 ELSE content END,
			content_document=CASE WHEN $3 <> '' THEN NULLIF($5::text,'') ELSE content_document END,
			content_format=CASE WHEN $3 <> ''
				THEN CASE WHEN NULLIF($5::text,'') IS NULL THEN 'plain' ELSE COALESCE(NULLIF($6::text,''),'plain') END
				ELSE content_format END,
			content_revision=CASE WHEN $3 <> '' AND $3 IS DISTINCT FROM content THEN content_revision+1 ELSE content_revision END,
			tags=ARRAY(SELECT DISTINCT tag FROM unnest(COALESCE(tags,'{}'::text[]) || COALESCE($4::text[],'{}'::text[])) AS tag ORDER BY tag),
			updated_at=NOW()
		WHERE id=$1`, linkID, item.Title, item.Body, item.Tags, item.BodyDocument, string(item.BodyFormat))
	if err != nil {
		return fmt.Errorf("merge inbox draft into link: %w", err)
	}
	return nil
}

// findCanonicalLink resolves the canonical record for a normalized URL before
// falling back to the raw source_key/url match, so a confirmation reuses the
// same row /api/links would have reused rather than inserting alongside it.
var findCanonicalLinkSQL = "SELECT id FROM links WHERE " +
	"(" + canonicalLinkMatch("$1") + " OR source_key=$1 OR url=$1) ORDER BY " +
	canonicalLinkMatch("$1") + " DESC, (source_key=$1) DESC, created_at ASC, id ASC LIMIT 1 FOR UPDATE"

func lockCanonicalLinkIdentity(ctx context.Context, db database.Querier, identity string) error {
	if _, err := db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "canonical-link:"+identity); err != nil {
		return fmt.Errorf("lock canonical link identity: %w", err)
	}
	return nil
}

func findCanonicalLink(ctx context.Context, db database.Querier, rawURL string) (*uuid.UUID, error) {
	var linkID uuid.UUID
	err := db.QueryRow(ctx, findCanonicalLinkSQL, rawURL).Scan(&linkID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find existing saved link: %w", err)
	}
	return &linkID, nil
}

// insertInboxSavedLink materializes a confirmed Inbox capture as a Library
// link. content and content_document are two projections of the same capture,
// not two copies of one string: body is the plain text, body_document is the
// Markdown converted from the captured HTML at capture time. A row with no
// document is plain and must say so — writing the flattened text into
// content_document under content_format='markdown' is what made confirmed
// captures render as one undifferentiated wall of text.
func insertInboxSavedLink(ctx context.Context, db database.Querier, item model.ReaderInbox, identityURL string) (*uuid.UUID, bool, error) {
	var linkID uuid.UUID
	err := db.QueryRow(ctx, `
		INSERT INTO links (
			url,source_kind,source_key,input_title,input_text,title,summary,tags,status,
			content,content_document,content_format,content_source,content_revision,
			library_kind,library_kind_locked,first_collected_at,created_at,updated_at)
			VALUES ($1,$2,$3,$5,$4,$5,$6,COALESCE($7::text[],'{}'::text[]),'done',$4,
				NULLIF($8::text,''),
				CASE WHEN NULLIF($8::text,'') IS NULL THEN 'plain' ELSE COALESCE(NULLIF($9::text,''),'plain') END,
				'user',1,'reading',true,NOW(),NOW(),NOW())
			ON CONFLICT (source_key) DO NOTHING
			RETURNING id`, item.URL, item.SourceKind, identityURL, item.Body, item.Title, item.Summary, item.Tags, item.BodyDocument, string(item.BodyFormat)).Scan(&linkID)
	if errors.Is(err, pgx.ErrNoRows) {
		matched, findErr := findCanonicalLink(ctx, db, identityURL)
		if findErr != nil {
			return nil, false, findErr
		}
		if matched == nil {
			return nil, false, fmt.Errorf("confirm inbox conflict did not resolve canonical link")
		}
		return matched, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("confirm inbox create link: %w", err)
	}
	return &linkID, true, nil
}

func (r *PGXReaderVNextRepository) BulkConfirmInbox(ctx context.Context, confirmations []model.ReaderInboxBulkConfirmation) ([]model.ReaderInboxBulkResult, error) {
	ids := make([]uuid.UUID, 0, len(confirmations))
	expectedRevisions := make(map[uuid.UUID]*int64, len(confirmations))
	for _, confirmation := range confirmations {
		ids = append(ids, confirmation.ID)
		if previous, exists := expectedRevisions[confirmation.ID]; exists {
			if previous == nil && confirmation.ExpectedRevision == nil {
				continue
			}
			if previous == nil || confirmation.ExpectedRevision == nil || *previous != *confirmation.ExpectedRevision {
				return nil, ErrRevisionConflict
			}
			continue
		}
		expectedRevisions[confirmation.ID] = confirmation.ExpectedRevision
	}
	return r.bulkConfirmInbox(ctx, ids, expectedRevisions)
}

const readerInboxAIProposalBatchSize = 100

// ConfirmAIProposals selects and confirms one stable server-owned batch. The
// selection is deliberately part of the transaction that confirms it: clients
// never race a paginated snapshot or reproduce eligibility locally.
func (r *PGXReaderVNextRepository) ConfirmAIProposals(ctx context.Context, partition model.ReaderInboxPartition) (model.ReaderInboxAIProposalConfirmation, error) {
	if !partition.Valid() {
		return model.ReaderInboxAIProposalConfirmation{}, ErrReaderInboxStateConflict
	}

	result := model.ReaderInboxAIProposalConfirmation{Items: make([]model.ReaderInboxBulkResult, 0, readerInboxAIProposalBatchSize)}
	err := r.withTx(ctx, func(db database.Querier) error {
		partitionClause := `(inbox.expires_at IS NULL OR inbox.expires_at > NOW())`
		if partition == model.ReaderInboxPartitionExpired {
			partitionClause = `inbox.expires_at IS NOT NULL AND inbox.expires_at <= NOW()`
		}
		rows, err := db.Query(ctx, `
			SELECT `+readerInboxColumnsQualified+`
			FROM reader_inbox inbox
			WHERE inbox.status='pending'
				AND inbox.deleted_at IS NULL
				AND `+partitionClause+`
				AND btrim(COALESCE(inbox.title,'')) <> ''
				AND inbox.proposal_status='completed'
			ORDER BY inbox.created_at ASC,inbox.id ASC
			LIMIT $1
			FOR UPDATE OF inbox`, readerInboxAIProposalBatchSize)
		if err != nil {
			return fmt.Errorf("select AI-ready inbox proposals: %w", err)
		}
		ids := make([]uuid.UUID, 0, readerInboxAIProposalBatchSize)
		for rows.Next() {
			item, scanErr := scanReaderInbox(rows)
			if scanErr != nil {
				rows.Close()
				return fmt.Errorf("scan AI-ready inbox proposal: %w", scanErr)
			}
			ids = append(ids, item.ID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read AI-ready inbox proposals: %w", err)
		}
		rows.Close()

		for _, id := range ids {
			confirmed, confirmErr := r.confirmInboxOn(ctx, db, id, nil)
			if confirmErr != nil {
				return confirmErr
			}
			result.Items = append(result.Items, confirmed)
		}

		var remaining int
		if err := db.QueryRow(ctx, `
			SELECT count(*)::int
			FROM reader_inbox inbox
			WHERE inbox.status='pending'
				AND inbox.deleted_at IS NULL
				AND `+partitionClause+`
				AND btrim(COALESCE(inbox.title,'')) <> ''
				AND inbox.proposal_status='completed'`).Scan(&remaining); err != nil {
			return fmt.Errorf("count remaining AI-ready inbox proposals: %w", err)
		}
		result.RemainingCount = remaining
		return nil
	})
	if err != nil {
		return model.ReaderInboxAIProposalConfirmation{}, err
	}
	return result, nil
}

func prepareInboxBatch(ids []uuid.UUID) ([]uuid.UUID, []uuid.UUID, error) {
	if len(ids) == 0 || len(ids) > 100 {
		return nil, nil, ErrReaderInboxStateConflict
	}
	ordered := append([]uuid.UUID(nil), ids...)
	seen := make(map[uuid.UUID]struct{}, len(ordered))
	unique := ordered[:0]
	for _, id := range ordered {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	lockOrder := append([]uuid.UUID(nil), unique...)
	sort.Slice(lockOrder, func(i, j int) bool { return lockOrder[i].String() < lockOrder[j].String() })
	return unique, lockOrder, nil
}

func (r *PGXReaderVNextRepository) bulkConfirmInbox(ctx context.Context, ids []uuid.UUID, expectedRevisions map[uuid.UUID]*int64) ([]model.ReaderInboxBulkResult, error) {
	unique, lockOrder, err := prepareInboxBatch(ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]model.ReaderInboxBulkResult, len(unique))
	err = r.withTx(ctx, func(db database.Querier) error {
		for _, id := range lockOrder {
			result, err := r.confirmInboxOn(ctx, db, id, expectedRevisions[id])
			if err != nil {
				return err
			}
			byID[id] = result
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	results := make([]model.ReaderInboxBulkResult, 0, len(unique))
	for _, id := range unique {
		results = append(results, byID[id])
	}
	return results, nil
}

func (r *PGXReaderVNextRepository) BulkDiscardInbox(ctx context.Context, ids []uuid.UUID) ([]model.ReaderInboxBulkResult, error) {
	unique, lockOrder, err := prepareInboxBatch(ids)
	if err != nil {
		return nil, err
	}
	err = r.withTx(ctx, func(db database.Querier) error {
		for _, id := range lockOrder {
			if err := r.discardInboxOn(ctx, db, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	results := make([]model.ReaderInboxBulkResult, 0, len(unique))
	for _, id := range unique {
		results = append(results, model.ReaderInboxBulkResult{ID: id, Status: "discarded"})
	}
	return results, nil
}

// StartInboxProposalTx marks the exact draft revision as queued. The caller
// inserts the River row in the same transaction, so there is no product/queue
// commit gap and no orphan state to reconcile later.
func (r *PGXReaderVNextRepository) StartInboxProposalTx(ctx context.Context, tx pgx.Tx, inboxID uuid.UUID, expectedRevision int64) (*model.ReaderInbox, error) {
	item, err := scanReaderInbox(tx.QueryRow(ctx, `
		UPDATE reader_inbox
		SET proposal_status=CASE WHEN proposal_status='running' THEN 'running' ELSE 'pending' END,
			updated_at=NOW()
		WHERE id=$1 AND status='pending' AND deleted_at IS NULL AND metadata_revision=$2
		RETURNING `+readerInboxColumns, inboxID, expectedRevision))
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if lookupErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reader_inbox WHERE id=$1 AND deleted_at IS NULL)`, inboxID).Scan(&exists); lookupErr != nil {
			return nil, fmt.Errorf("start inbox proposal: classify miss: %w", lookupErr)
		}
		if !exists {
			return nil, ErrNotFound
		}
		return nil, ErrRevisionConflict
	}
	if err != nil {
		return nil, fmt.Errorf("start inbox proposal: %w", err)
	}
	return item, nil
}

func (r *PGXReaderVNextRepository) ClaimInboxProposal(ctx context.Context, inboxID uuid.UUID, expectedRevision int64) (*model.ReaderInbox, error) {
	item, err := scanReaderInbox(r.db.QueryRow(ctx, `
		UPDATE reader_inbox
		SET proposal_status='running',updated_at=NOW()
		WHERE id=$1 AND metadata_revision=$2 AND status='pending' AND deleted_at IS NULL
			AND proposal_status IN ('pending','running')
		RETURNING `+readerInboxColumns, inboxID, expectedRevision))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrReaderInboxProposalNotRunnable
	}
	if err != nil {
		return nil, fmt.Errorf("claim inbox proposal: %w", err)
	}
	return item, nil
}

func (r *PGXReaderVNextRepository) RetryInboxProposal(ctx context.Context, inboxID uuid.UUID, expectedRevision int64) error {
	return r.updateInboxProposalStatus(ctx, inboxID, expectedRevision, "running", "pending")
}

func (r *PGXReaderVNextRepository) FailInboxProposal(ctx context.Context, inboxID uuid.UUID, expectedRevision int64) error {
	return r.updateInboxProposalStatus(ctx, inboxID, expectedRevision, "running", "failed")
}

func (r *PGXReaderVNextRepository) updateInboxProposalStatus(ctx context.Context, inboxID uuid.UUID, expectedRevision int64, from, to string) error {
	result, err := r.db.Exec(ctx, `
		UPDATE reader_inbox
		SET proposal_status=$4,updated_at=NOW()
		WHERE id=$1 AND metadata_revision=$2 AND status='pending' AND deleted_at IS NULL
			AND proposal_status=$3`, inboxID, expectedRevision, from, to)
	if err != nil {
		return fmt.Errorf("update inbox proposal status: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrReaderInboxProposalNotRunnable
	}
	return nil
}

func (r *PGXReaderVNextRepository) CompleteInboxProposal(ctx context.Context, inboxID uuid.UUID, expectedRevision int64, summary string, suggestedTags []string) error {
	result, err := r.db.Exec(ctx, `
		UPDATE reader_inbox
		SET summary=$3,suggested_tags=COALESCE($4::text[],'{}'::text[]),
			proposal_status='completed',updated_at=NOW()
		WHERE id=$1 AND metadata_revision=$2 AND status='pending' AND deleted_at IS NULL
			AND proposal_status='running'`,
		inboxID, expectedRevision, strings.TrimSpace(summary), suggestedTags)
	if err != nil {
		return fmt.Errorf("complete inbox proposal: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrReaderInboxProposalNotRunnable
	}
	return nil
}
