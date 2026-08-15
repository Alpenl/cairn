package repository

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/alloc"
	"webtag/internal/database"
	"webtag/internal/model"
)

const (
	readerTrashDefaultLimit = 50
	readerTrashMaxLimit     = 200
)

// ReaderHostLifecycleStore is deliberately narrower than ReaderVNextStore.
// Lifecycle routes can evolve without forcing unrelated Reader test doubles
// to implement host deletion semantics.
type ReaderHostLifecycleStore interface {
	SoftDeleteHost(context.Context, model.ReaderHostKind, uuid.UUID) (model.ReaderHostLifecycleResult, error)
	RestoreHost(context.Context, model.ReaderHostKind, uuid.UUID) (model.ReaderHostLifecycleResult, error)
	PurgeHost(context.Context, model.ReaderHostKind, uuid.UUID, uuid.UUID) error
	ListTrash(context.Context, *model.ReaderHostKind, string, int) ([]model.ReaderTrashItem, int, string, error)
}

var _ ReaderHostLifecycleStore = (*PGXReaderVNextRepository)(nil)

type readerLockedHost struct {
	deletedAt *time.Time
	body      string
	revision  int64
}

func (r *PGXReaderVNextRepository) SoftDeleteHost(ctx context.Context, kind model.ReaderHostKind, id uuid.UUID) (model.ReaderHostLifecycleResult, error) {
	result := model.ReaderHostLifecycleResult{HostKind: kind, HostID: id, State: model.ReaderHostTrashed}
	if !kind.Valid() {
		return result, ErrInvalidReaderHostKind
	}
	err := r.withTx(ctx, func(db database.Querier) error {
		host, err := lockReaderHost(ctx, db, kind, id)
		if err != nil {
			return err
		}
		if host.deletedAt != nil {
			return nil
		}
		if err := updateReaderHostDeletedAt(ctx, db, kind, id, true); err != nil {
			return err
		}
		// Link owns a database trigger because its existing durable deletion
		// command writes through PGXLinkRepository. Note and Inbox are entirely
		// owned by this transaction, so their tombstones are written explicitly.
		if kind != model.ReaderHostLink {
			if err := r.markThoughtHostTombstonesOn(ctx, db, string(kind), id.String(), readerHostTombstoneReason(kind)); err != nil {
				return err
			}
		}
		result.Changed = true
		return nil
	})
	return result, err
}

func (r *PGXReaderVNextRepository) RestoreHost(ctx context.Context, kind model.ReaderHostKind, id uuid.UUID) (model.ReaderHostLifecycleResult, error) {
	result := model.ReaderHostLifecycleResult{HostKind: kind, HostID: id, State: model.ReaderHostLive}
	if !kind.Valid() {
		return result, ErrInvalidReaderHostKind
	}
	err := r.withTx(ctx, func(db database.Querier) error {
		if kind == model.ReaderHostLink {
			if err := prelockLibraryFeedRevisions(ctx, db); err != nil {
				return err
			}
			restored, err := r.restoreLinkLifecycleOn(ctx, db, id)
			if err != nil {
				return err
			}
			if _, err := db.Exec(ctx, adoptSubmittedLinkSQL, id); err != nil {
				return fmt.Errorf("adopt explicitly restored link: %w", err)
			}
			result.Changed = restored.changed
			return nil
		}
		host, err := lockReaderHost(ctx, db, kind, id)
		if err != nil {
			return err
		}
		if host.deletedAt == nil {
			return nil
		}
		if err := updateReaderHostDeletedAt(ctx, db, kind, id, false); err != nil {
			return err
		}
		if err := r.restoreReaderHostThoughts(ctx, db, kind, id, host.body, host.revision); err != nil {
			return err
		}
		result.Changed = true
		return nil
	})
	return result, err
}

func lockReaderHost(ctx context.Context, db database.Querier, kind model.ReaderHostKind, id uuid.UUID) (readerLockedHost, error) {
	var (
		deletedAt pgtype.Timestamptz
		host      readerLockedHost
		err       error
	)
	switch kind {
	case model.ReaderHostLink:
		err = db.QueryRow(ctx, `SELECT deleted_at,COALESCE(content_document,content,''),content_revision FROM links WHERE id=$1 FOR UPDATE`, id).Scan(&deletedAt, &host.body, &host.revision)
	case model.ReaderHostInbox:
		err = db.QueryRow(ctx, `SELECT deleted_at,body,metadata_revision FROM reader_inbox WHERE id=$1 FOR UPDATE`, id).Scan(&deletedAt, &host.body, &host.revision)
	case model.ReaderHostNote:
		err = db.QueryRow(ctx, `SELECT deleted_at,published_content,published_revision FROM reader_notes WHERE id=$1 FOR UPDATE`, id).Scan(&deletedAt, &host.body, &host.revision)
	default:
		return readerLockedHost{}, ErrInvalidReaderHostKind
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return readerLockedHost{}, ErrNotFound
	}
	if err != nil {
		return readerLockedHost{}, fmt.Errorf("lock reader %s host: %w", kind, err)
	}
	if deletedAt.Valid {
		value := deletedAt.Time
		host.deletedAt = &value
	}
	return host, nil
}

func updateReaderHostDeletedAt(ctx context.Context, db database.Querier, kind model.ReaderHostKind, id uuid.UUID, deleted bool) error {
	table := ""
	switch kind {
	case model.ReaderHostLink:
		table = "links"
	case model.ReaderHostInbox:
		table = "reader_inbox"
	case model.ReaderHostNote:
		table = "reader_notes"
	default:
		return ErrInvalidReaderHostKind
	}
	value := "NULL"
	if deleted {
		value = "NOW()"
	}
	tag, err := db.Exec(ctx, `UPDATE `+table+` SET deleted_at=`+value+`,updated_at=NOW() WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("update reader %s lifecycle: %w", kind, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func readerHostTombstoneReason(kind model.ReaderHostKind) string {
	switch kind {
	case model.ReaderHostInbox:
		return "inbox_discarded"
	default:
		return string(kind) + "_deleted"
	}
}

type readerLifecycleThought struct {
	item         model.ReaderThought
	tombstonedAt time.Time
}

func (r *PGXReaderVNextRepository) restoreReaderHostThoughts(ctx context.Context, db database.Querier, kind model.ReaderHostKind, id uuid.UUID, body string, revision int64) error {
	rows, err := db.Query(ctx, `
		SELECT `+readerThoughtColumns+`,tt.created_at
		FROM reader_thought_tombstones tt
		JOIN reader_thoughts
		  ON reader_thoughts.id=tt.thought_id
		WHERE tt.host_kind=$1
		  AND tt.host_id=$2
		  AND tt.reason=$3
		  AND reader_thoughts.deleted=false
		ORDER BY reader_thoughts.id
		FOR UPDATE OF reader_thoughts`, kind, id.String(), readerHostTombstoneReason(kind))
	if err != nil {
		return fmt.Errorf("list reader %s restore thoughts: %w", kind, err)
	}
	thoughts := make([]readerLifecycleThought, 0)
	for rows.Next() {
		item, scanErr := scanReaderLifecycleThought(rows)
		if scanErr != nil {
			rows.Close()
			return fmt.Errorf("scan reader %s restore thought: %w", kind, scanErr)
		}
		thoughts = append(thoughts, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate reader %s restore thoughts: %w", kind, err)
	}
	rows.Close()
	for i := range thoughts {
		if err := r.restoreReaderHostThought(ctx, db, kind, id, body, revision, thoughts[i]); err != nil {
			return err
		}
	}
	return nil
}

func scanReaderLifecycleThought(row readerScanner) (readerLifecycleThought, error) {
	var (
		out           readerLifecycleThought
		target, quote []byte
	)
	if err := row.Scan(
		&out.item.ID,
		&out.item.HostKind,
		&out.item.HostID,
		&out.item.LinkID,
		&target,
		&quote,
		&out.item.Body,
		&out.item.Source,
		&out.item.Deleted,
		&out.item.LastSequence,
		&out.item.WinnerKey.LogicalClock,
		&out.item.WinnerKey.DeviceID,
		&out.item.WinnerKey.OpID,
		&out.item.CreatedAt,
		&out.item.UpdatedAt,
		&out.tombstonedAt,
	); err != nil {
		return readerLifecycleThought{}, err
	}
	out.item.Target = append(out.item.Target[:0], target...)
	if len(quote) > 0 {
		out.item.Quote = append(out.item.Quote[:0], quote...)
	}
	return out, nil
}

func (r *PGXReaderVNextRepository) restoreReaderHostThought(ctx context.Context, db database.Querier, kind model.ReaderHostKind, id uuid.UUID, body string, revision int64, lifecycle readerLifecycleThought) error {
	command := model.ReaderThoughtReattachCommand{TargetHostKind: string(kind), TargetHostID: id.String()}
	target, payload, err := readerReattachThoughtPayload(&lifecycle.item, command, revision, body)
	if err != nil {
		if errors.Is(err, ErrInvalidReaderReanchor) || errors.Is(err, ErrInvalidReaderThought) {
			return nil
		}
		return fmt.Errorf("reanchor reader %s restore thought: %w", kind, err)
	}
	op := model.ReaderThoughtOp{
		OpID:          "host-restore-" + string(kind) + "-" + id.String() + "-" + strconv.FormatInt(revision, 10) + "-" + strconv.FormatInt(lifecycle.tombstonedAt.UnixNano(), 10) + "-" + lifecycle.item.ID,
		DeviceID:      "reader-lifecycle",
		OperationKind: "update",
		AnnotationID:  lifecycle.item.ID,
		HostKind:      string(kind),
		HostID:        id.String(),
		Target:        target,
		Payload:       payload,
	}
	thoughtOp, sequence, duplicate, err := r.appendDerivedThoughtOp(ctx, db, op)
	if err != nil {
		return fmt.Errorf("append reader %s restore thought: %w", kind, err)
	}
	if !duplicate {
		if err := r.materializeThought(ctx, db, thoughtOp, sequence); err != nil {
			return err
		}
	}
	if _, err := db.Exec(ctx, `DELETE FROM reader_thought_tombstones WHERE thought_id=$1`, lifecycle.item.ID); err != nil {
		return fmt.Errorf("clear reader %s restore tombstone: %w", kind, err)
	}
	return nil
}

func (r *PGXReaderVNextRepository) PurgeHost(ctx context.Context, kind model.ReaderHostKind, id, operationID uuid.UUID) error {
	if !kind.Valid() {
		return ErrInvalidReaderHostKind
	}
	if operationID == uuid.Nil {
		return ErrInvalidReaderReanchor
	}
	return r.withTx(ctx, func(db database.Querier) error {
		matched, exists, err := readerPurgeReceipt(ctx, db, kind, id, operationID)
		if err != nil {
			return err
		}
		if exists {
			if matched {
				return nil
			}
			return ErrNotFound
		}
		host, err := lockReaderHost(ctx, db, kind, id)
		if errors.Is(err, ErrNotFound) {
			matched, _, receiptErr := readerPurgeReceipt(ctx, db, kind, id, operationID)
			if receiptErr != nil {
				return receiptErr
			}
			if matched {
				return nil
			}
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if host.deletedAt == nil {
			return ErrReaderHostNotTrashed
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO reader_host_purge_receipts (host_kind,host_id,operation_id,outcome)
			VALUES ($1,$2,$3,'purged')`, kind, id, operationID); err != nil {
			return fmt.Errorf("record reader %s purge receipt: %w", kind, err)
		}
		if err := deleteReaderHostDerivedRows(ctx, db, kind, id); err != nil {
			return err
		}
		table := map[model.ReaderHostKind]string{
			model.ReaderHostLink: "links", model.ReaderHostInbox: "reader_inbox", model.ReaderHostNote: "reader_notes",
		}[kind]
		tag, err := db.Exec(ctx, `DELETE FROM `+table+` WHERE id=$1 AND deleted_at IS NOT NULL`, id)
		if err != nil {
			return fmt.Errorf("purge reader %s host: %w", kind, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func readerPurgeReceipt(ctx context.Context, db database.Querier, kind model.ReaderHostKind, id, operationID uuid.UUID) (matched, exists bool, err error) {
	var stored uuid.UUID
	err = db.QueryRow(ctx, `SELECT operation_id FROM reader_host_purge_receipts WHERE host_kind=$1 AND host_id=$2`, kind, id).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("read reader %s purge receipt: %w", kind, err)
	}
	return stored == operationID, true, nil
}

func deleteReaderHostDerivedRows(ctx context.Context, db database.Querier, kind model.ReaderHostKind, id uuid.UUID) error {
	hostID := id.String()
	for _, statement := range []string{
		`DELETE FROM reader_categorizables WHERE host_kind=$1 AND host_id=$2`,
		`DELETE FROM reader_todos WHERE origin_host_kind=$1 AND origin_host_id=$2`,
	} {
		if _, err := db.Exec(ctx, statement, kind, hostID); err != nil {
			return fmt.Errorf("delete reader %s purge projection: %w", kind, err)
		}
	}
	if kind == model.ReaderHostNote {
		if _, err := db.Exec(ctx, `DELETE FROM reader_note_history WHERE note_id=$1`, id); err != nil {
			return fmt.Errorf("delete reader note purge history: %w", err)
		}
	}
	return nil
}

func readerTrashCursor(at time.Time, kind model.ReaderHostKind, id uuid.UUID) string {
	value := at.UTC().Format(time.RFC3339Nano) + "|" + string(kind) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func parseReaderTrashCursor(raw string) (time.Time, model.ReaderHostKind, uuid.UUID, error) {
	if raw == "" {
		return time.Time{}, "", uuid.Nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", uuid.Nil, fmt.Errorf("%w: invalid trash cursor", ErrInvalidReaderCursor)
	}
	parts := strings.Split(string(decoded), "|")
	if len(parts) != 3 {
		return time.Time{}, "", uuid.Nil, fmt.Errorf("%w: invalid trash cursor", ErrInvalidReaderCursor)
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", uuid.Nil, fmt.Errorf("%w: invalid trash cursor", ErrInvalidReaderCursor)
	}
	kind := model.ReaderHostKind(parts[1])
	id, err := uuid.Parse(parts[2])
	if !kind.Valid() || err != nil {
		return time.Time{}, "", uuid.Nil, fmt.Errorf("%w: invalid trash cursor", ErrInvalidReaderCursor)
	}
	return at, kind, id, nil
}

func (r *PGXReaderVNextRepository) ListTrash(ctx context.Context, kind *model.ReaderHostKind, after string, limit int) ([]model.ReaderTrashItem, int, string, error) {
	if kind != nil && !kind.Valid() {
		return nil, 0, "", ErrInvalidReaderHostKind
	}
	limit = normalizeReaderTrashLimit(limit)
	cursorAt, cursorKind, cursorID, err := parseReaderTrashCursor(after)
	if err != nil {
		return nil, 0, "", err
	}
	base := readerTrashBaseQuery(kind)
	args := []any{}
	var count int
	if err := r.db.QueryRow(ctx, `SELECT count(*)`+base, args...).Scan(&count); err != nil {
		return nil, 0, "", fmt.Errorf("count reader trash: %w", err)
	}
	query, args := readerTrashPageQuery(base, args, cursorAt, cursorKind, cursorID, limit)
	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, "", fmt.Errorf("list reader trash: %w", err)
	}
	items, next, err := scanReaderTrashRows(rows, limit)
	if err != nil {
		return nil, 0, "", err
	}
	return items, count, next, nil
}

func normalizeReaderTrashLimit(limit int) int {
	if limit <= 0 || limit > readerTrashMaxLimit {
		return readerTrashDefaultLimit
	}
	return limit
}

func readerTrashBaseQuery(kind *model.ReaderHostKind) string {
	queries := make([]string, 0, 3)
	add := func(hostKind model.ReaderHostKind, query string) {
		if kind == nil || *kind == hostKind {
			queries = append(queries, query)
		}
	}
	add(model.ReaderHostLink, `SELECT 'link'::text,id,title,url,deleted_at FROM links WHERE deleted_at IS NOT NULL`)
	add(model.ReaderHostInbox, `SELECT 'inbox'::text,id,title,url,deleted_at FROM reader_inbox WHERE deleted_at IS NOT NULL`)
	add(model.ReaderHostNote, `SELECT 'note'::text,id,title,NULL::text,deleted_at FROM reader_notes WHERE deleted_at IS NOT NULL`)
	var base strings.Builder
	base.WriteString(` FROM (`)
	for index, part := range queries {
		if index > 0 {
			base.WriteString(` UNION ALL `)
		}
		base.WriteString(part)
	}
	base.WriteString(`) trash(host_kind,host_id,title,url,trashed_at)`)
	return base.String()
}

func readerTrashPageQuery(base string, args []any, cursorAt time.Time, cursorKind model.ReaderHostKind, cursorID uuid.UUID, limit int) (string, []any) {
	query := `SELECT host_kind,host_id,title,url,trashed_at` + base
	if !cursorAt.IsZero() {
		first := len(args) + 1
		query += fmt.Sprintf(
			` WHERE (trashed_at < $%d OR (trashed_at = $%d AND (host_kind > $%d OR (host_kind = $%d AND host_id < $%d))))`,
			first, first, first+1, first+1, first+2,
		)
		args = append(args, cursorAt, string(cursorKind), cursorID)
	}
	query += fmt.Sprintf(` ORDER BY trashed_at DESC,host_kind,host_id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	return query, args
}

func scanReaderTrashRows(rows pgx.Rows, limit int) ([]model.ReaderTrashItem, string, error) {
	defer rows.Close()
	items := make([]model.ReaderTrashItem, 0, alloc.Hint(normalizeReaderTrashLimit(limit)))
	for rows.Next() {
		var (
			item       model.ReaderTrashItem
			title, url pgtype.Text
		)
		if err := rows.Scan(&item.HostKind, &item.HostID, &title, &url, &item.TrashedAt); err != nil {
			return nil, "", fmt.Errorf("scan reader trash: %w", err)
		}
		if title.Valid {
			value := title.String
			item.Title = &value
		}
		if url.Valid {
			value := url.String
			item.URL = &value
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate reader trash: %w", err)
	}
	if len(items) == limit {
		last := items[len(items)-1]
		return items, readerTrashCursor(last.TrashedAt, last.HostKind, last.HostID), nil
	}
	return items, "", nil
}
