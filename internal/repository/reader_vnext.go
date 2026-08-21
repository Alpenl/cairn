package repository

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/database"
	"webtag/internal/model"
	"webtag/internal/notetitle"
	"webtag/internal/readertext"
	"webtag/internal/urlidentity"
)

// ReaderVNextStore is the persistence boundary for the personal Reader
// surfaces. It is intentionally separate from the link repository so Reader-owned data
// does not make the existing link repository a second god interface.
type ReaderVNextStore interface {
	AppendThoughtOps(context.Context, []model.ReaderThoughtOp) ([]model.ReaderThoughtAck, error)
	ListThoughts(context.Context, string, string, int) ([]model.ReaderThought, string, error)
	SearchThoughts(context.Context, string, string, int) ([]model.ReaderThoughtSearch, int, string, error)
	ListThoughtsSince(context.Context, string, int) ([]model.ReaderThought, string, error)
	ListThoughtHistory(context.Context, string, int) ([]model.ReaderThought, string, error)
	ListThoughtConflicts(context.Context, string, int) ([]model.ReaderThoughtConflict, string, error)
	GetThought(context.Context, string) (*model.ReaderThought, error)
	GetAIContext(context.Context, uuid.UUID) (*model.ReaderAIContext, error)
	MarkThoughtHostTombstones(context.Context, string, string, string) error
	CreateNote(context.Context, model.ReaderNote) (*model.ReaderNote, error)
	ListNotes(context.Context, string, int) ([]model.ReaderNote, int, string, error)
	SearchPublishedNotes(context.Context, string, int) ([]model.ReaderNoteSearch, int, error)
	GetNote(context.Context, uuid.UUID) (*model.ReaderNote, error)
	SaveNoteDraft(context.Context, model.ReaderNoteDraftCommand) (*model.ReaderNote, error)
	DiscardNoteDraft(context.Context, model.ReaderNoteDiscardDraftCommand) error
	PublishNote(context.Context, model.ReaderNotePublishCommand) (*model.ReaderNote, error)
	ListNoteHistory(context.Context, uuid.UUID, int) ([]model.ReaderNoteHistory, error)
	RestoreNoteRevision(context.Context, model.ReaderNoteRestoreCommand) (*model.ReaderNote, error)
	CreateInbox(context.Context, model.ReaderInbox) (*model.ReaderInbox, error)
	ListInbox(context.Context, model.ReaderInboxPartition, string, int) ([]model.ReaderInboxListItem, int, int, string, error)
	GetInbox(context.Context, uuid.UUID) (*model.ReaderInbox, error)
	PatchInbox(context.Context, model.ReaderInboxPatch) (*model.ReaderInbox, error)
	DiscardInbox(context.Context, uuid.UUID) error
	RestoreInbox(context.Context, uuid.UUID) error
	ConfirmInbox(context.Context, uuid.UUID, *int64) (uuid.UUID, error)
	BulkConfirmInbox(context.Context, []model.ReaderInboxBulkConfirmation) ([]model.ReaderInboxBulkResult, error)
	ConfirmAIProposals(context.Context, model.ReaderInboxPartition) (model.ReaderInboxAIProposalConfirmation, error)
	BulkDiscardInbox(context.Context, []uuid.UUID) ([]model.ReaderInboxBulkResult, error)
	ClaimInboxProposal(context.Context, uuid.UUID, int64) (*model.ReaderInbox, error)
	RetryInboxProposal(context.Context, uuid.UUID, int64) error
	FailInboxProposal(context.Context, uuid.UUID, int64) error
	CompleteInboxProposal(context.Context, uuid.UUID, int64, string, []string) error
	CreateTodo(context.Context, model.ReaderTodo) (*model.ReaderTodo, error)
	ListTodos(context.Context, string, int) (model.ReaderTodoPage, error)
	PatchTodo(context.Context, model.ReaderTodoPatch) (*model.ReaderTodo, error)
	DeleteTodo(context.Context, uuid.UUID) error
	GetEngagement(context.Context, uuid.UUID) (*model.ReaderEngagement, error)
	PatchEngagement(context.Context, model.ReaderEngagementPatch) (*model.ReaderEngagement, error)
	LoadHomeAggregate(context.Context) (ReaderHomeAggregate, error)
	HomeCounts(context.Context) (map[string]int, error)
	ListFeedWithSources(context.Context, string, string, []string, int) (*model.ReaderFeedPage, error)
	FeedbackFeed(context.Context, string, string) (model.ReaderFeedFeedback, error)
	RelatedTags(context.Context, *uuid.UUID, int) ([]string, error)
	ListActivity(context.Context, model.ReaderActivityQuery) (model.ReaderActivityPage, error)
	UpdateLinkMetadata(context.Context, model.ReaderLinkMetadataPatch) (model.ReaderLinkMetadataUpdate, error)
	SoftDeleteHost(context.Context, model.ReaderHostKind, uuid.UUID) (model.ReaderHostLifecycleResult, error)
	RestoreHost(context.Context, model.ReaderHostKind, uuid.UUID) (model.ReaderHostLifecycleResult, error)
	PurgeHost(context.Context, model.ReaderHostKind, uuid.UUID, uuid.UUID) error
	ListTrash(context.Context, *model.ReaderHostKind, string, int) ([]model.ReaderTrashItem, int, string, error)
}

type PGXReaderVNextRepository struct {
	db                 database.Querier
	linkLifecycleQueue ReaderLinkLifecycleQueue
}

func NewPGXReaderVNextRepository(db database.Querier) *PGXReaderVNextRepository {
	return &PGXReaderVNextRepository{db: db}
}

func NewPGXReaderVNextRepositoryWithLinkLifecycle(
	db database.Querier,
	queue ReaderLinkLifecycleQueue,
) *PGXReaderVNextRepository {
	if queue == nil {
		panic("repository: nil Reader Link lifecycle queue")
	}
	return &PGXReaderVNextRepository{db: db, linkLifecycleQueue: queue}
}

// ReaderLinkLifecycleQueue is the transaction-bound River port required when
// Reader entry points trash or restore a Link. It stays narrow so the
// repository does not depend on the worker package.
type ReaderLinkLifecycleQueue interface {
	EnqueueTx(context.Context, pgx.Tx, model.ParseAttempt) error
	CancelAllActiveTx(context.Context, pgx.Tx, uuid.UUID) error
}

var _ ReaderVNextStore = (*PGXReaderVNextRepository)(nil)

type readerTxBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

func (r *PGXReaderVNextRepository) withTx(ctx context.Context, fn func(database.Querier) error) error {
	beginner, ok := r.db.(readerTxBeginner)
	if !ok {
		return fn(r.db)
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func rawJSON(value json.RawMessage) []byte {
	if len(value) == 0 {
		return []byte(`{}`)
	}
	return value
}

func parseReaderCursor(raw string) (time.Time, string, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: %w", ErrInvalidReaderCursor, err)
	}
	parts := strings.SplitN(string(decoded), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", ErrInvalidReaderCursor
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil || parts[1] == "" {
		return time.Time{}, "", ErrInvalidReaderCursor
	}
	return at, parts[1], nil
}

func readerCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

type readerTodoPageCursor struct {
	Done      bool       `json:"done"`
	DueAt     *time.Time `json:"due_at"`
	CreatedAt time.Time  `json:"created_at"`
	ID        uuid.UUID  `json:"id"`
}

func parseReaderTodoCursor(raw string) (readerTodoPageCursor, error) {
	if strings.TrimSpace(raw) == "" {
		return readerTodoPageCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return readerTodoPageCursor{}, fmt.Errorf("%w: invalid TODO cursor", ErrInvalidReaderCursor)
	}
	var cursor readerTodoPageCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.ID == uuid.Nil || cursor.CreatedAt.IsZero() {
		return readerTodoPageCursor{}, fmt.Errorf("%w: invalid TODO cursor", ErrInvalidReaderCursor)
	}
	return cursor, nil
}

func readerTodoCursor(item model.ReaderTodo) string {
	raw, _ := json.Marshal(readerTodoPageCursor{
		Done: item.Done, DueAt: item.DueAt, CreatedAt: item.CreatedAt, ID: item.ID,
	})
	return base64.RawURLEncoding.EncodeToString(raw)
}

type readerScanner interface {
	Scan(...any) error
}

func scanReaderThought(row readerScanner) (*model.ReaderThought, error) {
	return scanReaderThoughtWithLifecycle(row, false)
}

func scanReaderThoughtWithLifecycle(row readerScanner, lifecycle bool) (*model.ReaderThought, error) {
	var out model.ReaderThought
	var target, quote []byte
	if lifecycle {
		var reason pgtype.Text
		var tombstonedAt pgtype.Timestamptz
		if err := row.Scan(&out.ID, &out.HostKind, &out.HostID, &out.LinkID, &target, &quote, &out.Body, &out.Source, &out.Deleted, &out.LastSequence, &out.WinnerKey.LogicalClock, &out.WinnerKey.DeviceID, &out.WinnerKey.OpID, &out.CreatedAt, &out.UpdatedAt, &reason, &tombstonedAt); err != nil {
			return nil, err
		}
		out.LifecycleStatus = "active"
		if reason.Valid || tombstonedAt.Valid {
			out.LifecycleStatus = "tombstone"
			if reason.Valid {
				value := reason.String
				out.LifecycleReason = &value
			}
			if tombstonedAt.Valid {
				value := tombstonedAt.Time
				out.TombstonedAt = &value
			}
		}
	} else if err := row.Scan(&out.ID, &out.HostKind, &out.HostID, &out.LinkID, &target, &quote, &out.Body, &out.Source, &out.Deleted, &out.LastSequence, &out.WinnerKey.LogicalClock, &out.WinnerKey.DeviceID, &out.WinnerKey.OpID, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return nil, err
	} else {
		out.LifecycleStatus = "active"
	}
	out.Target = append(json.RawMessage(nil), target...)
	if len(quote) > 0 {
		out.Quote = append(json.RawMessage(nil), quote...)
	}
	return &out, nil
}

// applyReaderThoughtSnapshot replaces only user-visible content. Sequence and
// winner metadata intentionally remain from the materialized row so clients
// can continue to advance their sync cursor and perform CAS without ever
// treating a later replay as authority over archived content.
const readerThoughtSnapshotVersion = 1

func applyReaderThoughtSnapshot(item *model.ReaderThought, raw []byte) error {
	var snapshot struct {
		SnapshotVersion      int             `json:"snapshot_version"`
		ID                   string          `json:"id"`
		HostKind             string          `json:"host_kind"`
		HostID               string          `json:"host_id"`
		LinkID               *string         `json:"link_id"`
		Target               json.RawMessage `json:"target"`
		Quote                json.RawMessage `json:"quote"`
		Body                 string          `json:"body"`
		Source               string          `json:"source"`
		OriginalHostSnapshot json.RawMessage `json:"original_host_snapshot"`
	}
	if !json.Valid(raw) || json.Unmarshal(raw, &snapshot) != nil {
		return ErrInvalidReaderThought
	}
	if snapshot.SnapshotVersion != readerThoughtSnapshotVersion {
		return ErrInvalidReaderThought
	}
	if snapshot.ID != "" {
		item.ID = snapshot.ID
	}
	if snapshot.HostKind != "" {
		item.HostKind = snapshot.HostKind
	}
	if snapshot.HostID != "" {
		item.HostID = snapshot.HostID
	}
	if snapshot.LinkID != nil {
		linkID, err := uuid.Parse(*snapshot.LinkID)
		if err != nil {
			return ErrInvalidReaderThought
		}
		item.LinkID = &linkID
	} else {
		item.LinkID = nil
	}
	if len(snapshot.Target) > 0 {
		item.Target = append(item.Target[:0], snapshot.Target...)
	}
	if len(snapshot.Quote) > 0 {
		item.Quote = append(item.Quote[:0], snapshot.Quote...)
	} else {
		item.Quote = nil
	}
	item.Body = snapshot.Body
	item.Source = snapshot.Source
	if len(snapshot.OriginalHostSnapshot) > 0 {
		item.OriginalHostSnapshot = append(item.OriginalHostSnapshot[:0], snapshot.OriginalHostSnapshot...)
	}
	return nil
}

// applyReaderThoughtReplaySnapshot is deliberately stricter than the helper
// used by reattach flows. Sync is a recovery boundary: returning the mutable
// projection for a malformed tombstone would silently replace the historical
// record a client is supposed to preserve.
func applyReaderThoughtReplaySnapshot(item *model.ReaderThought, raw []byte) error {
	if err := validateReaderThoughtReplaySnapshot(item.ID, raw); err != nil {
		return err
	}
	return applyReaderThoughtSnapshot(item, raw)
}

// applyReaderThoughtUserDeletionSnapshot intentionally never copies snapshot
// fields into the replay item. A user deletion stores only an
// anti-resurrection marker, and sync must not disclose stale projection
// content if that marker is replayed after another operation advanced the log.
func applyReaderThoughtUserDeletionSnapshot(item *model.ReaderThought, raw []byte) error {
	var snapshot struct {
		ID       string `json:"id"`
		HostKind string `json:"host_kind"`
		HostID   string `json:"host_id"`
	}
	if !item.Deleted || !json.Valid(raw) || json.Unmarshal(raw, &snapshot) != nil ||
		snapshot.ID == "" || snapshot.ID != item.ID || snapshot.HostKind != item.HostKind || snapshot.HostID != item.HostID {
		return ErrInvalidReaderThought
	}
	item.LinkID = nil
	item.Target = json.RawMessage(`{}`)
	item.Quote = nil
	item.Body = ""
	item.Source = ""
	item.OriginalHostSnapshot = nil
	return nil
}

var readerThoughtReplaySnapshotFields = []string{
	"snapshot_version", "id", "host_kind", "host_id", "link_id", "type", "body", "target", "quote", "source",
	"created_at", "updated_at", "original_host_snapshot", "original_host_identity", "frozen_at",
}

func decodeReaderThoughtReplaySnapshot(raw []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if !json.Valid(raw) || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, ErrInvalidReaderThought
	}
	for _, name := range readerThoughtReplaySnapshotFields {
		if len(fields[name]) == 0 {
			return nil, ErrInvalidReaderThought
		}
	}
	var snapshotVersion int
	if json.Unmarshal(fields["snapshot_version"], &snapshotVersion) != nil || snapshotVersion != readerThoughtSnapshotVersion {
		return nil, ErrInvalidReaderThought
	}
	return fields, nil
}

func readerThoughtSnapshotString(fields map[string]json.RawMessage, name string) (string, bool) {
	rawValue := strings.TrimSpace(string(fields[name]))
	if len(rawValue) < 2 || rawValue[0] != '"' {
		return "", false
	}
	var value string
	if json.Unmarshal(fields[name], &value) != nil {
		return "", false
	}
	return value, true
}

func readerThoughtSnapshotObject(fields map[string]json.RawMessage, name string) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(fields[name], &value) == nil && value != nil
}

func validReaderThoughtSnapshotIdentity(fields map[string]json.RawMessage, thoughtID string) bool {
	id, ok := readerThoughtSnapshotString(fields, "id")
	if !ok || id == "" || id != thoughtID {
		return false
	}
	hostKind, hostKindOK := readerThoughtSnapshotString(fields, "host_kind")
	hostID, hostIDOK := readerThoughtSnapshotString(fields, "host_id")
	typeName, typeOK := readerThoughtSnapshotString(fields, "type")
	return hostKindOK && strings.TrimSpace(hostKind) != "" && hostIDOK && strings.TrimSpace(hostID) != "" && typeOK && typeName == "thought"
}

func validReaderThoughtSnapshotContent(fields map[string]json.RawMessage) bool {
	if _, ok := readerThoughtSnapshotString(fields, "body"); !ok {
		return false
	}
	if _, ok := readerThoughtSnapshotString(fields, "source"); !ok {
		return false
	}
	if !readerThoughtSnapshotObject(fields, "target") || !readerThoughtSnapshotObject(fields, "original_host_identity") {
		return false
	}
	quote := strings.TrimSpace(string(fields["quote"]))
	return (quote == "null" || readerThoughtSnapshotObject(fields, "quote")) && json.Valid(fields["original_host_snapshot"])
}

func validReaderThoughtSnapshotLink(fields map[string]json.RawMessage) bool {
	if strings.TrimSpace(string(fields["link_id"])) == "null" {
		return true
	}
	var value string
	if json.Unmarshal(fields["link_id"], &value) != nil {
		return false
	}
	_, err := uuid.Parse(value)
	return err == nil
}

func validReaderThoughtSnapshotTimes(fields map[string]json.RawMessage) bool {
	for _, name := range []string{"created_at", "updated_at", "frozen_at"} {
		var value time.Time
		if json.Unmarshal(fields[name], &value) != nil || value.IsZero() {
			return false
		}
	}
	return true
}

func validateReaderThoughtReplaySnapshot(thoughtID string, raw []byte) error {
	fields, err := decodeReaderThoughtReplaySnapshot(raw)
	if err != nil {
		return err
	}
	if !validReaderThoughtSnapshotIdentity(fields, thoughtID) || !validReaderThoughtSnapshotContent(fields) ||
		!validReaderThoughtSnapshotLink(fields) || !validReaderThoughtSnapshotTimes(fields) {
		return ErrInvalidReaderThought
	}
	return nil
}

func scanReaderThoughtSyncSnapshot(row readerScanner) (*model.ReaderThought, error) {
	var (
		item          model.ReaderThought
		target, quote []byte
		snapshot      []byte
		reason        pgtype.Text
		tombstonedAt  pgtype.Timestamptz
	)
	if err := row.Scan(
		&item.ID, &item.HostKind, &item.HostID, &item.LinkID, &target, &quote,
		&item.Body, &item.Source, &item.Deleted, &item.LastSequence,
		&item.WinnerKey.LogicalClock, &item.WinnerKey.DeviceID, &item.WinnerKey.OpID,
		&item.CreatedAt, &item.UpdatedAt, &snapshot, &reason, &tombstonedAt,
	); err != nil {
		return nil, err
	}
	item.Target = append(json.RawMessage(nil), target...)
	if len(quote) > 0 {
		item.Quote = append(json.RawMessage(nil), quote...)
	}
	if !reason.Valid && !tombstonedAt.Valid && len(snapshot) == 0 {
		item.LifecycleStatus = "active"
		return &item, nil
	}
	if !reason.Valid || strings.TrimSpace(reason.String) == "" || !tombstonedAt.Valid || len(snapshot) == 0 {
		return nil, ErrInvalidReaderThought
	}
	if reason.String == "user_deleted" {
		if err := applyReaderThoughtUserDeletionSnapshot(&item, snapshot); err != nil {
			return nil, err
		}
	} else if err := applyReaderThoughtReplaySnapshot(&item, snapshot); err != nil {
		return nil, err
	}
	item.LifecycleStatus = "tombstone"
	reasonValue := reason.String
	item.LifecycleReason = &reasonValue
	item.TombstonedAt = &tombstonedAt.Time
	return &item, nil
}

func scanReaderThoughtHistorySnapshot(row readerScanner) (*model.ReaderThought, error) {
	var (
		item     model.ReaderThought
		target   []byte
		quote    []byte
		snapshot []byte
		reason   string
		at       time.Time
	)
	if err := row.Scan(
		&item.ID, &item.HostKind, &item.HostID, &item.LinkID, &target, &quote,
		&item.Body, &item.Source, &item.Deleted, &item.LastSequence,
		&item.WinnerKey.LogicalClock, &item.WinnerKey.DeviceID, &item.WinnerKey.OpID,
		&item.CreatedAt, &item.UpdatedAt, &snapshot, &reason, &at,
	); err != nil {
		return nil, err
	}
	item.Target = append(item.Target, target...)
	item.Quote = append(item.Quote, quote...)
	if err := applyReaderThoughtReplaySnapshot(&item, snapshot); err != nil {
		return nil, fmt.Errorf("decode thought lifecycle snapshot: %w", err)
	}
	item.LifecycleStatus = "tombstone"
	item.LifecycleReason = &reason
	item.TombstonedAt = &at
	return &item, nil
}

func scanReaderNote(row readerScanner) (*model.ReaderNote, error) {
	var out model.ReaderNote
	if err := row.Scan(&out.ID, &out.Title, &out.PublishedContent, &out.PublishedRevision, &out.DraftContent, &out.DraftRevision, &out.DraftUpdatedAt, &out.DeletedAt, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return nil, err
	}
	return &out, nil
}

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

// readerInboxPreview turns the SQL-truncated card source into the bounded
// single-line preview the queue renders. The input is already clamped to
// readerInboxPreviewSourceLimit characters, so the rune conversion here is
// bounded regardless of how large the stored body is.
func readerInboxPreview(raw string) string {
	preview := []rune(strings.Join(strings.Fields(raw), " "))
	if len(preview) <= readerInboxPreviewLimit {
		return string(preview)
	}
	return string(preview[:readerInboxPreviewLimit]) + "…"
}

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

func scanReaderTodo(row readerScanner) (*model.ReaderTodo, error) {
	var out model.ReaderTodo
	var ref []byte
	if err := row.Scan(&out.ID, &out.Text, &out.DueAt, &out.Done, &out.OriginKind, &out.OriginHostKind, &out.OriginHostID, &ref, &out.HostRevision, &out.CompletedAt, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return nil, err
	}
	out.OriginRef = append(json.RawMessage(nil), ref...)
	out.Expired = out.DueAt != nil && out.DueAt.Before(time.Now()) && !out.Done
	return &out, nil
}

func scanReaderEngagement(row readerScanner) (*model.ReaderEngagement, error) {
	var out model.ReaderEngagement
	var lastOpened pgtype.Timestamptz
	if err := row.Scan(&out.LinkID, &out.Read, &out.Progress, &out.ReadLater, &lastOpened, &out.UpdatedAt); err != nil {
		return nil, err
	}
	if lastOpened.Valid {
		value := lastOpened.Time
		out.LastOpened = &value
	}
	return &out, nil
}

const readerNoteColumns = `id, title, published_content, published_revision, draft_content, draft_revision, draft_updated_at, deleted_at, created_at, updated_at`
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

// Qualified columns keep the sync/history joins unambiguous while remaining
// valid for the other queries that read directly from reader_thoughts.
const readerThoughtColumns = `reader_thoughts.id, reader_thoughts.host_kind, reader_thoughts.host_id, reader_thoughts.link_id, reader_thoughts.target, reader_thoughts.quote, reader_thoughts.body, reader_thoughts.source, reader_thoughts.deleted, reader_thoughts.last_sequence, reader_thoughts.winner_logical_clock, reader_thoughts.winner_device_id, reader_thoughts.winner_op_id, reader_thoughts.created_at, reader_thoughts.updated_at`
const readerTodoColumns = `id, text, due_at, done, origin_kind, origin_host_kind, origin_host_id, origin_ref, host_revision, completed_at, created_at, updated_at`

type duplicateThoughtOp struct {
	sequence  int64
	createdAt time.Time
}

func (r *PGXReaderVNextRepository) AppendThoughtOps(ctx context.Context, ops []model.ReaderThoughtOp) ([]model.ReaderThoughtAck, error) {
	if len(ops) == 0 {
		return []model.ReaderThoughtAck{}, nil
	}
	var acks []model.ReaderThoughtAck
	err := r.withTx(ctx, func(db database.Querier) error {
		duplicates, newOps, err := classifyThoughtOps(ctx, db, ops)
		if err != nil {
			return err
		}
		acks, err = r.appendThoughtOpsLocked(ctx, db, ops, newOps, duplicates)
		return err
	})
	if err != nil {
		return nil, err
	}
	return acks, nil
}

func classifyThoughtOps(ctx context.Context, db database.Querier, ops []model.ReaderThoughtOp) ([]*duplicateThoughtOp, []model.ReaderThoughtOp, error) {
	duplicates := make([]*duplicateThoughtOp, len(ops))
	newOps := make([]model.ReaderThoughtOp, 0, len(ops))
	for index, op := range ops {
		if err := validateReaderThoughtOpEnvelope(op); err != nil {
			return nil, nil, err
		}
		if op.Reattach != nil {
			sequence, exists, err := existingClientReattach(ctx, db, op)
			if err != nil {
				return nil, nil, err
			}
			if exists {
				duplicates[index] = &duplicateThoughtOp{sequence: sequence}
				continue
			}
			newOps = append(newOps, op)
			continue
		}
		sequence, createdAt, exists, err := readExistingThoughtOp(ctx, db, op)
		if err != nil {
			return nil, nil, err
		}
		if exists {
			duplicates[index] = &duplicateThoughtOp{sequence: sequence, createdAt: createdAt}
			continue
		}
		newOps = append(newOps, op)
	}
	return duplicates, newOps, nil
}

func (r *PGXReaderVNextRepository) appendThoughtOpsLocked(
	ctx context.Context,
	db database.Querier,
	ops, newOps []model.ReaderThoughtOp,
	duplicates []*duplicateThoughtOp,
) ([]model.ReaderThoughtAck, error) {
	// An accepted operation has an immutable envelope, so duplicates can be
	// recognized before checking whether their former host is still live.
	if err := lockReaderThoughtOpHosts(ctx, db, newOps); err != nil {
		return nil, err
	}
	if err := lockReaderThoughtOps(ctx, db, ops); err != nil {
		return nil, err
	}
	acks := make([]model.ReaderThoughtAck, 0, len(ops))
	for index, op := range ops {
		ack, err := r.appendOneThoughtOp(ctx, db, op, duplicates[index])
		if err != nil {
			return nil, err
		}
		acks = append(acks, ack)
	}
	if err := r.fillThoughtWinnerKeys(ctx, db, ops, acks); err != nil {
		return nil, err
	}
	return acks, nil
}

func (r *PGXReaderVNextRepository) appendOneThoughtOp(ctx context.Context, db database.Querier, op model.ReaderThoughtOp, existing *duplicateThoughtOp) (model.ReaderThoughtAck, error) {
	if existing != nil {
		op.CreatedAt = existing.createdAt
		return model.ReaderThoughtAck{OpID: op.OpID, Sequence: existing.sequence, Disposition: "duplicate", SubmittedKey: thoughtVersionKey(op)}, nil
	}
	if op.Reattach != nil {
		sequence, duplicate, err := r.appendClientReattachThoughtOp(ctx, db, op)
		if err != nil {
			return model.ReaderThoughtAck{}, err
		}
		disposition := "superseded"
		if duplicate {
			disposition = "duplicate"
		}
		return model.ReaderThoughtAck{OpID: op.OpID, Sequence: sequence, Disposition: disposition, SubmittedKey: thoughtVersionKey(op)}, nil
	}
	sequence, createdAt, duplicate, err := r.appendThoughtOp(ctx, db, op)
	if err != nil {
		return model.ReaderThoughtAck{}, err
	}
	op.CreatedAt = createdAt
	ack := model.ReaderThoughtAck{OpID: op.OpID, Sequence: sequence, Disposition: "superseded", SubmittedKey: thoughtVersionKey(op)}
	if duplicate {
		ack.Disposition = "duplicate"
		return ack, nil
	}
	if err := r.validateThoughtRecovery(ctx, db, op); err != nil {
		return model.ReaderThoughtAck{}, err
	}
	if err := r.materializeThought(ctx, db, op, sequence); err != nil {
		return model.ReaderThoughtAck{}, err
	}
	return ack, nil
}

func (r *PGXReaderVNextRepository) fillThoughtWinnerKeys(ctx context.Context, db database.Querier, ops []model.ReaderThoughtOp, acks []model.ReaderThoughtAck) error {
	for index := range acks {
		winner, err := r.readThoughtWinnerKey(ctx, db, ops[index].AnnotationID)
		if err != nil {
			return err
		}
		acks[index].WinnerKey = winner
		if acks[index].Disposition != "duplicate" && acks[index].SubmittedKey.Compare(winner) == 0 {
			acks[index].Disposition = "applied"
		}
	}
	return nil
}

// lockReaderThoughtOps serializes the previous-winner read with the ensuing
// materialization for every Thought in a batch. Existing Thought rows are
// locked before their advisory lock: server-derived writers already hold the
// row while they allocate their next logical clock, so the shared order must
// be row then advisory. A first write has no row to lock; its advisory lock
// still serializes concurrent creators. Acquiring Thought ids in stable order
// also keeps multi-Thought batches from deadlocking each other.
func lockReaderThoughtOps(ctx context.Context, db database.Querier, ops []model.ReaderThoughtOp) error {
	annotationIDs := make(map[string]struct{}, len(ops))
	for _, op := range ops {
		annotationIDs[op.AnnotationID] = struct{}{}
	}
	ordered := make([]string, 0, len(annotationIDs))
	for annotationID := range annotationIDs {
		ordered = append(ordered, annotationID)
	}
	sort.Strings(ordered)
	for _, annotationID := range ordered {
		if err := lockReaderThoughtOp(ctx, db, annotationID); err != nil {
			return err
		}
	}
	return nil
}

// lockReaderThoughtOp preserves the row-then-advisory order for every
// materializing Thought writer. The row lock serializes replacement writes;
// the advisory lock covers the no-row first-write case and makes the winner
// snapshot/event transition atomic across client and server-derived writers.
func lockReaderThoughtOp(ctx context.Context, db database.Querier, annotationID string) error {
	var lockedID string
	err := db.QueryRow(ctx, `
		SELECT id
		FROM reader_thoughts
		WHERE id=$1
		FOR UPDATE`, annotationID).Scan(&lockedID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lock thought materialization row: %w", err)
	}
	if _, err := db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, annotationID); err != nil {
		return fmt.Errorf("lock thought supersession: %w", err)
	}
	return nil
}

type readerThoughtHostLock struct {
	kind model.ReaderHostKind
	id   uuid.UUID
	mode readerThoughtHostLockMode
}

type readerThoughtHostLockMode string

const (
	readerThoughtHostShare  readerThoughtHostLockMode = "FOR SHARE"
	readerThoughtHostUpdate readerThoughtHostLockMode = "FOR UPDATE"
)

var errReaderThoughtHostUnavailable = errors.New("reader thought host is unavailable")

// lockReaderThoughtHost is the first step of every operation that changes a
// live Thought. The global order is host, Thought row, then Thought advisory
// lock. Host lifecycle writers already use host -> Thought, so keeping client
// and server-derived writers on this order prevents a host/Thought wait cycle.
func lockReaderThoughtHost(
	ctx context.Context,
	db database.Querier,
	kind model.ReaderHostKind,
	id uuid.UUID,
	mode readerThoughtHostLockMode,
) error {
	if mode != readerThoughtHostShare && mode != readerThoughtHostUpdate {
		return fmt.Errorf("%w: invalid thought host lock mode", ErrInvalidReaderThought)
	}
	var query string
	switch kind {
	case model.ReaderHostLink:
		query = `SELECT id FROM links WHERE id=$1 AND deleted_at IS NULL ` + string(mode)
	case model.ReaderHostInbox:
		query = `SELECT id FROM reader_inbox WHERE id=$1 AND deleted_at IS NULL ` + string(mode)
	case model.ReaderHostNote:
		query = `SELECT id FROM reader_notes WHERE id=$1 AND deleted_at IS NULL ` + string(mode)
	default:
		return fmt.Errorf("%w: invalid thought host kind", ErrInvalidReaderThought)
	}
	var lockedID uuid.UUID
	if err := db.QueryRow(ctx, query, id).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", errReaderThoughtHostUnavailable, kind)
		}
		return fmt.Errorf("lock %s thought host: %w", kind, err)
	}
	return nil
}

// lockReaderThoughtOpHosts serializes live thought writes with host lifecycle
// transitions. FOR SHARE permits concurrent thought batches on one host while
// conflicting with the FOR UPDATE lock used by delete, restore, and purge.
// Explicit thought deletion deliberately skips the host lock: deleting a
// thought must remain possible after its host entered trash or was purged.
func lockReaderThoughtOpHosts(ctx context.Context, db database.Querier, ops []model.ReaderThoughtOp) error {
	unique := make(map[string]readerThoughtHostLock, len(ops))
	for _, op := range ops {
		if op.OperationKind == "delete" {
			continue
		}
		kind := model.ReaderHostKind(strings.TrimSpace(op.HostKind))
		if !kind.Valid() {
			return fmt.Errorf("%w: invalid thought host kind", ErrInvalidReaderThought)
		}
		id, err := uuid.Parse(strings.TrimSpace(op.HostID))
		if err != nil {
			return fmt.Errorf("%w: invalid %s host id", ErrInvalidReaderThought, kind)
		}
		key := string(kind) + "\x00" + id.String()
		mode := readerThoughtHostShare
		if op.Reattach != nil {
			mode = readerThoughtHostUpdate
		}
		if current, ok := unique[key]; !ok || current.mode == readerThoughtHostShare && mode == readerThoughtHostUpdate {
			unique[key] = readerThoughtHostLock{kind: kind, id: id, mode: mode}
		}
	}
	locks := make([]readerThoughtHostLock, 0, len(unique))
	for _, lock := range unique {
		locks = append(locks, lock)
	}
	sort.Slice(locks, func(i, j int) bool {
		if locks[i].kind != locks[j].kind {
			return locks[i].kind < locks[j].kind
		}
		return locks[i].id.String() < locks[j].id.String()
	})
	for _, lock := range locks {
		if err := lockReaderThoughtHost(ctx, db, lock.kind, lock.id, lock.mode); err != nil {
			if errors.Is(err, errReaderThoughtHostUnavailable) {
				if lock.kind == model.ReaderHostLink {
					return ErrReaderThoughtLinkMismatch
				}
				return fmt.Errorf("%w: %s host is unavailable", ErrInvalidReaderThought, lock.kind)
			}
			return err
		}
	}
	return nil
}

func thoughtVersionKey(op model.ReaderThoughtOp) model.ReaderThoughtVersionKey {
	return model.ReaderThoughtVersionKey{
		LogicalClock: op.LogicalClock,
		DeviceID:     op.DeviceID,
		OpID:         op.OpID,
	}
}

func (r *PGXReaderVNextRepository) readThoughtWinnerKey(ctx context.Context, db database.Querier, annotationID string) (model.ReaderThoughtVersionKey, error) {
	var key model.ReaderThoughtVersionKey
	err := db.QueryRow(ctx, `
		SELECT winner_logical_clock,winner_device_id,winner_op_id
		FROM reader_thoughts
		WHERE id=$1`, annotationID).Scan(
		&key.LogicalClock, &key.DeviceID, &key.OpID,
	)
	if err != nil {
		return model.ReaderThoughtVersionKey{}, fmt.Errorf("read thought winner key: %w", err)
	}
	return key, nil
}

func validateReaderThoughtOpEnvelope(op model.ReaderThoughtOp) error {
	if op.LogicalClock < 0 || op.LogicalClock > model.ReaderThoughtMaxLogicalClock {
		return fmt.Errorf("%w: logical_clock is outside the safe range", ErrReaderThoughtClockInvalid)
	}
	if (op.RecoveryOf == nil) != (op.ExpectedWinnerKey == nil) {
		return fmt.Errorf("%w: incomplete recovery metadata", ErrInvalidReaderThought)
	}
	if op.Reattach != nil {
		if op.RecoveryOf != nil || op.ExpectedWinnerKey != nil || op.OperationKind != "update" ||
			op.Reattach.ExpectedLastSequence < 0 || op.Reattach.ExpectedHostRevision <= 0 {
			return fmt.Errorf("%w: invalid reattach metadata", ErrInvalidReaderThought)
		}
	}
	return nil
}

// readExistingThoughtOp verifies the full immutable operation envelope. It is
// safe to use before host liveness checks because reader_thought_ops rows are
// append-only; callers still take host -> Thought locks before any new write.
func readExistingThoughtOp(ctx context.Context, db database.Querier, op model.ReaderThoughtOp) (int64, time.Time, bool, error) {
	var existing struct {
		Sequence          int64
		CreatedAt         time.Time
		DeviceID          string
		LogicalClock      int64
		OperationKind     string
		AnnotationID      string
		HostKind          string
		HostID            string
		Target            []byte
		Payload           []byte
		RecoveryOf        []byte
		ExpectedWinnerKey []byte
	}
	// Provenance is part of the immutable envelope, even when this incoming
	// operation is not a recovery. Otherwise a normal operation could reuse a
	// recovery's op_id and be falsely acknowledged as an idempotent retry.
	err := db.QueryRow(ctx, `
		SELECT sequence,created_at,device_id,logical_clock,operation_kind,annotation_id,host_kind,host_id,target,payload,recovery_of,expected_winner_key
		FROM reader_thought_ops
		WHERE op_id=$1`, op.OpID).Scan(
		&existing.Sequence, &existing.CreatedAt, &existing.DeviceID, &existing.LogicalClock, &existing.OperationKind,
		&existing.AnnotationID, &existing.HostKind, &existing.HostID, &existing.Target, &existing.Payload,
		&existing.RecoveryOf, &existing.ExpectedWinnerKey,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, fmt.Errorf("read duplicate thought op: %w", err)
	}
	existingRecoveryOf, recoveryValid := thoughtVersionKeyFromJSON(existing.RecoveryOf)
	existingExpectedWinner, expectedWinnerValid := thoughtVersionKeyFromJSON(existing.ExpectedWinnerKey)
	if existing.DeviceID != op.DeviceID ||
		existing.OperationKind != op.OperationKind ||
		existing.AnnotationID != op.AnnotationID ||
		existing.HostKind != op.HostKind ||
		existing.HostID != op.HostID ||
		!readerJSONEqual(existing.Target, rawJSON(op.Target)) ||
		existing.LogicalClock != op.LogicalClock ||
		!readerJSONEqual(existing.Payload, rawJSON(op.Payload)) ||
		!recoveryValid || !expectedWinnerValid ||
		!thoughtVersionKeysEqual(existingRecoveryOf, op.RecoveryOf) ||
		!thoughtVersionKeysEqual(existingExpectedWinner, op.ExpectedWinnerKey) {
		return 0, time.Time{}, false, fmt.Errorf("%w: op_id=%s", ErrReaderThoughtOpConflict, op.OpID)
	}
	return existing.Sequence, existing.CreatedAt, true, nil
}

func (r *PGXReaderVNextRepository) appendThoughtOp(ctx context.Context, db database.Querier, op model.ReaderThoughtOp) (int64, time.Time, bool, error) {
	if err := validateReaderThoughtOpEnvelope(op); err != nil {
		return 0, time.Time{}, false, err
	}
	var sequence int64
	var createdAt time.Time
	insert := `
		INSERT INTO reader_thought_ops
			(op_id, device_id, operation_kind, annotation_id, host_kind, host_id, target, payload, logical_clock)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb,$9)
		ON CONFLICT (op_id) DO NOTHING
		RETURNING sequence,created_at`
	args := []any{op.OpID, op.DeviceID, op.OperationKind, op.AnnotationID, op.HostKind, op.HostID, rawJSON(op.Target), rawJSON(op.Payload), op.LogicalClock}
	if op.RecoveryOf != nil {
		insert = `
			INSERT INTO reader_thought_ops
			(op_id, device_id, operation_kind, annotation_id, host_kind, host_id, target, payload, logical_clock,recovery_of,expected_winner_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb,$9,$10::jsonb,$11::jsonb)
		ON CONFLICT (op_id) DO NOTHING
		RETURNING sequence,created_at`
		args = append(args, thoughtVersionKeyJSON(op.RecoveryOf), thoughtVersionKeyJSON(op.ExpectedWinnerKey))
	}
	err := db.QueryRow(ctx, insert, args...).Scan(&sequence, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		sequence, createdAt, exists, readErr := readExistingThoughtOp(ctx, db, op)
		if readErr != nil {
			return 0, time.Time{}, false, readErr
		}
		if !exists {
			return 0, time.Time{}, false, fmt.Errorf("read duplicate thought op: conflict row disappeared")
		}
		return sequence, createdAt, true, nil
	} else if err != nil {
		return 0, time.Time{}, false, fmt.Errorf("append thought op: %w", err)
	}
	return sequence, createdAt, false, nil
}

type readerThoughtReattachMarker struct {
	ExpectedLastSequence int64 `json:"expected_last_sequence"`
	ExpectedHostRevision int64 `json:"expected_host_revision"`
}

type existingReaderThoughtReattach struct {
	Sequence          int64
	DeviceID          string
	LogicalClock      int64
	OperationKind     string
	AnnotationID      string
	HostKind          string
	HostID            string
	Target            []byte
	Payload           []byte
	RecoveryOf        []byte
	ExpectedWinnerKey []byte
}

func readerThoughtReattachMarkerFromPayload(raw json.RawMessage) (*readerThoughtReattachMarker, bool) {
	var payload struct {
		Reattach *readerThoughtReattachMarker `json:"reattach"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Reattach == nil ||
		payload.Reattach.ExpectedLastSequence < 0 || payload.Reattach.ExpectedHostRevision <= 0 {
		return nil, false
	}
	return payload.Reattach, true
}

func readerThoughtReattachMatches(existing existingReaderThoughtReattach, op model.ReaderThoughtOp) bool {
	marker, marked := readerThoughtReattachMarkerFromPayload(existing.Payload)
	recoveryOf, recoveryValid := thoughtVersionKeyFromJSON(existing.RecoveryOf)
	expectedWinner, winnerValid := thoughtVersionKeyFromJSON(existing.ExpectedWinnerKey)
	return op.Reattach != nil && marked && recoveryValid && winnerValid && recoveryOf == nil && expectedWinner == nil &&
		existing.DeviceID == op.DeviceID && existing.LogicalClock == op.LogicalClock &&
		existing.OperationKind == "update" && existing.AnnotationID == op.AnnotationID &&
		existing.HostKind == op.HostKind && existing.HostID == op.HostID &&
		readerJSONEqual(existing.Target, rawJSON(op.Target)) &&
		marker.ExpectedLastSequence == op.Reattach.ExpectedLastSequence &&
		marker.ExpectedHostRevision == op.Reattach.ExpectedHostRevision
}

// existingClientReattach verifies a prior server-rebuilt reattach using the
// client command's immutable identity and its persisted CAS marker. It runs
// before lifecycle checks so a lost-response retry remains a duplicate even
// after the tombstone was cleared or a later operation won.
func existingClientReattach(ctx context.Context, db database.Querier, op model.ReaderThoughtOp) (int64, bool, error) {
	var existing existingReaderThoughtReattach
	err := db.QueryRow(ctx, `
		SELECT sequence,device_id,logical_clock,operation_kind,annotation_id,host_kind,host_id,target,payload,recovery_of,expected_winner_key
		FROM reader_thought_ops
		WHERE op_id=$1`, op.OpID).Scan(
		&existing.Sequence, &existing.DeviceID, &existing.LogicalClock, &existing.OperationKind,
		&existing.AnnotationID, &existing.HostKind, &existing.HostID, &existing.Target, &existing.Payload,
		&existing.RecoveryOf, &existing.ExpectedWinnerKey,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read existing client reattach: %w", err)
	}
	if !readerThoughtReattachMatches(existing, op) {
		return 0, false, fmt.Errorf("%w: op_id=%s", ErrReaderThoughtOpConflict, op.OpID)
	}
	return existing.Sequence, true, nil
}

// appendClientReattachThoughtOp turns one durable browser command into a
// normal update operation rebuilt exclusively from the immutable tombstone.
// AppendThoughtOps has already locked the target host, Thought row, and
// advisory key in global order before this method is reached.
//
//nolint:gocyclo // one transaction must preserve duplicate, lifecycle, host CAS, materialization, winner CAS, and tombstone-clear order.
func (r *PGXReaderVNextRepository) appendClientReattachThoughtOp(ctx context.Context, db database.Querier, op model.ReaderThoughtOp) (int64, bool, error) {
	if op.Reattach == nil {
		return 0, false, ErrInvalidReaderThought
	}
	if sequence, found, err := existingClientReattach(ctx, db, op); err != nil || found {
		return sequence, found, err
	}
	item, err := scanReaderThought(db.QueryRow(ctx, `SELECT `+readerThoughtColumns+` FROM reader_thoughts WHERE id=$1 AND deleted=false FOR UPDATE`, op.AnnotationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, ErrNotFound
	}
	if err != nil {
		return 0, false, fmt.Errorf("read thought for client reattach: %w", err)
	}
	if op.Reattach.ExpectedLastSequence != item.LastSequence || thoughtVersionKey(op).Compare(item.WinnerKey) <= 0 {
		return 0, false, ErrRevisionConflict
	}
	var snapshot []byte
	if err := db.QueryRow(ctx, `SELECT snapshot FROM reader_thought_tombstones WHERE thought_id=$1 FOR UPDATE`, op.AnnotationID).Scan(&snapshot); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, ErrReaderThoughtReattachInvalidState
		}
		return 0, false, fmt.Errorf("read client reattach snapshot: %w", err)
	}
	if err := applyReaderThoughtReplaySnapshot(item, snapshot); err != nil {
		return 0, false, fmt.Errorf("decode client reattach snapshot: %w", err)
	}
	command := model.ReaderThoughtReattachCommand{
		ThoughtID: op.AnnotationID, TargetHostKind: op.HostKind, TargetHostID: op.HostID,
		ExpectedLastSequence: op.Reattach.ExpectedLastSequence, ExpectedHostRevision: op.Reattach.ExpectedHostRevision,
	}
	hostRevision, err := readerReattachHost(ctx, db, command)
	if err != nil {
		return 0, false, err
	}
	if hostRevision != op.Reattach.ExpectedHostRevision {
		return 0, false, ErrRevisionConflict
	}
	hostBody, err := readerReattachHostBody(ctx, db, command.TargetHostKind, command.TargetHostID)
	if err != nil {
		return 0, false, err
	}
	target, payload, err := readerReattachThoughtPayloadWithMarker(item, command, hostRevision, hostBody, op.Reattach)
	if err != nil {
		return 0, false, err
	}
	if !readerJSONEqual(target, rawJSON(op.Target)) {
		return 0, false, ErrInvalidReaderThought
	}
	materialized := op
	materialized.Target = target
	materialized.Payload = payload
	materialized.Reattach = nil
	sequence, createdAt, duplicate, err := r.appendThoughtOp(ctx, db, materialized)
	if err != nil || duplicate {
		return sequence, duplicate, err
	}
	materialized.CreatedAt = createdAt
	if err := r.materializeThought(ctx, db, materialized, sequence); err != nil {
		return 0, false, err
	}
	winner, err := r.readThoughtWinnerKey(ctx, db, op.AnnotationID)
	if err != nil {
		return 0, false, err
	}
	if thoughtVersionKey(op).Compare(winner) != 0 {
		return 0, false, ErrRevisionConflict
	}
	if _, err := db.Exec(ctx, `DELETE FROM reader_thought_tombstones WHERE thought_id=$1`, op.AnnotationID); err != nil {
		return 0, false, fmt.Errorf("clear client reattach tombstone: %w", err)
	}
	return sequence, false, nil
}

func thoughtVersionKeyJSON(key *model.ReaderThoughtVersionKey) []byte {
	if key == nil {
		return nil
	}
	value, err := json.Marshal(map[string]any{
		"logical_clock": key.LogicalClock,
		"device_id":     key.DeviceID,
		"op_id":         key.OpID,
	})
	if err != nil {
		return nil
	}
	return value
}

func thoughtVersionKeyFromJSON(raw []byte) (*model.ReaderThoughtVersionKey, bool) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 3 ||
		fields["logical_clock"] == nil || fields["device_id"] == nil || fields["op_id"] == nil {
		return nil, false
	}
	var value struct {
		LogicalClock int64  `json:"logical_clock"`
		DeviceID     string `json:"device_id"`
		OpID         string `json:"op_id"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false
	}
	if value.LogicalClock < 0 || value.LogicalClock > model.ReaderThoughtMaxLogicalClock ||
		value.DeviceID == "" || value.OpID == "" {
		return nil, false
	}
	return &model.ReaderThoughtVersionKey{LogicalClock: value.LogicalClock, DeviceID: value.DeviceID, OpID: value.OpID}, true
}

func thoughtVersionKeysEqual(left, right *model.ReaderThoughtVersionKey) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.LogicalClock == right.LogicalClock &&
		left.DeviceID == right.DeviceID && left.OpID == right.OpID
}

func (r *PGXReaderVNextRepository) validateThoughtRecovery(ctx context.Context, db database.Querier, op model.ReaderThoughtOp) error {
	if op.RecoveryOf == nil && op.ExpectedWinnerKey == nil {
		return nil
	}
	if op.RecoveryOf == nil || op.ExpectedWinnerKey == nil {
		return fmt.Errorf("%w: incomplete recovery metadata", ErrInvalidReaderThought)
	}
	var recoverable bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM reader_thought_supersession_events event
			JOIN reader_thought_ops loser
			  ON loser.sequence=event.loser_sequence
			WHERE event.annotation_id=$1
			  AND loser.logical_clock=$2
			  AND loser.device_id=$3
			  AND loser.op_id=$4
		)`,
		op.AnnotationID,
		op.RecoveryOf.LogicalClock, op.RecoveryOf.DeviceID, op.RecoveryOf.OpID,
	).Scan(&recoverable); err != nil {
		return fmt.Errorf("verify thought recovery provenance: %w", err)
	}
	if !recoverable {
		return fmt.Errorf("%w: recovery_of is not a durable superseded version", ErrInvalidReaderThought)
	}
	var current model.ReaderThoughtVersionKey
	err := db.QueryRow(ctx, `
		SELECT winner_logical_clock,winner_device_id,winner_op_id
		FROM reader_thoughts
		WHERE id=$1
		FOR UPDATE`, op.AnnotationID).Scan(
		&current.LogicalClock, &current.DeviceID, &current.OpID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no current winner", ErrReaderThoughtRecoveryConflict)
	}
	if err != nil {
		return fmt.Errorf("lock thought recovery winner: %w", err)
	}
	if current.Compare(*op.ExpectedWinnerKey) != 0 {
		return fmt.Errorf("%w: expected=%d/%s/%s actual=%d/%s/%s",
			ErrReaderThoughtRecoveryConflict,
			op.ExpectedWinnerKey.LogicalClock, op.ExpectedWinnerKey.DeviceID, op.ExpectedWinnerKey.OpID,
			current.LogicalClock, current.DeviceID, current.OpID,
		)
	}
	return nil
}

// appendDerivedThoughtOp serializes server-owned writers per thought,
// reuses an already accepted idempotent operation's clock, and otherwise
// allocates exactly current_winner_clock+1 in the caller's transaction.
func (r *PGXReaderVNextRepository) appendDerivedThoughtOp(
	ctx context.Context,
	db database.Querier,
	op model.ReaderThoughtOp,
) (model.ReaderThoughtOp, int64, bool, error) {
	var existingClock int64
	err := db.QueryRow(ctx, `
		SELECT logical_clock
		FROM reader_thought_ops
		WHERE op_id=$1`, op.OpID).Scan(&existingClock)
	if err == nil {
		op.LogicalClock = existingClock
		sequence, createdAt, duplicate, appendErr := r.appendThoughtOp(ctx, db, op)
		op.CreatedAt = createdAt
		return op, sequence, duplicate, appendErr
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return model.ReaderThoughtOp{}, 0, false, fmt.Errorf("read derived thought operation: %w", err)
	}
	if err := lockReaderThoughtOp(ctx, db, op.AnnotationID); err != nil {
		return model.ReaderThoughtOp{}, 0, false, err
	}

	var winnerClock int64
	err = db.QueryRow(ctx, `
		SELECT winner_logical_clock
		FROM reader_thoughts
		WHERE id=$1
		FOR UPDATE`, op.AnnotationID).Scan(&winnerClock)
	if errors.Is(err, pgx.ErrNoRows) {
		winnerClock = 0
	} else if err != nil {
		return model.ReaderThoughtOp{}, 0, false, fmt.Errorf("lock thought winner: %w", err)
	}
	if winnerClock < 0 || winnerClock > model.ReaderThoughtMaxLogicalClock {
		return model.ReaderThoughtOp{}, 0, false, fmt.Errorf("%w: persisted winner clock is invalid", ErrReaderThoughtClockInvalid)
	}
	if winnerClock == model.ReaderThoughtMaxLogicalClock {
		return model.ReaderThoughtOp{}, 0, false, ErrReaderThoughtClockExhausted
	}
	op.LogicalClock = winnerClock + 1
	sequence, createdAt, duplicate, err := r.appendThoughtOp(ctx, db, op)
	op.CreatedAt = createdAt
	return op, sequence, duplicate, err
}

func readerJSONEqual(left, right []byte) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

type readerThoughtPayload struct {
	Body   string          `json:"body"`
	Quote  json.RawMessage `json:"quote"`
	Source string          `json:"source"`
	LinkID string          `json:"link_id"`
}

func readReaderThoughtPayload(ctx context.Context, db database.Querier, op model.ReaderThoughtOp) (readerThoughtPayload, *uuid.UUID, error) {
	var payload readerThoughtPayload
	if err := json.Unmarshal(op.Payload, &payload); err != nil {
		return payload, nil, fmt.Errorf("decode thought payload: %w", err)
	}
	if payload.Source == "" {
		payload.Source = "user"
	}
	if op.HostKind != "link" {
		if payload.LinkID != "" {
			return payload, nil, fmt.Errorf("%w: non-link host cannot carry link_id", ErrReaderThoughtLinkMismatch)
		}
		return payload, nil, nil
	}

	hostLinkID, err := uuid.Parse(op.HostID)
	if err != nil {
		return payload, nil, fmt.Errorf("%w: invalid link host id", ErrReaderThoughtLinkMismatch)
	}
	if payload.LinkID != "" {
		payloadLinkID, err := uuid.Parse(payload.LinkID)
		if err != nil || payloadLinkID != hostLinkID {
			return payload, nil, fmt.Errorf("%w: payload link_id differs from host", ErrReaderThoughtLinkMismatch)
		}
	}
	if op.OperationKind != "delete" {
		var exists bool
		if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM links WHERE id=$1 AND deleted_at IS NULL)`, hostLinkID).Scan(&exists); err != nil {
			return payload, nil, fmt.Errorf("check thought link host: %w", err)
		}
		if !exists {
			return payload, nil, ErrReaderThoughtLinkMismatch
		}
	}
	return payload, &hostLinkID, nil
}

func (r *PGXReaderVNextRepository) materializeThought(ctx context.Context, db database.Querier, op model.ReaderThoughtOp, sequence int64) error {
	// Read the previous winner before the conditional upsert.  The event is a
	// write-time fact, not a query-time comparison against a future winner.
	var previous model.ReaderThoughtConflictOperation
	previousOK := false
	var err error
	previous, previousOK, err = r.readThoughtOperation(ctx, db, op.AnnotationID)
	if err != nil {
		return err
	}
	payload, linkID, err := readReaderThoughtPayload(ctx, db, op)
	if err != nil {
		return err
	}
	deleted := op.OperationKind == "delete"
	quote := payload.Quote
	if len(quote) == 0 {
		quote = nil
	}
	target := rawJSON(op.Target)
	if deleted {
		linkID = nil
		target = []byte(`{}`)
		quote = nil
		payload.Body = ""
		payload.Source = ""
	}
	// Keep the legacy sequence predicate syntactically present for the
	// materialized projection contract, but make it an exhaustive partition.
	// The actual winner gate below is solely the stable client version key.
	tag, err := db.Exec(ctx, `
		INSERT INTO reader_thoughts
			(id, host_kind, host_id, link_id, target, quote, body, source, deleted, last_sequence,
			 winner_logical_clock,winner_device_id,winner_op_id)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (id) DO UPDATE SET
			host_kind=CASE WHEN EXCLUDED.deleted THEN reader_thoughts.host_kind ELSE EXCLUDED.host_kind END,
			host_id=CASE WHEN EXCLUDED.deleted THEN reader_thoughts.host_id ELSE EXCLUDED.host_id END,
			link_id=CASE WHEN EXCLUDED.deleted THEN NULL ELSE EXCLUDED.link_id END,
			target=EXCLUDED.target,
			quote=EXCLUDED.quote,
			body=EXCLUDED.body,
			source=EXCLUDED.source,
			deleted=EXCLUDED.deleted, last_sequence=EXCLUDED.last_sequence,
			winner_logical_clock=EXCLUDED.winner_logical_clock,
			winner_device_id=EXCLUDED.winner_device_id,
			winner_op_id=EXCLUDED.winner_op_id,
		updated_at=NOW()
	WHERE (EXCLUDED.winner_logical_clock, convert_to(EXCLUDED.winner_device_id,'UTF8'), convert_to(EXCLUDED.winner_op_id,'UTF8'))
		> (reader_thoughts.winner_logical_clock, convert_to(reader_thoughts.winner_device_id,'UTF8'), convert_to(reader_thoughts.winner_op_id,'UTF8'))`,
		op.AnnotationID, op.HostKind, op.HostID, linkID, target, rawJSON(quote), payload.Body, payload.Source, deleted, sequence, op.LogicalClock, op.DeviceID, op.OpID)
	if err != nil {
		return fmt.Errorf("materialize thought: %w", err)
	}
	if deleted && tag.RowsAffected() > 0 {
		// Delete has no LWW privilege. Only a delete that won the projection may
		// replace a host lifecycle snapshot with the minimal anti-resurrection
		// marker. A delayed losing delete remains an append-only sync event and
		// must leave the frozen history intact.
		if _, err := db.Exec(ctx, `
			INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason,snapshot)
			VALUES ($1,$2,$3,'user_deleted',jsonb_build_object('id',$1::text,'host_kind',$2::text,'host_id',$3::text,'lifecycle_sequence',$4::bigint))
			ON CONFLICT (thought_id) DO UPDATE
			SET reason='user_deleted',snapshot=jsonb_build_object('id',EXCLUDED.thought_id,'host_kind',EXCLUDED.host_kind,'host_id',EXCLUDED.host_id,'lifecycle_sequence',$4::bigint),created_at=NOW()`,
			op.AnnotationID, op.HostKind, op.HostID, sequence); err != nil {
			return fmt.Errorf("mark thought user deletion: %w", err)
		}
	} else if !deleted && tag.RowsAffected() > 0 && previousOK && previous.OperationKind == "delete" {
		// A user-delete marker belongs to the delete version that created it;
		// it cannot give that version priority over a later operation in the
		// total LWW order. Clearing only user_deleted preserves immutable host
		// lifecycle snapshots while allowing a genuinely newer client version
		// to converge regardless of delivery order.
		if _, err := db.Exec(ctx, `DELETE FROM reader_thought_tombstones WHERE thought_id=$1 AND reason='user_deleted'`, op.AnnotationID); err != nil {
			return fmt.Errorf("clear superseded thought user deletion: %w", err)
		}
	}
	// The projection is maintained here rather than at each command, because
	// every Thought write — add, update, delete, reattach, lifecycle, note
	// reanchor, checkbox writeback, history restore — ends in this function.
	// It runs after the tombstone bookkeeping above so it observes the final
	// liveness of the Thought, not the intermediate one.
	if err := r.replaceThoughtTodoProjectionsOn(ctx, db, op.AnnotationID); err != nil {
		return err
	}
	return r.recordThoughtSupersession(ctx, db, op, sequence, previous, previousOK)
}

func (r *PGXReaderVNextRepository) recordThoughtSupersession(ctx context.Context, db database.Querier, op model.ReaderThoughtOp, sequence int64, previous model.ReaderThoughtConflictOperation, previousOK bool) error {
	if !previousOK {
		return nil
	}
	incoming := thoughtConflictOperation(op, sequence)
	winner, loser := incoming, previous
	if thoughtOperationKey(previous).Compare(thoughtOperationKey(incoming)) > 0 {
		winner, loser = previous, incoming
	}
	if winner.OpID == loser.OpID {
		return nil
	}
	if err := r.appendThoughtSupersessionEvent(ctx, db, op.AnnotationID, loser, winner); err != nil {
		return err
	}
	return nil
}

func thoughtConflictOperation(op model.ReaderThoughtOp, sequence int64) model.ReaderThoughtConflictOperation {
	return model.ReaderThoughtConflictOperation{Sequence: sequence, OpID: op.OpID, DeviceID: op.DeviceID, LogicalClock: op.LogicalClock, OperationKind: op.OperationKind, AnnotationID: op.AnnotationID, HostKind: op.HostKind, HostID: op.HostID, Target: append(json.RawMessage(nil), op.Target...), Payload: append(json.RawMessage(nil), op.Payload...), RecoveryOf: cloneThoughtVersionKey(op.RecoveryOf), ExpectedWinnerKey: cloneThoughtVersionKey(op.ExpectedWinnerKey), CreatedAt: op.CreatedAt}
}

func cloneThoughtVersionKey(key *model.ReaderThoughtVersionKey) *model.ReaderThoughtVersionKey {
	if key == nil {
		return nil
	}
	value := *key
	return &value
}

func thoughtOperationKey(op model.ReaderThoughtConflictOperation) model.ReaderThoughtVersionKey {
	return model.ReaderThoughtVersionKey{LogicalClock: op.LogicalClock, DeviceID: op.DeviceID, OpID: op.OpID}
}

func (r *PGXReaderVNextRepository) readThoughtOperation(ctx context.Context, db database.Querier, annotationID string) (model.ReaderThoughtConflictOperation, bool, error) {
	var op model.ReaderThoughtConflictOperation
	var target, payload []byte
	var recoveryOf, expectedWinner []byte
	err := db.QueryRow(ctx, `SELECT operation.sequence,operation.op_id,operation.device_id,operation.logical_clock,operation.operation_kind,operation.annotation_id,operation.host_kind,operation.host_id,operation.target,operation.payload,operation.recovery_of,operation.expected_winner_key,operation.created_at FROM reader_thoughts thought JOIN reader_thought_ops operation ON operation.sequence=thought.last_sequence WHERE thought.id=$1`, annotationID).Scan(&op.Sequence, &op.OpID, &op.DeviceID, &op.LogicalClock, &op.OperationKind, &op.AnnotationID, &op.HostKind, &op.HostID, &target, &payload, &recoveryOf, &expectedWinner, &op.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ReaderThoughtConflictOperation{}, false, nil
	}
	if err != nil {
		return model.ReaderThoughtConflictOperation{}, false, fmt.Errorf("read thought winner operation: %w", err)
	}
	op.Target, op.Payload = append(json.RawMessage(nil), target...), append(json.RawMessage(nil), payload...)
	parsedRecoveryOf, recoveryValid := thoughtVersionKeyFromJSON(recoveryOf)
	parsedExpectedWinner, expectedWinnerValid := thoughtVersionKeyFromJSON(expectedWinner)
	op.RecoveryOf = parsedRecoveryOf
	op.ExpectedWinnerKey = parsedExpectedWinner
	if !recoveryValid || !expectedWinnerValid || (op.RecoveryOf == nil) != (op.ExpectedWinnerKey == nil) {
		return model.ReaderThoughtConflictOperation{}, false, fmt.Errorf("read thought winner recovery metadata: invalid recovery provenance")
	}
	return op, true, nil
}

func (r *PGXReaderVNextRepository) appendThoughtSupersessionEvent(ctx context.Context, db database.Querier, annotationID string, loser, winner model.ReaderThoughtConflictOperation) error {
	loserJSON, err := json.Marshal(loser)
	if err != nil {
		return fmt.Errorf("encode thought supersession loser: %w", err)
	}
	winnerJSON, err := json.Marshal(winner)
	if err != nil {
		return fmt.Errorf("encode thought supersession winner: %w", err)
	}
	_, err = db.Exec(ctx, `INSERT INTO reader_thought_supersession_events (annotation_id,loser_sequence,winner_sequence,loser,winner_at_detection) VALUES ($1,$2,$3,$4::jsonb,$5::jsonb) ON CONFLICT (loser_sequence) DO NOTHING`, annotationID, loser.Sequence, winner.Sequence, loserJSON, winnerJSON)
	if err != nil {
		return fmt.Errorf("append thought supersession event: %w", err)
	}
	return nil
}

func (r *PGXReaderVNextRepository) ListThoughts(ctx context.Context, query, after string, limit int) ([]model.ReaderThought, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	at, id, err := parseReaderCursor(after)
	if err != nil {
		return nil, "", err
	}
	sql := `SELECT ` + readerThoughtColumns + ` FROM reader_thoughts WHERE deleted=false AND NOT EXISTS (SELECT 1 FROM reader_thought_tombstones tt WHERE tt.thought_id=reader_thoughts.id)`
	args := []any{}
	if strings.TrimSpace(query) != "" {
		sql += fmt.Sprintf(` AND body ILIKE $%d`, len(args)+1)
		args = append(args, "%"+strings.TrimSpace(query)+"%")
	}
	if !at.IsZero() {
		sql += fmt.Sprintf(` AND (updated_at < $%d OR (updated_at = $%d AND id < $%d))`, len(args)+1, len(args)+1, len(args)+2)
		args = append(args, at, id)
	}
	sql += fmt.Sprintf(` ORDER BY updated_at DESC, id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list thoughts: %w", err)
	}
	defer rows.Close()
	items := make([]model.ReaderThought, 0)
	for rows.Next() {
		item, err := scanReaderThought(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan thought: %w", err)
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) == limit {
		last := items[len(items)-1]
		return items, readerCursor(last.UpdatedAt, last.ID), nil
	}
	return items, "", nil
}

const thoughtSearchCursorVersion = 2

type thoughtSearchCursorPayload struct {
	Version          int    `json:"v"`
	UpdatedAt        string `json:"updated_at"`
	ThoughtID        string `json:"thought_id"`
	Scope            string `json:"scope"`
	SnapshotSequence int64  `json:"snapshot_sequence"`
	SnapshotAt       string `json:"snapshot_at"`
	Total            int    `json:"total"`
}

func canonicalThoughtSearchQuery(query string) string {
	return strings.ToLower(strings.TrimSpace(query))
}

func thoughtSearchCursorScope(query string) string {
	digest := sha256.Sum256([]byte(canonicalThoughtSearchQuery(query)))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func thoughtSearchCursor(at time.Time, id, query string, snapshotSequence int64, snapshotAt time.Time, total int) (string, error) {
	payload, err := json.Marshal(thoughtSearchCursorPayload{
		Version:          thoughtSearchCursorVersion,
		UpdatedAt:        at.UTC().Format(time.RFC3339Nano),
		ThoughtID:        id,
		Scope:            thoughtSearchCursorScope(query),
		SnapshotSequence: snapshotSequence,
		SnapshotAt:       snapshotAt.UTC().Format(time.RFC3339Nano),
		Total:            total,
	})
	if err != nil {
		return "", fmt.Errorf("encode thought search cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func parseThoughtSearchCursor(raw, query string) (time.Time, string, int64, time.Time, int, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, "", 0, time.Time{}, -1, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", 0, time.Time{}, 0, fmt.Errorf("%w: malformed thought search cursor", ErrInvalidReaderCursor)
	}
	var payload thoughtSearchCursorPayload
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return time.Time{}, "", 0, time.Time{}, 0, fmt.Errorf("%w: malformed thought search cursor", ErrInvalidReaderCursor)
	}
	if payload.Version != thoughtSearchCursorVersion || payload.ThoughtID == "" || payload.Scope == "" || payload.SnapshotSequence < 0 || payload.Total < 0 {
		return time.Time{}, "", 0, time.Time{}, 0, fmt.Errorf("%w: malformed thought search cursor", ErrInvalidReaderCursor)
	}
	at, err := time.Parse(time.RFC3339Nano, payload.UpdatedAt)
	if err != nil {
		return time.Time{}, "", 0, time.Time{}, 0, fmt.Errorf("%w: malformed thought search cursor", ErrInvalidReaderCursor)
	}
	snapshotAt, err := time.Parse(time.RFC3339Nano, payload.SnapshotAt)
	if err != nil {
		return time.Time{}, "", 0, time.Time{}, 0, fmt.Errorf("%w: malformed thought search cursor", ErrInvalidReaderCursor)
	}
	if subtle.ConstantTimeCompare([]byte(payload.Scope), []byte(thoughtSearchCursorScope(query))) != 1 {
		return time.Time{}, "", 0, time.Time{}, 0, fmt.Errorf("%w: thought search cursor scope mismatch", ErrInvalidReaderCursor)
	}
	return at, payload.ThoughtID, payload.SnapshotSequence, snapshotAt, payload.Total, nil
}

func scanThoughtSearchRows(rows pgx.Rows, limit int, query string, cursorTotal int) ([]model.ReaderThoughtSearch, int, string, error) {
	items := make([]model.ReaderThoughtSearch, 0)
	total := cursorTotal
	totalIsAuthoritative := cursorTotal >= 0
	var snapshotSequence int64
	var snapshotAt time.Time
	for rows.Next() {
		var item model.ReaderThoughtSearch
		var rawLinkID string
		var totalRows int64
		var reason pgtype.Text
		if err := rows.Scan(&item.ID, &item.HostKind, &item.HostID, &rawLinkID, &item.Snippet, &totalRows, &item.UpdatedAt, &item.LifecycleStatus, &reason, &item.HistoryDeepLink, &snapshotSequence, &snapshotAt); err != nil {
			return nil, 0, "", fmt.Errorf("scan thought search result: %w", err)
		}
		if rawLinkID != "" {
			linkID, err := uuid.Parse(rawLinkID)
			if err != nil {
				return nil, 0, "", fmt.Errorf("scan thought search link id: %w", err)
			}
			item.LinkID = &linkID
		}
		if reason.Valid {
			value := reason.String
			item.LifecycleReason = &value
		}
		if itemTotal := int(totalRows); !totalIsAuthoritative && (total < 0 || itemTotal > total) {
			total = itemTotal
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, "", err
	}
	if len(items) <= limit {
		if total < 0 {
			total = 0
		}
		return items, total, "", nil
	}
	items = items[:limit]
	last := items[len(items)-1]
	nextCursor, err := thoughtSearchCursor(last.UpdatedAt, last.ID, query, snapshotSequence, snapshotAt, total)
	if err != nil {
		return nil, 0, "", err
	}
	return items, total, nextCursor, nil
}

func (r *PGXReaderVNextRepository) SearchThoughts(ctx context.Context, query, after string, limit int) ([]model.ReaderThoughtSearch, int, string, error) {
	query = canonicalThoughtSearchQuery(query)
	if query == "" {
		return []model.ReaderThoughtSearch{}, 0, "", nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	at, id, snapshotSequence, snapshotAt, cursorTotal, err := parseThoughtSearchCursor(after, query)
	if err != nil {
		return nil, 0, "", fmt.Errorf("parse thought search cursor: %w", err)
	}
	// Count before applying the keyset condition so every returned page carries
	// the total match hint, rather than only the number still after its cursor.
	sql := `WITH search_authority AS (
		SELECT COALESCE($2::bigint, GREATEST(
			(SELECT COALESCE(max(sequence),0) FROM reader_thought_ops),
			(SELECT COALESCE(max(last_sequence),0) FROM reader_thoughts)
		)) AS snapshot_sequence,
		COALESCE($3::timestamptz,statement_timestamp()) AS snapshot_at
	), search_candidates AS MATERIALIZED (
		-- 唯一目的是让 planner 分别命中 active / tombstone 的两个 trigram 索引。
		-- 两个分支的表达式与下面 WHERE 里那组 ILIKE 完全同构：某一列包含
		-- %query% 的子串，拼接串必然也包含它，所以这里是**必要条件**，
		-- 不是新的过滤器——多进来的行照样被下面原封不动的 OR 复核掉。
		-- 语义因此逐字不变；变的只有扫描方式。
		-- UNION（非 UNION ALL）保证一个 thought_id 至多出现一次，JOIN 不会放大行数。
		SELECT thought.id AS thought_id
		FROM reader_thoughts thought
		WHERE (thought.body || ' ' || thought.source || ' ' || COALESCE(thought.quote::text,'')) ILIKE $1
		UNION
		SELECT tombstone.thought_id
		FROM reader_thought_tombstones tombstone
		WHERE (COALESCE(tombstone.snapshot->>'body','') || ' ' || COALESCE(tombstone.snapshot->>'source','')
			|| ' ' || COALESCE((tombstone.snapshot->'quote')::text,'')) ILIKE $1
	), matching_thoughts AS (
		SELECT thought.id AS thought_id,
			CASE WHEN tombstone.thought_id IS NULL THEN thought.host_kind ELSE tombstone.snapshot->>'host_kind' END AS host_kind,
			CASE WHEN tombstone.thought_id IS NULL THEN thought.host_id ELSE tombstone.snapshot->>'host_id' END AS host_id,
			CASE WHEN tombstone.thought_id IS NULL THEN COALESCE(thought.link_id::text, '') ELSE COALESCE(tombstone.snapshot->>'link_id', '') END AS link_id,
			left(regexp_replace(CASE WHEN tombstone.thought_id IS NULL THEN thought.body ELSE tombstone.snapshot->>'body' END, '\s+', ' ', 'g'), 240) AS snippet,
			count(*) OVER () AS total_count,
			CASE WHEN tombstone.thought_id IS NULL THEN thought.updated_at ELSE (tombstone.snapshot->>'updated_at')::timestamptz END AS updated_at,
			CASE WHEN tombstone.thought_id IS NULL THEN 'active' ELSE 'tombstone' END AS lifecycle_status,
			tombstone.reason AS lifecycle_reason,
			CASE WHEN tombstone.thought_id IS NULL THEN '' ELSE '?tool=history&thought_view=history&thought_id=' || thought.id END AS history_deep_link,
			authority.snapshot_sequence,
			authority.snapshot_at
		FROM reader_thoughts thought
		JOIN search_candidates candidate ON candidate.thought_id=thought.id
		CROSS JOIN search_authority authority
		LEFT JOIN reader_thought_tombstones tombstone ON tombstone.thought_id=thought.id AND (
			CASE WHEN (tombstone.snapshot->>'lifecycle_sequence') ~ '^[0-9]+$'
				THEN (tombstone.snapshot->>'lifecycle_sequence')::bigint <= authority.snapshot_sequence
				ELSE tombstone.created_at <= authority.snapshot_at
			END
		)
		WHERE thought.created_at <= authority.snapshot_at
		-- 排序键也必须落在快照里。分页按 updated_at DESC 做 keyset，而活跃 thought
		-- 的 updated_at 会被编辑顶高（每次写入都 NOW()）；只钉 created_at 的话，
		-- 翻页途中被编辑的那条 updated_at 会大于游标而被 keyset 条件排除，
		-- 结果它在任何一页都不出现，total 却仍然把它算在内。
		AND (CASE WHEN tombstone.thought_id IS NULL THEN thought.updated_at ELSE (tombstone.snapshot->>'updated_at')::timestamptz END) <= authority.snapshot_at
		AND thought.deleted=false AND tombstone.reason IS DISTINCT FROM 'user_deleted' AND (
			tombstone.thought_id IS NULL OR (
				jsonb_typeof(tombstone.snapshot)='object'
				AND tombstone.snapshot ?& ARRAY['snapshot_version','id','host_kind','host_id','link_id','type','body','target','quote','source','created_at','updated_at','original_host_snapshot','original_host_identity','frozen_at']
				AND jsonb_typeof(tombstone.snapshot->'snapshot_version')='number'
				AND tombstone.snapshot->>'snapshot_version'='1'
				AND tombstone.snapshot->>'id'=thought.id
				AND NULLIF(btrim(tombstone.snapshot->>'host_kind'),'') IS NOT NULL
				AND NULLIF(btrim(tombstone.snapshot->>'host_id'),'') IS NOT NULL
				AND tombstone.snapshot->>'type'='thought'
				AND jsonb_typeof(tombstone.snapshot->'link_id') IN ('string','null')
				AND jsonb_typeof(tombstone.snapshot->'body')='string'
				AND jsonb_typeof(tombstone.snapshot->'target')='object'
				AND jsonb_typeof(tombstone.snapshot->'quote') IN ('object','null')
				AND jsonb_typeof(tombstone.snapshot->'source')='string'
				AND jsonb_typeof(tombstone.snapshot->'created_at')='string'
				AND jsonb_typeof(tombstone.snapshot->'updated_at')='string'
				AND jsonb_typeof(tombstone.snapshot->'original_host_identity')='object'
				AND jsonb_typeof(tombstone.snapshot->'frozen_at')='string'
			)
		) AND (
			(tombstone.thought_id IS NULL AND (
				thought.body ILIKE $1 OR thought.source ILIKE $1 OR thought.quote::text ILIKE $1
			)) OR (tombstone.thought_id IS NOT NULL AND (
				(tombstone.snapshot->>'body') ILIKE $1
				OR (tombstone.snapshot->>'source') ILIKE $1
				OR ((tombstone.snapshot->'quote')::text) ILIKE $1
			)))
	)
	SELECT thought_id, host_kind, host_id, link_id, snippet, total_count, updated_at,
		lifecycle_status, lifecycle_reason, history_deep_link, snapshot_sequence, snapshot_at
	FROM matching_thoughts`
	var snapshotSequenceArg any
	var snapshotAtArg any
	if !snapshotAt.IsZero() {
		snapshotSequenceArg = snapshotSequence
		snapshotAtArg = snapshotAt
	}
	args := []any{"%" + query + "%", snapshotSequenceArg, snapshotAtArg}
	if !at.IsZero() {
		sql += ` WHERE (updated_at < $4 OR (updated_at = $4 AND thought_id < $5))`
		args = append(args, at, id)
	}
	sql += fmt.Sprintf(` ORDER BY updated_at DESC, thought_id DESC LIMIT $%d`, len(args)+1)
	// One extra row distinguishes a full final page from a page with another
	// cursor, while the public response still respects the requested limit.
	args = append(args, limit+1)
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, "", fmt.Errorf("search thoughts: %w", err)
	}
	defer rows.Close()
	return scanThoughtSearchRows(rows, limit, query, cursorTotal)
}

func parseThoughtSyncCursor(raw string) (int64, string, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, "", fmt.Errorf("%w: invalid thought sync cursor", ErrInvalidReaderCursor)
	}
	parts := strings.SplitN(string(decoded), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return 0, "", fmt.Errorf("%w: invalid thought sync cursor", ErrInvalidReaderCursor)
	}
	sequence, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || sequence < 0 {
		return 0, "", fmt.Errorf("%w: invalid thought sync cursor", ErrInvalidReaderCursor)
	}
	return sequence, parts[1], nil
}

func thoughtSyncCursor(sequence int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(sequence, 10) + "|" + id))
}

// ListThoughtsSince is the installation-level replay surface for local-first
// clients. It includes tombstones and orders by the server-owned operation
// sequence, so a client never mistakes its IndexedDB auto-increment sequence
// for the authoritative cross-device ordering.
func (r *PGXReaderVNextRepository) ListThoughtsSince(ctx context.Context, after string, limit int) ([]model.ReaderThought, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	sequence, id, err := parseThoughtSyncCursor(after)
	if err != nil {
		return nil, "", err
	}
	sql := `SELECT ` + readerThoughtColumns + `,tt.snapshot,tt.reason,tt.created_at
		FROM reader_thoughts
		LEFT JOIN reader_thought_tombstones tt
			ON tt.thought_id=reader_thoughts.id
		WHERE true`
	args := []any{}
	if sequence > 0 {
		sql += fmt.Sprintf(` AND (last_sequence > $%d OR (last_sequence = $%d AND id > $%d))`, len(args)+1, len(args)+1, len(args)+2)
		args = append(args, sequence, id)
	}
	if sequence == 0 && id != "" {
		return nil, "", fmt.Errorf("%w: invalid thought sync cursor", ErrInvalidReaderCursor)
	}
	limitPosition := len(args) + 1
	sql += fmt.Sprintf(` ORDER BY last_sequence ASC, id ASC LIMIT $%d`, limitPosition)
	args = append(args, limit)
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list thoughts since: %w", err)
	}
	defer rows.Close()
	items := make([]model.ReaderThought, 0)
	for rows.Next() {
		item, err := scanReaderThoughtSyncSnapshot(rows)
		if err != nil {
			return nil, "", fmt.Errorf("decode thought sync snapshot: %w", err)
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) > 0 {
		last := items[len(items)-1]
		return items, thoughtSyncCursor(last.LastSequence, last.ID), nil
	}
	return items, "", nil
}

// ListThoughtConflicts keeps the compatibility endpoint name, but pages the
// independent immutable supersession event sequence.  It must not derive rows
// from the mutable materialized winner: doing so loses late losers and rewrites
// the winner that was observed when an earlier version lost.
func (r *PGXReaderVNextRepository) ListThoughtConflicts(ctx context.Context, after string, limit int) ([]model.ReaderThoughtConflict, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	sequence, marker, err := parseThoughtSyncCursor(after)
	if err != nil {
		return nil, "", err
	}
	if marker != "" && marker != "event" {
		return nil, "", fmt.Errorf("%w: invalid thought conflict cursor", ErrInvalidReaderCursor)
	}
	sql := `SELECT sequence,annotation_id,loser,winner_at_detection FROM reader_thought_supersession_events WHERE true`
	args := []any{}
	if sequence > 0 {
		sql += ` AND sequence > $1`
		args = append(args, sequence)
	}
	limitPosition := len(args) + 1
	sql += fmt.Sprintf(` ORDER BY sequence ASC LIMIT $%d`, limitPosition)
	args = append(args, limit)
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list thought conflicts: %w", err)
	}
	defer rows.Close()
	items := make([]model.ReaderThoughtConflict, 0)
	for rows.Next() {
		var item model.ReaderThoughtConflict
		var loser, winner []byte
		if err := rows.Scan(&item.Sequence, &item.AnnotationID, &loser, &winner); err != nil {
			return nil, "", fmt.Errorf("scan thought conflict: %w", err)
		}
		if err := json.Unmarshal(loser, &item.Loser); err != nil {
			return nil, "", fmt.Errorf("decode thought conflict loser: %w", err)
		}
		if err := json.Unmarshal(winner, &item.Winner); err != nil {
			return nil, "", fmt.Errorf("decode thought conflict winner: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("read thought conflicts: %w", err)
	}
	if len(items) == 0 {
		return items, "", nil
	}
	last := items[len(items)-1]
	return items, thoughtSyncCursor(last.Sequence, "event"), nil
}

func (r *PGXReaderVNextRepository) ListThoughtHistory(ctx context.Context, after string, limit int) ([]model.ReaderThought, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	at, id, err := parseReaderCursor(after)
	if err != nil {
		return nil, "", err
	}
	args := []any{}
	sql := `SELECT ` + readerThoughtColumns + `,tt.snapshot,tt.reason,tt.created_at
		FROM reader_thought_tombstones tt
		JOIN reader_thoughts ON reader_thoughts.id=tt.thought_id
		WHERE reader_thoughts.deleted=false AND tt.reason <> 'user_deleted'`
	if !at.IsZero() {
		sql += fmt.Sprintf(` AND (tt.created_at < $%d OR (tt.created_at = $%d AND tt.thought_id < $%d))`, len(args)+1, len(args)+1, len(args)+2)
		args = append(args, at, id)
	}
	sql += fmt.Sprintf(` ORDER BY tt.created_at DESC,tt.thought_id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list thought history: %w", err)
	}
	defer rows.Close()
	items := make([]model.ReaderThought, 0)
	for rows.Next() {
		item, err := scanReaderThoughtHistorySnapshot(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan thought history: %w", err)
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) == limit {
		last := items[len(items)-1]
		at := last.TombstonedAt
		if at == nil {
			return items, "", nil
		}
		return items, readerCursor(*at, last.ID), nil
	}
	return items, "", nil
}

func (r *PGXReaderVNextRepository) MarkThoughtHostTombstones(ctx context.Context, hostKind, hostID, reason string) error {
	if strings.TrimSpace(hostKind) == "" || strings.TrimSpace(hostID) == "" {
		return ErrReaderTodoHostMissing
	}
	return r.withTx(ctx, func(db database.Querier) error {
		return r.markThoughtHostTombstonesOn(ctx, db, hostKind, hostID, reason)
	})
}

func (r *PGXReaderVNextRepository) markThoughtHostTombstonesOn(ctx context.Context, db database.Querier, hostKind, hostID, reason string) error {
	rows, err := db.Query(ctx, `
		SELECT id
		FROM reader_thoughts
		WHERE host_kind=$1 AND host_id=$2 AND deleted=false
		ORDER BY id
		FOR UPDATE`, hostKind, hostID)
	if err != nil {
		return fmt.Errorf("list thought tombstones: %w", err)
	}
	thoughtIDs := make([]string, 0)
	for rows.Next() {
		var thoughtID string
		if err := rows.Scan(&thoughtID); err != nil {
			rows.Close()
			return fmt.Errorf("scan thought tombstone: %w", err)
		}
		thoughtIDs = append(thoughtIDs, thoughtID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read thought tombstones: %w", err)
	}
	for _, thoughtID := range thoughtIDs {
		if err := r.markThoughtTombstoneOn(ctx, db, thoughtID, reason); err != nil {
			return err
		}
	}
	return nil
}

func (r *PGXReaderVNextRepository) markThoughtTombstoneOn(ctx context.Context, db database.Querier, thoughtID, reason string) error {
	item, err := scanReaderThought(db.QueryRow(ctx, `SELECT `+readerThoughtColumns+`
		FROM reader_thoughts
		WHERE id=$1 AND deleted=false
		FOR UPDATE`, thoughtID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read thought for tombstone: %w", err)
	}
	var currentReason string
	err = db.QueryRow(ctx, `SELECT reason FROM reader_thought_tombstones WHERE thought_id=$1`, thoughtID).Scan(&currentReason)
	if err == nil && currentReason == reason {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read thought tombstone: %w", err)
	}
	lifecycleSequence, err := r.appendThoughtLifecycleOp(ctx, db, item, "tombstone", reason)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `
		INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason,snapshot)
		SELECT id,host_kind,host_id,$2,
			jsonb_build_object(
				'snapshot_version',1,
				'id',id,'host_kind',host_kind,'host_id',host_id,'link_id',link_id,
				'type','thought','body',body,'target',target,'quote',quote,'source',source,
				'created_at',created_at,'updated_at',updated_at,
				'original_host_snapshot',CASE host_kind
					WHEN 'link' THEN (SELECT to_jsonb(COALESCE(link.content_document,link.content,'')) FROM links link WHERE link.id=reader_thoughts.link_id)
					WHEN 'note' THEN (SELECT to_jsonb(note.published_content) FROM reader_notes note WHERE note.id=reader_thoughts.host_id::uuid)
					WHEN 'inbox' THEN (SELECT to_jsonb(inbox.body) FROM reader_inbox inbox WHERE inbox.id=reader_thoughts.host_id::uuid)
					ELSE NULL
				END,
				'original_host_identity',jsonb_build_object('kind',host_kind,'id',host_id,'link_id',link_id),
				'frozen_at',CURRENT_TIMESTAMP,
				'lifecycle_sequence',$3::bigint)
		FROM reader_thoughts
		WHERE id=$1 AND deleted=false
		ON CONFLICT (thought_id) DO NOTHING`,
		thoughtID, reason, lifecycleSequence)
	if err != nil {
		return fmt.Errorf("mark thought tombstone: %w", err)
	}
	// The lifecycle op above materializes the Thought, which refreshes the
	// projection while the Thought is still a live source. The tombstone is
	// what actually retires it, so the projection is refreshed again here.
	return r.replaceThoughtTodoProjectionsOn(ctx, db, thoughtID)
}

func readerThoughtLifecycleOpID(action, reason string, item *model.ReaderThought) string {
	seed := fmt.Sprintf("reader-thought-lifecycle:%s:%s:%s:%d", action, reason, item.ID, item.LastSequence)
	return "lifecycle-" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(seed)).String()
}

func (r *PGXReaderVNextRepository) appendThoughtLifecycleOp(ctx context.Context, db database.Querier, item *model.ReaderThought, action, reason string) (int64, error) {
	linkID := ""
	if item.LinkID != nil {
		linkID = item.LinkID.String()
	}
	payload, err := json.Marshal(struct {
		Body   string          `json:"body"`
		Quote  json.RawMessage `json:"quote,omitempty"`
		Source string          `json:"source"`
		LinkID string          `json:"link_id,omitempty"`
	}{Body: item.Body, Quote: item.Quote, Source: item.Source, LinkID: linkID})
	if err != nil {
		return 0, fmt.Errorf("encode thought lifecycle operation: %w", err)
	}
	thoughtOp, sequence, duplicate, err := r.appendDerivedThoughtOp(ctx, db, model.ReaderThoughtOp{
		OpID:          readerThoughtLifecycleOpID(action, reason, item),
		DeviceID:      "reader-lifecycle",
		OperationKind: "update",
		AnnotationID:  item.ID,
		HostKind:      item.HostKind,
		HostID:        item.HostID,
		Target:        item.Target,
		Payload:       payload,
	})
	if err != nil {
		return 0, fmt.Errorf("append thought lifecycle operation: %w", err)
	}
	if !duplicate {
		if err := r.materializeThought(ctx, db, thoughtOp, sequence); err != nil {
			return 0, err
		}
	}
	return sequence, nil
}

// readerReattachHost resolves and locks the target before the Thought row and
// either caller CAS are checked. Its body is read only after both CAS values
// match, immediately before quote reanchoring.
func readerReattachHost(ctx context.Context, db database.Querier, command model.ReaderThoughtReattachCommand) (int64, error) {
	hostRevision, err := readerReattachHostRevision(ctx, db, command.TargetHostKind, command.TargetHostID)
	if err != nil {
		return 0, err
	}
	return hostRevision, nil
}

// readerReattachThoughtPayload rebuilds the target and op payload for the new
// host. The quote is re-anchored against the destination body rather than
// carried over verbatim, so a reattached thought always points at text that
// actually exists on the host it now belongs to.
func readerReattachThoughtPayload(item *model.ReaderThought, command model.ReaderThoughtReattachCommand, hostRevision int64, hostBody string) (json.RawMessage, json.RawMessage, error) {
	return readerReattachThoughtPayloadWithMarker(item, command, hostRevision, hostBody, nil)
}

func readerReattachThoughtPayloadWithMarker(item *model.ReaderThought, command model.ReaderThoughtReattachCommand, hostRevision int64, hostBody string, marker *model.ReaderThoughtReattachOperation) (json.RawMessage, json.RawMessage, error) {
	linkID := ""
	if command.TargetHostKind == "link" {
		linkID = command.TargetHostID
	}
	target, err := rewriteReaderThoughtTargetHost(item.Target, command.TargetHostID, command.TargetHostKind, hostRevision)
	if err != nil {
		return nil, nil, err
	}
	quote, _, err := readerReanchorQuoteForContent(hostBody, item.Quote)
	if err != nil {
		return nil, nil, err
	}
	payload, err := json.Marshal(struct {
		Body     string                       `json:"body"`
		Quote    json.RawMessage              `json:"quote,omitempty"`
		Source   string                       `json:"source"`
		LinkID   string                       `json:"link_id,omitempty"`
		Reattach *readerThoughtReattachMarker `json:"reattach,omitempty"`
	}{
		Body: item.Body, Quote: quote, Source: item.Source, LinkID: linkID,
		Reattach: func() *readerThoughtReattachMarker {
			if marker == nil {
				return nil
			}
			return &readerThoughtReattachMarker{ExpectedLastSequence: marker.ExpectedLastSequence, ExpectedHostRevision: marker.ExpectedHostRevision}
		}(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("encode thought reattach: %w", err)
	}
	return target, payload, nil
}

func rewriteReaderThoughtTargetHost(raw json.RawMessage, hostID, hostKind string, hostRevision int64) (json.RawMessage, error) {
	var target map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &target) != nil || target == nil {
		return nil, ErrInvalidReaderThought
	}
	encodedHostID, err := json.Marshal(hostID)
	if err != nil {
		return nil, fmt.Errorf("encode thought target host: %w", err)
	}
	target["host_id"] = encodedHostID
	if hostRevision <= 0 {
		return nil, ErrRevisionConflict
	}
	var version map[string]json.RawMessage
	if rawVersion, ok := target["version"]; ok && len(rawVersion) > 0 {
		if err := json.Unmarshal(rawVersion, &version); err != nil || version == nil {
			return nil, ErrInvalidReaderThought
		}
	} else {
		version = make(map[string]json.RawMessage)
	}
	encodedRevision, err := json.Marshal(hostRevision)
	if err != nil {
		return nil, fmt.Errorf("encode thought target revision: %w", err)
	}
	switch hostKind {
	case "link":
		version["content_revision"] = encodedRevision
	case "note":
		version["note_revision"] = encodedRevision
	case "inbox":
		version["metadata_revision"] = encodedRevision
	default:
		return nil, ErrInvalidReaderThought
	}
	target["version"], err = json.Marshal(version)
	if err != nil {
		return nil, fmt.Errorf("encode thought target version: %w", err)
	}
	return json.Marshal(target)
}

func readerReattachHostBody(ctx context.Context, db database.Querier, hostKind, hostID string) (string, error) {
	var body string
	var err error
	switch hostKind {
	case "link":
		id, parseErr := uuid.Parse(hostID)
		if parseErr != nil {
			return "", ErrNotFound
		}
		err = db.QueryRow(ctx, `SELECT COALESCE(content_document,content,'') FROM links WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&body)
	case "note":
		id, parseErr := uuid.Parse(hostID)
		if parseErr != nil {
			return "", ErrNotFound
		}
		err = db.QueryRow(ctx, `SELECT published_content FROM reader_notes WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&body)
	case "inbox":
		id, parseErr := uuid.Parse(hostID)
		if parseErr != nil {
			return "", ErrNotFound
		}
		err = db.QueryRow(ctx, `SELECT body FROM reader_inbox WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&body)
	default:
		return "", ErrNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read thought reattach host body: %w", err)
	}
	return body, nil
}

func readerReattachHostRevision(ctx context.Context, db database.Querier, hostKind, hostID string) (int64, error) {
	var revision int64
	kind := model.ReaderHostKind(hostKind)
	id, err := uuid.Parse(hostID)
	if err != nil {
		return 0, ErrNotFound
	}
	if err := lockReaderThoughtHost(ctx, db, kind, id, readerThoughtHostUpdate); err != nil {
		if errors.Is(err, errReaderThoughtHostUnavailable) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	var query string
	switch hostKind {
	case "link":
		query = `SELECT content_revision FROM links WHERE id=$1 AND deleted_at IS NULL`
	case "note":
		query = `SELECT published_revision FROM reader_notes WHERE id=$1 AND deleted_at IS NULL`
	case "inbox":
		query = `SELECT metadata_revision FROM reader_inbox WHERE id=$1 AND deleted_at IS NULL`
	default:
		return 0, ErrNotFound
	}
	err = db.QueryRow(ctx, query, id).Scan(&revision)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("read thought reattach host revision: %w", err)
	}
	return revision, nil
}

func (r *PGXReaderVNextRepository) GetThought(ctx context.Context, id string) (*model.ReaderThought, error) {
	item, err := scanReaderThought(r.db.QueryRow(ctx, `SELECT `+readerThoughtColumns+` FROM reader_thoughts WHERE id=$1 AND deleted=false`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get thought: %w", err)
	}
	var reason string
	var tombstonedAt time.Time
	var snapshot []byte
	if err := r.db.QueryRow(ctx, `SELECT snapshot,reason,created_at FROM reader_thought_tombstones WHERE thought_id=$1`, id).Scan(&snapshot, &reason, &tombstonedAt); err == nil {
		if reason == "user_deleted" {
			return nil, ErrNotFound
		}
		if err := applyReaderThoughtReplaySnapshot(item, snapshot); err != nil {
			return nil, fmt.Errorf("get thought lifecycle snapshot: %w", err)
		}
		item.LifecycleStatus = "tombstone"
		item.LifecycleReason = &reason
		item.TombstonedAt = &tombstonedAt
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("get thought lifecycle: %w", err)
	}
	return item, nil
}

// GetAIContext returns only bounded, published link context and a small live
// thought projection from the installed library.
func (r *PGXReaderVNextRepository) GetAIContext(ctx context.Context, linkID uuid.UUID) (*model.ReaderAIContext, error) {
	item := &model.ReaderAIContext{LinkID: linkID, Tags: []string{}, Thoughts: []model.ReaderAIThoughtContext{}}
	if err := r.db.QueryRow(ctx, `
		SELECT left(COALESCE(content,''),12000),left(COALESCE(summary,''),2000),COALESCE(tags,'{}')
		FROM links
		WHERE id=$1 AND deleted_at IS NULL`, linkID).Scan(&item.Content, &item.Summary, &item.Tags); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read AI link context: %w", err)
	}
	rows, err := r.db.Query(ctx, `
		SELECT left(t.body,1000)
		FROM reader_thoughts t
		WHERE t.host_kind='link' AND t.host_id=$1 AND t.deleted=false
			AND NOT EXISTS (
				SELECT 1 FROM reader_thought_tombstones tt
				WHERE tt.thought_id=t.id
			)
		ORDER BY t.updated_at DESC,t.id DESC LIMIT 8`, linkID.String())
	if err != nil {
		return nil, fmt.Errorf("read AI thought context: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var thought model.ReaderAIThoughtContext
		if err := rows.Scan(&thought.Body); err != nil {
			return nil, fmt.Errorf("scan AI thought context: %w", err)
		}
		item.Thoughts = append(item.Thoughts, thought)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read AI thought context rows: %w", err)
	}
	return item, nil
}

// CreateNote runs in a transaction because a note created with published
// content is immediately a TODO source; the note row and its projections must
// become visible together.
func (r *PGXReaderVNextRepository) CreateNote(ctx context.Context, note model.ReaderNote) (*model.ReaderNote, error) {
	var created *model.ReaderNote
	err := r.withTx(ctx, func(db database.Querier) error {
		item, err := scanReaderNote(db.QueryRow(ctx, `
			INSERT INTO reader_notes (title, published_content, published_revision, draft_content, draft_revision)
			VALUES (COALESCE(NULLIF($1,''),'未命名笔记'),$2,CASE WHEN $2 <> '' THEN 1 ELSE 0 END,$3::text,CASE WHEN $3::text IS NOT NULL THEN 1 ELSE 0 END)
			RETURNING `+readerNoteColumns, note.Title, note.PublishedContent, note.DraftContent))
		if err != nil {
			return fmt.Errorf("create note: %w", err)
		}
		created = item
		return r.replaceNoteTodoProjectionsOn(ctx, db, item.ID)
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (r *PGXReaderVNextRepository) ListNotes(ctx context.Context, after string, limit int) ([]model.ReaderNote, int, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	at, id, err := parseReaderCursor(after)
	if err != nil {
		return nil, 0, "", err
	}
	args := []any{}
	sql := `SELECT ` + readerNoteColumns + ` FROM reader_notes WHERE deleted_at IS NULL`
	if !at.IsZero() {
		parsedID, parseErr := uuid.Parse(id)
		if parseErr != nil {
			return nil, 0, "", fmt.Errorf("%w: invalid note cursor", ErrInvalidReaderCursor)
		}
		sql += fmt.Sprintf(` AND (updated_at < $%d OR (updated_at = $%d AND id < $%d))`, len(args)+1, len(args)+1, len(args)+2)
		args = append(args, at, parsedID)
	}
	sql += fmt.Sprintf(` ORDER BY updated_at DESC, id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, "", fmt.Errorf("list notes: %w", err)
	}
	defer rows.Close()
	items := make([]model.ReaderNote, 0)
	for rows.Next() {
		item, err := scanReaderNote(rows)
		if err != nil {
			return nil, 0, "", fmt.Errorf("scan note: %w", err)
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, "", err
	}
	var count int
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM reader_notes WHERE deleted_at IS NULL`).Scan(&count); err != nil {
		return nil, 0, "", fmt.Errorf("count notes: %w", err)
	}
	if len(items) == limit {
		last := items[len(items)-1]
		return items, count, readerCursor(last.UpdatedAt, last.ID.String()), nil
	}
	return items, count, "", nil
}

func (r *PGXReaderVNextRepository) SearchPublishedNotes(ctx context.Context, query string, limit int) ([]model.ReaderNoteSearch, int, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []model.ReaderNoteSearch{}, 0, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, title,
			left(regexp_replace(published_content, '\s+', ' ', 'g'), 240),
			published_revision, count(*) OVER (), updated_at
		FROM reader_notes
		WHERE deleted_at IS NULL AND published_revision > 0
			AND (title ILIKE $1 OR published_content ILIKE $1)
		ORDER BY updated_at DESC, id DESC
		LIMIT $2`, "%"+query+"%", limit)
	if err != nil {
		return nil, 0, fmt.Errorf("search published notes: %w", err)
	}
	defer rows.Close()
	items := make([]model.ReaderNoteSearch, 0)
	total := 0
	for rows.Next() {
		var item model.ReaderNoteSearch
		var totalRows int64
		if err := rows.Scan(&item.ID, &item.Title, &item.Snippet, &item.PublishedRevision, &totalRows, &item.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan note search result: %w", err)
		}
		itemTotal := int(totalRows)
		if itemTotal > total {
			total = itemTotal
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (r *PGXReaderVNextRepository) GetNote(ctx context.Context, id uuid.UUID) (*model.ReaderNote, error) {
	item, err := scanReaderNote(r.db.QueryRow(ctx, `SELECT `+readerNoteColumns+` FROM reader_notes WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get note: %w", err)
	}
	return item, nil
}

func (r *PGXReaderVNextRepository) SaveNoteDraft(ctx context.Context, command model.ReaderNoteDraftCommand) (*model.ReaderNote, error) {
	// The data-modifying CTE and the live-row probe share one PostgreSQL
	// statement snapshot. A separate existence query after a zero-row UPDATE
	// could observe a concurrent delete and misreport a missing note as stale.
	var (
		outcome                                         string
		id                                              pgtype.UUID
		title, publishedContent, draftContent           pgtype.Text
		publishedRevision, draftRevision                pgtype.Int8
		draftUpdatedAt, deletedAt, createdAt, updatedAt pgtype.Timestamptz
	)
	err := r.db.QueryRow(ctx, `
		WITH updated AS (
			UPDATE reader_notes
			SET draft_content=$1, draft_revision=draft_revision+1, draft_updated_at=NOW(), updated_at=NOW()
			WHERE id=$2 AND deleted_at IS NULL AND draft_revision=$3
			RETURNING `+readerNoteColumns+`
		), live AS (
			SELECT 1 FROM reader_notes WHERE id=$2 AND deleted_at IS NULL
		)
		SELECT 'updated', `+readerNoteColumns+` FROM updated
		UNION ALL
		SELECT CASE WHEN EXISTS (SELECT 1 FROM live) THEN 'stale' ELSE 'missing' END,
			NULL::uuid, NULL::text, NULL::text, NULL::bigint, NULL::text, NULL::bigint,
			NULL::timestamptz, NULL::timestamptz, NULL::timestamptz, NULL::timestamptz
		WHERE NOT EXISTS (SELECT 1 FROM updated)`, command.Content, command.NoteID, command.ExpectedDraftRevision).
		Scan(&outcome, &id, &title, &publishedContent, &publishedRevision, &draftContent, &draftRevision, &draftUpdatedAt, &deletedAt, &createdAt, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("save note draft: %w", err)
	}
	if outcome == "missing" {
		return nil, ErrNotFound
	}
	if outcome == "stale" {
		return nil, ErrRevisionConflict
	}
	if outcome != "updated" || !id.Valid || !title.Valid || !publishedContent.Valid || !publishedRevision.Valid || !draftRevision.Valid || !createdAt.Valid || !updatedAt.Valid {
		return nil, fmt.Errorf("save note draft: invalid outcome %q", outcome)
	}
	var draftContentValue *string
	if draftContent.Valid {
		draftContentValue = &draftContent.String
	}
	var draftUpdatedAtValue, deletedAtValue *time.Time
	if draftUpdatedAt.Valid {
		draftUpdatedAtValue = &draftUpdatedAt.Time
	}
	if deletedAt.Valid {
		deletedAtValue = &deletedAt.Time
	}
	return &model.ReaderNote{
		ID:                id.Bytes,
		Title:             title.String,
		PublishedContent:  publishedContent.String,
		PublishedRevision: publishedRevision.Int64,
		DraftContent:      draftContentValue,
		DraftRevision:     draftRevision.Int64,
		DraftUpdatedAt:    draftUpdatedAtValue,
		DeletedAt:         deletedAtValue,
		CreatedAt:         createdAt.Time,
		UpdatedAt:         updatedAt.Time,
	}, nil
}

func (r *PGXReaderVNextRepository) DiscardNoteDraft(ctx context.Context, command model.ReaderNoteDiscardDraftCommand) error {
	result, err := r.db.Exec(ctx, `
		UPDATE reader_notes
		SET draft_content=NULL, draft_updated_at=NULL, draft_revision=draft_revision+1, updated_at=NOW()
		WHERE id=$1 AND deleted_at IS NULL AND draft_revision=$2
			AND draft_content IS NOT NULL`, command.NoteID, command.ExpectedDraftRevision)
	if err != nil {
		return fmt.Errorf("discard note draft: %w", err)
	}
	if result.RowsAffected() == 0 {
		// A note with no draft is already in the requested state. Keep this
		// idempotent, but distinguish a stale revision from a missing note.
		var exists bool
		if err := r.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reader_notes WHERE id=$1 AND deleted_at IS NULL)`, command.NoteID).Scan(&exists); err != nil {
			return fmt.Errorf("check note draft discard: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		var revision int64
		var hasDraft bool
		if err := r.db.QueryRow(ctx, `SELECT draft_revision, draft_content IS NOT NULL FROM reader_notes WHERE id=$1 AND deleted_at IS NULL`, command.NoteID).Scan(&revision, &hasDraft); err != nil {
			return fmt.Errorf("read note draft discard state: %w", err)
		}
		if revision != command.ExpectedDraftRevision || hasDraft {
			return ErrRevisionConflict
		}
	}
	return nil
}

func (r *PGXReaderVNextRepository) PublishNote(ctx context.Context, command model.ReaderNotePublishCommand) (*model.ReaderNote, error) {
	var out *model.ReaderNote
	err := r.withTx(ctx, func(db database.Querier) error {
		var current model.ReaderNote
		row := db.QueryRow(ctx, `SELECT `+readerNoteColumns+` FROM reader_notes WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, command.NoteID)
		item, err := scanReaderNote(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		current = *item
		if current.DraftRevision != command.ExpectedDraftRevision || current.PublishedRevision != command.ExpectedPublishedRevision {
			return ErrRevisionConflict
		}
		content := current.PublishedContent
		if current.DraftContent != nil {
			content = *current.DraftContent
		}
		if readerNoteContentCanonicalEmpty(content) {
			return ErrReaderNoteContentEmpty
		}
		// A retry after a completed publish does not need a reanchor batch and
		// must not perturb revision, history, operations, or timestamps.
		if content == current.PublishedContent {
			out = &current
			return nil
		}
		reanchorOps, err := readerReanchorOpsJSON(command.ReanchorOps)
		if err != nil {
			return err
		}
		if err := r.validateNoteReanchorSet(ctx, db, command.NoteID, command.ReanchorOps); err != nil {
			return err
		}
		newRevision := current.PublishedRevision + 1
		title := notetitle.Derive(content)
		if _, err := db.Exec(ctx, `
			UPDATE reader_notes SET title=$1,published_content=$2, published_revision=$3,
				draft_content=NULL, draft_revision=draft_revision+1, draft_updated_at=NULL, updated_at=NOW()
			WHERE id=$4`, title, content, newRevision, command.NoteID); err != nil {
			return fmt.Errorf("publish note: %w", err)
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO reader_note_history (note_id, revision, title, content, reanchor_ops)
			VALUES ($1,$2,$3,$4,$5::jsonb)
			ON CONFLICT (note_id,revision) DO NOTHING`, command.NoteID, newRevision, title, content, reanchorOps); err != nil {
			return fmt.Errorf("record note history: %w", err)
		}
		if err := r.applyNoteReanchorOps(ctx, db, command.NoteID, current.PublishedRevision, newRevision, content, command.ReanchorOps); err != nil {
			return err
		}
		if err := r.replaceNoteTodoProjectionsOn(ctx, db, command.NoteID); err != nil {
			return err
		}
		out, err = scanReaderNote(db.QueryRow(ctx, `SELECT `+readerNoteColumns+` FROM reader_notes WHERE id=$1`, command.NoteID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readerNoteContentCanonicalEmpty deliberately matches the Reader's wire
// contract. Unicode whitespace is Markdown content, not an empty draft.
func readerNoteContentCanonicalEmpty(content string) bool {
	return strings.Trim(content, " \t\r\n") == ""
}

// validateNoteReanchorSet runs while PublishNote/RestoreNoteRevision hold the
// note row FOR UPDATE. AppendThoughtOps acquires that same host row FOR SHARE
// before it can materialize a live note thought, so a writer either commits
// before this query and is included, or is ordered after the transition. The
// row locks below keep each included active thought stable while it is checked.
func (r *PGXReaderVNextRepository) validateNoteReanchorSet(ctx context.Context, db database.Querier, noteID uuid.UUID, rawOps []json.RawMessage) error {
	rows, err := db.Query(ctx, `SELECT reader_thoughts.id
		FROM reader_thoughts
		WHERE reader_thoughts.host_kind='note'
			AND reader_thoughts.host_id=$1
			AND reader_thoughts.deleted=false
			AND NOT EXISTS (
				SELECT 1 FROM reader_thought_tombstones tombstone
				WHERE tombstone.thought_id=reader_thoughts.id
			)
		ORDER BY reader_thoughts.id
		FOR UPDATE`, noteID.String())
	if err != nil {
		return fmt.Errorf("list active note thoughts: %w", err)
	}
	defer rows.Close()
	active := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan active note thought: %w", err)
		}
		active[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate active note thoughts: %w", err)
	}
	provided := make(map[string]struct{}, len(rawOps))
	for _, raw := range rawOps {
		op, err := decodeReaderNoteReanchorOp(raw)
		if err != nil {
			return err
		}
		if _, exists := provided[op.ThoughtID]; exists {
			return ErrReaderNoteReanchorIncomplete
		}
		provided[op.ThoughtID] = struct{}{}
	}
	if len(active) != len(provided) {
		return ErrReaderNoteReanchorIncomplete
	}
	for id := range active {
		if _, exists := provided[id]; !exists {
			return ErrReaderNoteReanchorIncomplete
		}
	}
	return nil
}

type readerNoteReanchorOp struct {
	ThoughtID string `json:"thought_id"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	Target    struct {
		Kind    string `json:"kind"`
		HostID  string `json:"host_id"`
		Version struct {
			NoteRevision int64 `json:"note_revision"`
		} `json:"version"`
	} `json:"target"`
	Quote map[string]any `json:"quote"`
	Range struct {
		Start int `json:"start"`
		End   int `json:"end"`
	} `json:"range"`
}

func decodeReaderNoteReanchorOp(raw json.RawMessage) (readerNoteReanchorOp, error) {
	var op readerNoteReanchorOp
	if err := json.Unmarshal(raw, &op); err != nil || strings.TrimSpace(op.ThoughtID) == "" {
		return readerNoteReanchorOp{}, ErrInvalidReaderReanchor
	}
	if op.Status != "reanchored" && op.Status != "historical" {
		return readerNoteReanchorOp{}, ErrInvalidReaderReanchor
	}
	if op.Status == "reanchored" {
		if err := validateReaderNoteReanchorAnchor(op); err != nil {
			return readerNoteReanchorOp{}, err
		}
	}
	return op, nil
}

// validateReaderNoteReanchorAnchor guards the fields that only a successfully
// reanchored op is allowed to carry: a recognised reason, a note target pinned
// to a concrete revision, and a quote whose range is a usable slice. Historical
// ops skip this because they carry no anchor at all.
func validateReaderNoteReanchorAnchor(op readerNoteReanchorOp) error {
	if op.Reason != "diff-context" && op.Reason != "unique-quote" {
		return ErrInvalidReaderReanchor
	}
	if op.Target.Kind != "note" || strings.TrimSpace(op.Target.HostID) == "" || op.Target.Version.NoteRevision <= 0 {
		return ErrInvalidReaderReanchor
	}
	if op.Quote == nil {
		return ErrInvalidReaderReanchor
	}
	exact, ok := op.Quote["exact"].(string)
	if !ok || exact == "" || op.Range.Start < 0 || op.Range.End <= op.Range.Start || op.Range.End > 1<<24 {
		return ErrInvalidReaderReanchor
	}
	return nil
}

// readerUTF16Slice mirrors JavaScript String#slice offsets. Reanchor ranges
// are produced by the Reader client, whose offsets count UTF-16 code units;
// slicing Go bytes here would accept a range that points at different text for
// non-BMP characters.
func readerUTF16Slice(value string, start, end int) (string, bool) {
	if start < 0 || end <= start {
		return "", false
	}
	offset := 0
	byteStart, byteEnd := -1, -1
	for byteOffset, r := range value {
		if offset == start {
			byteStart = byteOffset
		}
		width := utf16.RuneLen(r)
		if width < 0 {
			width = 1
		}
		nextOffset := offset + width
		byteEndOffset := byteOffset + len(string(r))
		if nextOffset == start {
			byteStart = byteEndOffset
		}
		if nextOffset == end {
			byteEnd = byteEndOffset
			break
		}
		offset = nextOffset
	}
	if byteStart < 0 && offset == start {
		byteStart = len(value)
	}
	if byteEnd < 0 {
		if offset == end {
			byteEnd = len(value)
		} else if byteStart >= 0 {
			// The requested end fell in the middle of a UTF-16 code point or
			// outside the string; neither is a valid client range.
			return "", false
		}
	}
	if byteStart < 0 || byteEnd < byteStart {
		return "", false
	}
	return value[byteStart:byteEnd], true
}

type readerReanchorMatch struct {
	start  int
	end    int
	reason string
}

func readerUTF16Occurrences(source, needle string) []int {
	sourceUnits := utf16.Encode([]rune(source))
	needleUnits := utf16.Encode([]rune(needle))
	if len(needleUnits) == 0 || len(needleUnits) > len(sourceUnits) {
		return nil
	}
	positions := make([]int, 0, 1)
	for start := 0; start <= len(sourceUnits)-len(needleUnits); start++ {
		matched := true
		for offset, unit := range needleUnits {
			if sourceUnits[start+offset] != unit {
				matched = false
				break
			}
		}
		if matched {
			positions = append(positions, start)
		}
	}
	return positions
}

func readerUTF16UnitsEqual(left, right []uint16) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func readerReanchorContextMatches(sourceUnits []uint16, start, end int, prefix, suffix string) bool {
	prefixUnits := utf16.Encode([]rune(prefix))
	if len(prefixUnits) > 0 {
		if start < len(prefixUnits) || !readerUTF16UnitsEqual(sourceUnits[start-len(prefixUnits):start], prefixUnits) {
			return false
		}
	}
	suffixUnits := utf16.Encode([]rune(suffix))
	if len(suffixUnits) > 0 {
		if end+len(suffixUnits) > len(sourceUnits) || !readerUTF16UnitsEqual(sourceUnits[end:end+len(suffixUnits)], suffixUnits) {
			return false
		}
	}
	return true
}

func readerReanchorDiffContextCandidates(sourceUnits []uint16, prefix, suffix string) []readerReanchorMatch {
	if prefix == "" || suffix == "" {
		return nil
	}
	candidates := make([]readerReanchorMatch, 0, 1)
	for _, prefixStart := range readerUTF16Occurrences(string(utf16.Decode(sourceUnits)), prefix) {
		start := prefixStart + len(utf16.Encode([]rune(prefix)))
		for _, suffixStart := range readerUTF16Occurrences(string(utf16.Decode(sourceUnits)), suffix) {
			if suffixStart <= start {
				continue
			}
			candidates = append(candidates, readerReanchorMatch{start: start, end: suffixStart, reason: "diff-context"})
		}
	}
	return candidates
}

func readerReanchorMatchForQuote(content, exact, prefix, suffix string) (readerReanchorMatch, bool) {
	sourceUnits := utf16.Encode([]rune(content))
	exactPositions := readerUTF16Occurrences(content, exact)
	if len(exactPositions) == 0 {
		candidates := readerReanchorDiffContextCandidates(sourceUnits, prefix, suffix)
		if len(candidates) == 1 {
			return candidates[0], true
		}
		if len(candidates) > 1 {
			return readerReanchorMatch{reason: "ambiguous-quote"}, false
		}
		return readerReanchorMatch{reason: "missing-quote"}, false
	}
	if prefix == "" && suffix == "" {
		if len(exactPositions) == 1 {
			start := exactPositions[0]
			return readerReanchorMatch{start: start, end: start + len(utf16.Encode([]rune(exact))), reason: "unique-quote"}, true
		}
		return readerReanchorMatch{reason: "ambiguous-quote"}, false
	}
	contextual := make([]readerReanchorMatch, 0, len(exactPositions))
	exactLength := len(utf16.Encode([]rune(exact)))
	for _, start := range exactPositions {
		end := start + exactLength
		if readerReanchorContextMatches(sourceUnits, start, end, prefix, suffix) {
			contextual = append(contextual, readerReanchorMatch{start: start, end: end, reason: "unique-quote"})
		}
	}
	if len(contextual) == 1 {
		return contextual[0], true
	}
	if len(contextual) > 1 || len(exactPositions) > 1 {
		return readerReanchorMatch{reason: "ambiguous-quote"}, false
	}
	return readerReanchorMatch{reason: "missing-quote"}, false
}

// decodeReaderReanchorQuote unpacks the stored selector. The quote map itself
// is returned so the caller can rewrite it in place, which keeps any selector
// field the Reader client added but this code does not understand.
func decodeReaderReanchorQuote(raw json.RawMessage) (map[string]json.RawMessage, string, string, string, error) {
	var quote map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &quote) != nil || quote == nil {
		return nil, "", "", "", ErrInvalidReaderReanchor
	}
	var exact, prefix, suffix string
	if value, ok := quote["exact"]; !ok || json.Unmarshal(value, &exact) != nil || exact == "" {
		return nil, "", "", "", ErrInvalidReaderReanchor
	}
	if value, ok := quote["prefix"]; ok && json.Unmarshal(value, &prefix) != nil {
		return nil, "", "", "", ErrInvalidReaderReanchor
	}
	if value, ok := quote["suffix"]; ok && json.Unmarshal(value, &suffix) != nil {
		return nil, "", "", "", ErrInvalidReaderReanchor
	}
	return quote, exact, prefix, suffix, nil
}

// readerReanchorRewriteContext replaces the selector context with the 32 UTF-16
// units surrounding the match in the current content. Carrying the old context
// forward would leave a selector that no longer disambiguates the new text.
func readerReanchorRewriteContext(quote map[string]json.RawMessage, content string, contentUnits []uint16, match readerReanchorMatch) {
	quote["prefix"] = json.RawMessage(`""`)
	quote["suffix"] = json.RawMessage(`""`)
	if match.start > 0 {
		contextStart := match.start - 32
		if contextStart < 0 {
			contextStart = 0
		}
		if value, valid := readerUTF16Slice(content, contextStart, match.start); valid {
			quote["prefix"], _ = json.Marshal(value)
		}
	}
	if match.end < len(contentUnits) {
		contextEnd := match.end + 32
		if contextEnd > len(contentUnits) {
			contextEnd = len(contentUnits)
		}
		if value, valid := readerUTF16Slice(content, match.end, contextEnd); valid {
			quote["suffix"], _ = json.Marshal(value)
		}
	}
}

func readerReanchorQuoteForContent(content string, raw json.RawMessage) (json.RawMessage, string, error) {
	quote, exact, prefix, suffix, err := decodeReaderReanchorQuote(raw)
	if err != nil {
		return nil, "missing-quote", err
	}
	match, ok := readerReanchorMatchForQuote(content, exact, prefix, suffix)
	if !ok {
		if match.reason == "" {
			match.reason = "missing-quote"
		}
		return nil, match.reason, ErrInvalidReaderReanchor
	}
	contentUnits := utf16.Encode([]rune(content))
	matchedExact, valid := readerUTF16Slice(content, match.start, match.end)
	if !valid {
		return nil, "missing-quote", ErrInvalidReaderReanchor
	}
	quote["exact"], _ = json.Marshal(matchedExact)
	readerReanchorRewriteContext(quote, content, contentUnits, match)
	quote["start"], _ = json.Marshal(match.start)
	quote["end"], _ = json.Marshal(match.end)
	encoded, err := json.Marshal(quote)
	if err != nil {
		return nil, match.reason, err
	}
	return encoded, match.reason, nil
}

func (r *PGXReaderVNextRepository) applyNoteReanchorOps(
	ctx context.Context,
	db database.Querier,
	noteID uuid.UUID,
	previousRevision int64,
	nextRevision int64,
	content string,
	rawOps []json.RawMessage,
) error {
	seen := make(map[string]struct{}, len(rawOps))
	for _, raw := range rawOps {
		op, err := decodeReaderNoteReanchorOp(raw)
		if err != nil {
			return err
		}
		if _, ok := seen[op.ThoughtID]; ok {
			return ErrInvalidReaderReanchor
		}
		seen[op.ThoughtID] = struct{}{}
		if err := r.applyNoteReanchorOp(ctx, db, noteID, previousRevision, nextRevision, content, op); err != nil {
			return err
		}
	}
	return nil
}

// applyNoteReanchorOp moves one thought onto the new note revision, or
// tombstones it when the publisher reported it could not be reanchored.
func (r *PGXReaderVNextRepository) applyNoteReanchorOp(
	ctx context.Context,
	db database.Querier,
	noteID uuid.UUID,
	previousRevision int64,
	nextRevision int64,
	content string,
	op readerNoteReanchorOp,
) error {
	item, err := scanReaderThought(db.QueryRow(ctx, `SELECT `+readerThoughtColumns+` FROM reader_thoughts WHERE id=$1 AND deleted=false FOR UPDATE`, op.ThoughtID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidReaderReanchor
	}
	if err != nil {
		return fmt.Errorf("read note reanchor thought: %w", err)
	}
	if item.HostKind != "note" || item.HostID != noteID.String() {
		return ErrInvalidReaderReanchor
	}
	if op.Status == "historical" {
		reason := strings.TrimSpace(op.Reason)
		if reason == "" {
			reason = "not-reanchored"
		}
		return r.markThoughtTombstoneOn(ctx, db, op.ThoughtID, "note_reanchor_"+reason)
	}
	if err := validateNoteReanchorOp(item, op, noteID, previousRevision, nextRevision, content); err != nil {
		return err
	}
	return r.commitNoteReanchor(ctx, db, noteID, nextRevision, item, op)
}

// validateNoteReanchorOp proves the op moves the thought off exactly the
// previous revision onto the new one, and that the new range still quotes the
// same text. Anything else would silently re-point a thought at other prose.
func validateNoteReanchorOp(item *model.ReaderThought, op readerNoteReanchorOp, noteID uuid.UUID, previousRevision, nextRevision int64, content string) error {
	var previousTarget struct {
		Kind    string `json:"kind"`
		Version struct {
			NoteRevision int64 `json:"note_revision"`
		} `json:"version"`
	}
	if err := json.Unmarshal(item.Target, &previousTarget); err != nil || previousTarget.Kind != "note" || previousTarget.Version.NoteRevision != previousRevision {
		return ErrInvalidReaderReanchor
	}
	if op.Target.HostID != noteID.String() || op.Target.Version.NoteRevision != nextRevision {
		return ErrInvalidReaderReanchor
	}
	exact, _ := op.Quote["exact"].(string)
	rangedText, ok := readerUTF16Slice(content, op.Range.Start, op.Range.End)
	if !ok || rangedText != exact {
		return ErrInvalidReaderReanchor
	}
	return nil
}

// commitNoteReanchor writes the reanchored thought: a new op, the refreshed
// materialised row, and the tombstone cleared.
func (r *PGXReaderVNextRepository) commitNoteReanchor(
	ctx context.Context,
	db database.Querier,
	noteID uuid.UUID,
	nextRevision int64,
	item *model.ReaderThought,
	op readerNoteReanchorOp,
) error {
	quote := make(map[string]any, len(op.Quote)+2)
	for key, value := range op.Quote {
		quote[key] = value
	}
	quote["start"] = op.Range.Start
	quote["end"] = op.Range.End
	target, err := json.Marshal(op.Target)
	if err != nil {
		return fmt.Errorf("encode note reanchor target: %w", err)
	}
	quoteRaw, err := json.Marshal(quote)
	if err != nil {
		return fmt.Errorf("encode note reanchor quote: %w", err)
	}
	linkID := ""
	if item.LinkID != nil {
		linkID = item.LinkID.String()
	}
	payload, err := json.Marshal(struct {
		Body   string          `json:"body"`
		Quote  json.RawMessage `json:"quote"`
		Source string          `json:"source"`
		LinkID string          `json:"link_id,omitempty"`
	}{Body: item.Body, Quote: quoteRaw, Source: item.Source, LinkID: linkID})
	if err != nil {
		return fmt.Errorf("encode note reanchor payload: %w", err)
	}
	thoughtOp, sequence, duplicate, err := r.appendDerivedThoughtOp(ctx, db, model.ReaderThoughtOp{
		OpID:          "note-reanchor-" + noteID.String() + "-" + strconv.FormatInt(nextRevision, 10) + "-" + op.ThoughtID,
		DeviceID:      "reader-note-publish",
		OperationKind: "update",
		AnnotationID:  op.ThoughtID,
		HostKind:      "note",
		HostID:        noteID.String(),
		Target:        target,
		Payload:       payload,
	})
	if err != nil {
		return fmt.Errorf("append note reanchor op: %w", err)
	}
	if !duplicate {
		if err := r.materializeThought(ctx, db, thoughtOp, sequence); err != nil {
			return err
		}
	}
	if _, err := db.Exec(ctx, `DELETE FROM reader_thought_tombstones WHERE thought_id=$1`, op.ThoughtID); err != nil {
		return fmt.Errorf("clear note reanchor tombstone: %w", err)
	}
	// Clearing the tombstone brings the Thought back as a TODO source, which
	// the materialization above could not yet see.
	return r.replaceThoughtTodoProjectionsOn(ctx, db, op.ThoughtID)
}

func readerReanchorOpsJSON(ops []json.RawMessage) ([]byte, error) {
	if len(ops) > 500 {
		return nil, ErrInvalidReaderReanchor
	}
	if len(ops) == 0 {
		return []byte(`[]`), nil
	}
	encoded := make([]json.RawMessage, 0, len(ops))
	for _, raw := range ops {
		if len(raw) == 0 || len(raw) > 128*1024 || !json.Valid(raw) {
			return nil, ErrInvalidReaderReanchor
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return nil, ErrInvalidReaderReanchor
		}
		if _, err := decodeReaderNoteReanchorOp(raw); err != nil {
			return nil, err
		}
		encoded = append(encoded, append(json.RawMessage(nil), raw...))
	}
	return json.Marshal(encoded)
}

func (r *PGXReaderVNextRepository) ListNoteHistory(ctx context.Context, noteID uuid.UUID, limit int) ([]model.ReaderNoteHistory, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `SELECT id,note_id,revision,title,content,reanchor_ops,created_at FROM reader_note_history WHERE note_id=$1 ORDER BY revision DESC LIMIT $2`, noteID, limit)
	if err != nil {
		return nil, fmt.Errorf("list note history: %w", err)
	}
	defer rows.Close()
	out := make([]model.ReaderNoteHistory, 0)
	for rows.Next() {
		var item model.ReaderNoteHistory
		if err := rows.Scan(&item.ID, &item.NoteID, &item.Revision, &item.Title, &item.Content, &item.ReanchorOps, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *PGXReaderVNextRepository) RestoreNoteRevision(ctx context.Context, command model.ReaderNoteRestoreCommand) (*model.ReaderNote, error) {
	var out *model.ReaderNote
	err := r.withTx(ctx, func(db database.Querier) error {
		var err error
		out, err = r.restoreNoteRevisionOn(ctx, db, command)
		return err
	})
	return out, err
}

func (r *PGXReaderVNextRepository) restoreNoteRevisionOn(ctx context.Context, db database.Querier, command model.ReaderNoteRestoreCommand) (*model.ReaderNote, error) {
	current, err := lockNoteForRevisionRestore(ctx, db, command)
	if err != nil {
		return nil, err
	}
	content, err := noteHistoryContentForRestore(ctx, db, command)
	if err != nil {
		return nil, err
	}
	title := notetitle.Derive(content)
	reanchorOps, err := readerReanchorOpsJSON(command.ReanchorOps)
	if err != nil {
		return nil, err
	}
	if err := r.validateNoteReanchorSet(ctx, db, command.NoteID, command.ReanchorOps); err != nil {
		return nil, err
	}
	updated, err := updateRestoredNote(ctx, db, command.NoteID, current, title, content)
	if err != nil {
		return nil, err
	}
	newRevision := updated.PublishedRevision
	if _, err := db.Exec(ctx, `INSERT INTO reader_note_history (note_id,revision,title,content,reanchor_ops) VALUES ($1,$2,$3,$4,$5::jsonb) ON CONFLICT (note_id,revision) DO NOTHING`, command.NoteID, newRevision, title, content, reanchorOps); err != nil {
		return updated, fmt.Errorf("record restored note history: %w", err)
	}
	if err := r.applyNoteReanchorOps(ctx, db, command.NoteID, current.PublishedRevision, newRevision, content, command.ReanchorOps); err != nil {
		return updated, err
	}
	if err := r.replaceNoteTodoProjectionsOn(ctx, db, command.NoteID); err != nil {
		return updated, err
	}
	return updated, nil
}

func lockNoteForRevisionRestore(ctx context.Context, db database.Querier, command model.ReaderNoteRestoreCommand) (*model.ReaderNote, error) {
	if command.Revision <= 0 {
		return nil, ErrNotFound
	}
	current, err := scanReaderNote(db.QueryRow(ctx, `SELECT `+readerNoteColumns+` FROM reader_notes WHERE id=$1 FOR UPDATE`, command.NoteID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock note for revision restore: %w", err)
	}
	if current.DeletedAt != nil {
		return nil, ErrNotFound
	}
	if current.DraftRevision != command.ExpectedDraftRevision || current.PublishedRevision != command.ExpectedPublishedRevision {
		return nil, ErrRevisionConflict
	}
	if current.DraftContent != nil && !readerNoteContentCanonicalEmpty(*current.DraftContent) && *current.DraftContent != current.PublishedContent {
		return nil, ErrReaderNoteDraftDirty
	}
	return current, nil
}

func noteHistoryContentForRestore(ctx context.Context, db database.Querier, command model.ReaderNoteRestoreCommand) (string, error) {
	var content string
	err := db.QueryRow(ctx, `SELECT content FROM reader_note_history WHERE note_id=$1 AND revision=$2`, command.NoteID, command.Revision).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return content, err
}

func updateRestoredNote(ctx context.Context, db database.Querier, noteID uuid.UUID, current *model.ReaderNote, title, content string) (*model.ReaderNote, error) {
	updated, err := scanReaderNote(db.QueryRow(ctx, `UPDATE reader_notes
		SET title=$1,published_content=$2,published_revision=$3,
			draft_content=NULL,draft_revision=$4,draft_updated_at=NULL,updated_at=NOW()
		WHERE id=$5 AND deleted_at IS NULL
			AND published_revision=$6 AND draft_revision=$7
		RETURNING `+readerNoteColumns,
		title, content, current.PublishedRevision+1, current.DraftRevision+1, noteID, current.PublishedRevision, current.DraftRevision))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRevisionConflict
	}
	if err != nil {
		return nil, fmt.Errorf("restore note revision: %w", err)
	}
	return updated, nil
}

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

func (r *PGXReaderVNextRepository) CreateTodo(ctx context.Context, todo model.ReaderTodo) (*model.ReaderTodo, error) {
	created, err := scanReaderTodo(r.db.QueryRow(ctx, `
		INSERT INTO reader_todos (text,due_at,done,origin_kind,origin_host_kind,origin_host_id,origin_ref,completed_at)
		VALUES ($1,$2,$3,'standalone',NULL,NULL,$4::jsonb,$5)
		RETURNING `+readerTodoColumns, todo.Text, todo.DueAt, todo.Done, rawJSON(todo.OriginRef), todo.CompletedAt))
	if err != nil {
		return nil, fmt.Errorf("create todo: %w", err)
	}
	return created, nil
}

func (r *PGXReaderVNextRepository) upsertTodoProjection(ctx context.Context, db database.Querier, todo model.ReaderTodo) (*model.ReaderTodo, error) {
	var existingID uuid.UUID
	lookupErr := db.QueryRow(ctx, `
		SELECT id FROM reader_todos
		WHERE origin_kind=$1 AND origin_host_id=$2
			AND origin_ref->>'block_ref'=$3
			AND COALESCE(origin_ref->>'occurrence','1')=$4 AND deleted_at IS NULL
		FOR UPDATE`, todo.OriginKind, valueOrEmpty(todo.OriginHostID), originBlockRef(todo.OriginRef), originBlockOccurrence(todo.OriginRef)).Scan(&existingID)
	if lookupErr == nil {
		// The host is authoritative for a projected checkbox. A completion
		// command updates the host and projection in one transaction, so replaying
		// the projection cannot reopen a checked item or overwrite a standalone
		// TODO, while an edit made on another device is still reflected here.
		return scanReaderTodo(db.QueryRow(ctx, `
			UPDATE reader_todos SET text=$1,origin_host_kind=$2,origin_ref=$3::jsonb,host_revision=$4,
				done=$5,completed_at=CASE WHEN $5 THEN COALESCE(completed_at,NOW()) ELSE NULL END,updated_at=NOW()
			WHERE id=$6
			RETURNING `+readerTodoColumns, todo.Text, todo.OriginHostKind, rawJSON(todo.OriginRef), todo.HostRevision, todo.Done, existingID))
	}
	if !errors.Is(lookupErr, pgx.ErrNoRows) {
		return nil, lookupErr
	}
	// FOR UPDATE 锁不住还不存在的行：两个并发 host 写请求会同时走到这里各插一次，
	// 输家会拿到 23505。
	// ON CONFLICT 必须完整复述 idx_reader_todos_projection 的部分索引谓词才能被
	// 推断，DO UPDATE 与上面的 UPDATE 分支保持同样的"host 权威"语义。
	return scanReaderTodo(db.QueryRow(ctx, `
		INSERT INTO reader_todos (text,due_at,done,origin_kind,origin_host_kind,origin_host_id,origin_ref,host_revision,completed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,CASE WHEN $3 THEN COALESCE($9::timestamptz,NOW()) ELSE NULL END)
			ON CONFLICT (origin_kind,origin_host_id,(origin_ref->>'block_ref'),COALESCE(origin_ref->>'occurrence','1'))
				WHERE origin_kind <> 'standalone' AND deleted_at IS NULL
			DO UPDATE SET text=EXCLUDED.text,origin_host_kind=EXCLUDED.origin_host_kind,
				origin_ref=EXCLUDED.origin_ref,host_revision=EXCLUDED.host_revision,done=EXCLUDED.done,
				completed_at=CASE WHEN EXCLUDED.done THEN COALESCE(reader_todos.completed_at,NOW()) ELSE NULL END,
				updated_at=NOW()
			RETURNING `+readerTodoColumns, todo.Text, todo.DueAt, todo.Done, todo.OriginKind, todo.OriginHostKind, todo.OriginHostID, rawJSON(todo.OriginRef), todo.HostRevision, todo.CompletedAt))
}

// refreshTodoProjections writes back every projection the authoritative host
// still emits, except those whose key is already tombstoned — a dismissed
// projection must not be resurrected by the next refresh.
func (r *PGXReaderVNextRepository) refreshTodoProjections(ctx context.Context, db database.Querier, todos []model.ReaderTodo, deleted map[string]struct{}) error {
	for _, todo := range todos {
		if todo.OriginKind == "standalone" {
			continue
		}
		if _, ok := deleted[readerTodoProjectionKey(todo.OriginKind, valueOrEmpty(todo.OriginHostID), todo.OriginRef)]; ok {
			continue
		}
		if _, err := r.upsertTodoProjection(ctx, db, todo); err != nil {
			return fmt.Errorf("upsert todo projection: %w", err)
		}
	}
	return nil
}

// dismissStaleTodoProjections soft-deletes the still-active projections whose
// key the host no longer emits. Rows that are already deleted are left alone so
// their tombstone keeps its original timestamp.
func dismissStaleTodoProjections(ctx context.Context, db database.Querier, existing []readerExistingTodoProjection, desired map[string]struct{}) error {
	for _, item := range existing {
		if item.deletedAt != nil {
			continue
		}
		if _, ok := desired[readerTodoProjectionKey(item.origin, valueOrEmpty(item.hostID), item.originRef)]; ok {
			continue
		}
		if _, err := db.Exec(ctx, `UPDATE reader_todos SET deleted_at=COALESCE(deleted_at,NOW()),updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`, item.id); err != nil {
			return fmt.Errorf("dismiss stale todo projection: %w", err)
		}
	}
	return nil
}

type readerExistingTodoProjection struct {
	id        uuid.UUID
	origin    string
	hostID    *string
	originRef []byte
	deletedAt *time.Time
}

func scanReaderExistingTodoProjections(rows pgx.Rows) ([]readerExistingTodoProjection, error) {
	defer rows.Close()
	existing := make([]readerExistingTodoProjection, 0, 32)
	for rows.Next() {
		var item readerExistingTodoProjection
		var deletedAt pgtype.Timestamptz
		if err := rows.Scan(&item.id, &item.origin, &item.hostID, &item.originRef, &deletedAt); err != nil {
			return nil, fmt.Errorf("scan existing todo projection: %w", err)
		}
		if deletedAt.Valid {
			value := deletedAt.Time
			item.deletedAt = &value
		}
		existing = append(existing, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read existing todo projections: %w", err)
	}
	return existing, nil
}

func readerTodoProjectionKey(originKind, hostID string, originRef json.RawMessage) string {
	return originKind + "\x00" + hostID + "\x00" + originBlockRef(originRef) + "\x00" + originBlockOccurrence(originRef)
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func originBlockRef(raw json.RawMessage) string {
	var value struct {
		BlockRef string `json:"block_ref"`
	}
	_ = json.Unmarshal(raw, &value)
	return value.BlockRef
}

func originBlockOccurrence(raw json.RawMessage) string {
	var value struct {
		Occurrence int `json:"occurrence"`
	}
	_ = json.Unmarshal(raw, &value)
	if value.Occurrence <= 0 {
		return "1"
	}
	return strconv.Itoa(value.Occurrence)
}

func (r *PGXReaderVNextRepository) ListTodos(ctx context.Context, after string, limit int) (model.ReaderTodoPage, error) {
	if limit <= 0 || limit > 200 {
		return model.ReaderTodoPage{}, fmt.Errorf("%w: invalid TODO page limit", ErrInvalidReaderCursor)
	}
	cursor, err := parseReaderTodoCursor(after)
	if err != nil {
		return model.ReaderTodoPage{}, err
	}
	rows, err := r.db.Query(ctx, `SELECT `+readerTodoColumns+` FROM reader_todos
		WHERE deleted_at IS NULL
		AND ($1::boolean IS FALSE OR done::integer > $2 OR (done::integer = $2 AND (
			(due_at IS NULL)::integer > $3 OR ((due_at IS NULL)::integer = $3 AND (
				($3 = 0 AND due_at > $4) OR
				(($3 = 1 OR due_at = $4) AND (created_at < $5 OR (created_at = $5 AND id < $6)))
			))
		)))
		ORDER BY done ASC, due_at ASC NULLS LAST, created_at DESC, id DESC LIMIT $7`,
		// 开关必须取自解析后的游标，不能取自原始串：parseReaderTodoCursor 把纯空白
		// 视为"无游标"并返回零值 + nil error，若用 after != "" 判断，`?after=%20`
		// 会带着零值游标启用 keyset 谓词，静默过滤掉全部未完成 TODO。
		cursor.ID != uuid.Nil, boolInt(cursor.Done), boolInt(cursor.DueAt == nil), cursor.DueAt, cursor.CreatedAt, cursor.ID, limit+1)
	if err != nil {
		return model.ReaderTodoPage{}, fmt.Errorf("list todos: %w", err)
	}
	defer rows.Close()
	out := make([]model.ReaderTodo, 0, limit+1)
	for rows.Next() {
		item, err := scanReaderTodo(rows)
		if err != nil {
			return model.ReaderTodoPage{}, err
		}
		out = append(out, *item)
	}
	if err := rows.Err(); err != nil {
		return model.ReaderTodoPage{}, err
	}
	page := model.ReaderTodoPage{Items: out}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = readerTodoCursor(page.Items[len(page.Items)-1])
	}
	return page, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (r *PGXReaderVNextRepository) PatchTodo(ctx context.Context, patch model.ReaderTodoPatch) (*model.ReaderTodo, error) {
	var out *model.ReaderTodo
	err := r.withTx(ctx, func(db database.Querier) error {
		item, err := r.patchTodoOn(ctx, db, patch)
		if err != nil {
			return err
		}
		out = item
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r *PGXReaderVNextRepository) patchTodoOn(ctx context.Context, db database.Querier, patch model.ReaderTodoPatch) (*model.ReaderTodo, error) {
	var originKind string
	var hostRevision int64
	if err := db.QueryRow(ctx, `SELECT origin_kind,host_revision FROM reader_todos WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, patch.ID).Scan(&originKind, &hostRevision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read todo revision: %w", err)
	}

	if originKind == "standalone" {
		if patch.ExpectedHostRevisionSet || patch.ExpectedHostRevision != nil {
			return nil, ErrReaderTodoHostRevisionNotApplicable
		}
		return patchStandaloneTodo(ctx, db, patch)
	}
	if patch.ExpectedHostRevision == nil || *patch.ExpectedHostRevision != hostRevision {
		return nil, ErrRevisionConflict
	}
	if patch.Text != nil || patch.DueAtSet || patch.DueAt != nil || patch.Done == nil {
		return nil, ErrReaderTodoProjectionImmutable
	}
	return r.patchProjectedTodo(ctx, db, patch, hostRevision)
}

func patchStandaloneTodo(ctx context.Context, db database.Querier, patch model.ReaderTodoPatch) (*model.ReaderTodo, error) {
	dueAtSet := patch.DueAtSet || patch.DueAt != nil
	item, err := scanReaderTodo(db.QueryRow(ctx, `
		UPDATE reader_todos SET
			text=COALESCE($1,text), due_at=CASE WHEN $2 THEN $3 ELSE due_at END,
			done=COALESCE($4,done),
			completed_at=CASE WHEN COALESCE($4,done) THEN COALESCE(completed_at,NOW()) ELSE NULL END,
			updated_at=NOW()
			WHERE id=$5 AND deleted_at IS NULL
			RETURNING `+readerTodoColumns, patch.Text, dueAtSet, patch.DueAt, patch.Done, patch.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("patch todo: %w", err)
	}
	return item, nil
}

func (r *PGXReaderVNextRepository) patchProjectedTodo(ctx context.Context, db database.Querier, patch model.ReaderTodoPatch, hostRevision int64) (*model.ReaderTodo, error) {
	var originKind, originHostKind, originHostID string
	var originRef []byte
	if err := db.QueryRow(ctx, `
		SELECT origin_kind,origin_host_kind,origin_host_id,origin_ref
		FROM reader_todos
		WHERE id=$1 AND deleted_at IS NULL
		FOR UPDATE`, patch.ID).Scan(&originKind, &originHostKind, &originHostID, &originRef); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read projected todo host: %w", err)
	}
	if originKind == "standalone" || originHostKind == "" || originHostID == "" {
		return nil, ErrReaderTodoHostMissing
	}
	blockRef := originBlockRef(originRef)
	if blockRef == "" {
		return nil, ErrReaderTodoAnchorNotFound
	}
	occurrence, err := strconv.Atoi(originBlockOccurrence(originRef))
	if err != nil || occurrence <= 0 {
		occurrence = 1
	}
	done := *patch.Done

	switch originKind {
	case "thought":
		return r.patchThoughtTodo(ctx, db, patch.ID, originHostKind, originHostID, blockRef, occurrence, hostRevision, done)
	case "note":
		return r.patchNoteTodo(ctx, db, patch.ID, originHostID, blockRef, occurrence, hostRevision, done)
	default:
		return nil, ErrReaderTodoHostMissing
	}
}

func readerTodoThoughtWritebackOpID(todoID uuid.UUID, hostRevision int64, blockRef string, occurrence int, done bool) string {
	seed := fmt.Sprintf("reader-todo-writeback:%s:%d:%s:%d:%t", todoID, hostRevision, blockRef, occurrence, done)
	return "todo-" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(seed)).String()
}

func (r *PGXReaderVNextRepository) patchThoughtTodo(ctx context.Context, db database.Querier, todoID uuid.UUID, hostKind, hostID, blockRef string, occurrence int, hostRevision int64, done bool) (*model.ReaderTodo, error) {
	var body, source string
	var target, quote []byte
	var linkID *uuid.UUID
	var currentSequence int64
	if err := db.QueryRow(ctx, `
		SELECT body,target,quote,source,link_id,last_sequence
		FROM reader_thoughts
		WHERE id=$1 AND deleted=false
		FOR UPDATE`, hostID).Scan(&body, &target, &quote, &source, &linkID, &currentSequence); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrReaderTodoHostMissing
		}
		return nil, fmt.Errorf("read thought todo host: %w", err)
	}
	if currentSequence != hostRevision {
		return nil, ErrRevisionConflict
	}
	updated := readertext.Update(body, blockRef, occurrence, done)
	switch updated.Status {
	case readertext.NotFound:
		return nil, ErrReaderTodoAnchorNotFound
	case readertext.Ambiguous:
		return nil, ErrReaderTodoAnchorAmbiguous
	}
	if updated.Source == body {
		return r.updateProjectedTodo(ctx, db, todoID, updated.Block.Text, done, hostRevision)
	}
	linkIDValue := ""
	if linkID != nil {
		linkIDValue = linkID.String()
	}
	payload, err := json.Marshal(struct {
		Body   string          `json:"body"`
		Quote  json.RawMessage `json:"quote,omitempty"`
		Source string          `json:"source"`
		LinkID string          `json:"link_id,omitempty"`
	}{Body: updated.Source, Quote: quote, Source: source, LinkID: linkIDValue})
	if err != nil {
		return nil, fmt.Errorf("encode thought todo writeback: %w", err)
	}
	thoughtOp, sequence, duplicate, err := r.appendDerivedThoughtOp(ctx, db, model.ReaderThoughtOp{
		OpID:          readerTodoThoughtWritebackOpID(todoID, hostRevision, blockRef, occurrence, done),
		DeviceID:      "reader-todo",
		OperationKind: "update",
		AnnotationID:  hostID,
		HostKind:      hostKind,
		HostID:        hostID,
		Target:        target,
		Payload:       payload,
	})
	if err != nil {
		return nil, fmt.Errorf("append thought todo writeback: %w", err)
	}
	if !duplicate {
		if err := r.materializeThought(ctx, db, thoughtOp, sequence); err != nil {
			return nil, err
		}
	}
	return r.updateProjectedTodo(ctx, db, todoID, updated.Block.Text, done, sequence)
}

func (r *PGXReaderVNextRepository) patchNoteTodo(ctx context.Context, db database.Querier, todoID uuid.UUID, hostID, blockRef string, occurrence int, hostRevision int64, done bool) (*model.ReaderTodo, error) {
	noteID, err := uuid.Parse(hostID)
	if err != nil {
		return nil, ErrReaderTodoHostMissing
	}
	var title, content string
	var draftContent pgtype.Text
	var currentRevision int64
	if err := db.QueryRow(ctx, `
		SELECT title,published_content,published_revision,draft_content
		FROM reader_notes
		WHERE id=$1 AND deleted_at IS NULL
		FOR UPDATE`, noteID).Scan(&title, &content, &currentRevision, &draftContent); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrReaderTodoHostMissing
		}
		return nil, fmt.Errorf("read note todo host: %w", err)
	}
	if currentRevision != hostRevision {
		return nil, ErrRevisionConflict
	}
	if draftContent.Valid && draftContent.String != content {
		return nil, ErrRevisionConflict
	}
	updated := readertext.Update(content, blockRef, occurrence, done)
	switch updated.Status {
	case readertext.NotFound:
		return nil, ErrReaderTodoAnchorNotFound
	case readertext.Ambiguous:
		return nil, ErrReaderTodoAnchorAmbiguous
	}
	if updated.Source == content {
		return r.updateProjectedTodo(ctx, db, todoID, updated.Block.Text, done, hostRevision)
	}
	newRevision := currentRevision + 1
	result, err := db.Exec(ctx, `
		UPDATE reader_notes
		SET published_content=$1,published_revision=$2,draft_content=NULL,
			draft_revision=draft_revision+1,draft_updated_at=NULL,updated_at=NOW()
		WHERE id=$3 AND published_revision=$4 AND deleted_at IS NULL`, updated.Source, newRevision, noteID, currentRevision)
	if err != nil {
		return nil, fmt.Errorf("write note todo host: %w", err)
	}
	if result.RowsAffected() == 0 {
		return nil, ErrRevisionConflict
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO reader_note_history (note_id,revision,title,content,reanchor_ops)
		VALUES ($1,$2,$3,$4,'[]'::jsonb)
		ON CONFLICT (note_id,revision) DO NOTHING`, noteID, newRevision, title, updated.Source); err != nil {
		return nil, fmt.Errorf("record note todo history: %w", err)
	}
	if err := r.markThoughtHostTombstonesOn(ctx, db, "note", noteID.String(), "note_todo_updated"); err != nil {
		return nil, err
	}
	if err := r.replaceNoteTodoProjectionsOn(ctx, db, noteID); err != nil {
		return nil, err
	}
	return r.updateProjectedTodo(ctx, db, todoID, updated.Block.Text, done, newRevision)
}

func (r *PGXReaderVNextRepository) updateProjectedTodo(ctx context.Context, db database.Querier, todoID uuid.UUID, text string, done bool, hostRevision int64) (*model.ReaderTodo, error) {
	item, err := scanReaderTodo(db.QueryRow(ctx, `
		UPDATE reader_todos SET
			text=$1,done=$2,host_revision=$3,
			completed_at=CASE WHEN $2 THEN COALESCE(completed_at,NOW()) ELSE NULL END,
			updated_at=NOW()
		WHERE id=$4 AND deleted_at IS NULL
		RETURNING `+readerTodoColumns, text, done, hostRevision, todoID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update projected todo: %w", err)
	}
	return item, nil
}

func (r *PGXReaderVNextRepository) DeleteTodo(ctx context.Context, id uuid.UUID) error {
	return r.withTx(ctx, func(db database.Querier) error {
		var originKind string
		if err := db.QueryRow(ctx, `
			SELECT origin_kind FROM reader_todos
			WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&originKind); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read todo for delete: %w", err)
		}
		if originKind != "standalone" {
			return ErrReaderTodoProjectionImmutable
		}
		result, err := db.Exec(ctx, `
			UPDATE reader_todos SET deleted_at=NOW(),updated_at=NOW()
			WHERE id=$1 AND deleted_at IS NULL`, id)
		if err != nil {
			return fmt.Errorf("delete todo: %w", err)
		}
		if result.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (r *PGXReaderVNextRepository) GetEngagement(ctx context.Context, linkID uuid.UUID) (*model.ReaderEngagement, error) {
	var linkExists bool
	if err := r.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM links WHERE id=$1 AND deleted_at IS NULL)`, linkID).Scan(&linkExists); err != nil {
		return nil, fmt.Errorf("check engagement link: %w", err)
	}
	if !linkExists {
		return nil, ErrNotFound
	}
	item, err := scanReaderEngagement(r.db.QueryRow(ctx, `SELECT link_id,read,progress,read_later,last_opened,updated_at FROM reader_engagement WHERE link_id=$1`, linkID))
	if errors.Is(err, pgx.ErrNoRows) {
		return &model.ReaderEngagement{LinkID: linkID, Progress: 0}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get engagement: %w", err)
	}
	return item, nil
}

func (r *PGXReaderVNextRepository) PatchEngagement(ctx context.Context, patch model.ReaderEngagementPatch) (*model.ReaderEngagement, error) {
	var linkExists bool
	if err := r.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM links WHERE id=$1 AND deleted_at IS NULL)`, patch.LinkID).Scan(&linkExists); err != nil {
		return nil, fmt.Errorf("check engagement link: %w", err)
	}
	if !linkExists {
		return nil, ErrNotFound
	}
	item, err := scanReaderEngagement(r.db.QueryRow(ctx, `
		INSERT INTO reader_engagement (link_id,read,progress,read_later,last_opened)
		VALUES ($1,COALESCE($2::boolean,false),COALESCE($3::real,0),COALESCE($4::boolean,false),CASE WHEN $2::boolean IS TRUE OR $3::real IS NOT NULL THEN NOW() ELSE NULL END)
		ON CONFLICT (link_id) DO UPDATE SET
			read=COALESCE($2::boolean,reader_engagement.read),progress=COALESCE($3::real,reader_engagement.progress),
			read_later=COALESCE($4::boolean,reader_engagement.read_later),
			last_opened=CASE WHEN $2::boolean IS TRUE OR $3::real IS NOT NULL THEN NOW() ELSE reader_engagement.last_opened END,
			updated_at=NOW()
		RETURNING link_id,read,progress,read_later,last_opened,updated_at`, patch.LinkID, patch.Read, patch.Progress, patch.ReadLater))
	if err != nil {
		return nil, fmt.Errorf("patch engagement: %w", err)
	}
	return item, nil
}

func (r *PGXReaderVNextRepository) HomeCounts(ctx context.Context) (map[string]int, error) {
	counts, err := homeCountsOn(ctx, r.db)
	if err != nil {
		return nil, fmt.Errorf("home counts: %w", err)
	}
	return counts, nil
}

type readerFeedCursor struct {
	Mode        string
	Sources     []string
	Score       int
	CreatedAt   time.Time
	EventAt     time.Time
	ResourceKey string
	Key         string
}

type readerFeedCursorWire struct {
	Version     int      `json:"version"`
	Mode        string   `json:"mode"`
	Sources     []string `json:"sources"`
	Score       *int     `json:"score,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
	EventAt     string   `json:"event_at,omitempty"`
	ResourceKey string   `json:"resource_key,omitempty"`
	Key         string   `json:"key"`
}

func normalizeRepositoryFeedSources(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(raw))
	for _, value := range raw {
		for _, part := range strings.Split(value, ",") {
			source := strings.ToLower(strings.TrimSpace(part))
			if source == "" {
				continue
			}
			switch source {
			case "saved":
				source = "reading"
			case "pending":
				source = "inbox"
			case "reading", "inbox", "subscription":
			default:
				return nil, fmt.Errorf("%w: invalid feed source", ErrInvalidReaderCursor)
			}
			seen[source] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	result := make([]string, 0, len(seen))
	for source := range seen {
		result = append(result, source)
	}
	sort.Strings(result)
	return result, nil
}

func sameRepositoryFeedSources(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func hasRepositoryFeedSource(sources []string, source string) bool {
	return len(sources) == 0 || slices.Contains(sources, source)
}

func feedCursor(raw string) (readerFeedCursor, error) {
	if strings.TrimSpace(raw) == "" {
		return readerFeedCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return readerFeedCursor{}, fmt.Errorf("%w: invalid feed cursor", ErrInvalidReaderCursor)
	}
	var wire readerFeedCursorWire
	if err := json.Unmarshal(decoded, &wire); err != nil || wire.Version != 1 {
		return readerFeedCursor{}, fmt.Errorf("%w: invalid feed cursor", ErrInvalidReaderCursor)
	}
	if wire.Mode != "recommended" && wire.Mode != "chronological" {
		return readerFeedCursor{}, fmt.Errorf("%w: invalid feed cursor mode", ErrInvalidReaderCursor)
	}
	sources, err := normalizeRepositoryFeedSources(wire.Sources)
	if err != nil || !sameRepositoryFeedSources(sources, wire.Sources) {
		return readerFeedCursor{}, fmt.Errorf("%w: invalid feed cursor sources", ErrInvalidReaderCursor)
	}
	if strings.TrimSpace(wire.Key) == "" || strings.TrimSpace(wire.Key) != wire.Key {
		return readerFeedCursor{}, fmt.Errorf("%w: invalid feed cursor key", ErrInvalidReaderCursor)
	}
	cursor := readerFeedCursor{Mode: wire.Mode, Sources: sources, Key: wire.Key}
	return feedCursorPosition(wire, cursor)
}

func feedCursorPosition(wire readerFeedCursorWire, cursor readerFeedCursor) (readerFeedCursor, error) {
	if wire.Mode == "recommended" {
		if wire.Score == nil {
			return readerFeedCursor{}, fmt.Errorf("%w: invalid recommended feed cursor", ErrInvalidReaderCursor)
		}
		createdAt, err := time.Parse(time.RFC3339Nano, wire.CreatedAt)
		if err != nil {
			return readerFeedCursor{}, fmt.Errorf("%w: invalid recommended feed cursor", ErrInvalidReaderCursor)
		}
		cursor.Score = *wire.Score
		cursor.CreatedAt = createdAt
		return cursor, nil
	}
	eventAt, err := time.Parse(time.RFC3339Nano, wire.EventAt)
	if err != nil || strings.TrimSpace(wire.ResourceKey) == "" {
		return readerFeedCursor{}, fmt.Errorf("%w: invalid chronological feed cursor", ErrInvalidReaderCursor)
	}
	cursor.EventAt = eventAt
	cursor.ResourceKey = wire.ResourceKey
	return cursor, nil
}

func makeFeedCursor(mode string, sources []string, item model.ReaderFeedItem) string {
	wire := readerFeedCursorWire{
		Version: 1,
		Mode:    mode,
		Sources: append([]string{}, sources...),
		Key:     item.Key,
	}
	if mode == "chronological" {
		wire.EventAt = item.VisibleEventAt().Format(time.RFC3339Nano)
		wire.ResourceKey = item.ResourceIdentity()
	} else {
		score := item.Score
		wire.Score = &score
		wire.CreatedAt = item.CreatedAt.Format(time.RFC3339Nano)
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// sortReaderFeedItems orders the live merged set by the exact tuple encoded in
// its cursor. Key is the final tie-breaker in both modes.
func sortReaderFeedItems(items []model.ReaderFeedItem, mode string) {
	if mode == "chronological" {
		sort.SliceStable(items, func(i, j int) bool {
			leftEventAt, rightEventAt := items[i].VisibleEventAt(), items[j].VisibleEventAt()
			if !leftEventAt.Equal(rightEventAt) {
				return leftEventAt.After(rightEventAt)
			}
			leftResource, rightResource := items[i].ResourceIdentity(), items[j].ResourceIdentity()
			if leftResource != rightResource {
				return leftResource < rightResource
			}
			return items[i].Key < items[j].Key
		})
		return
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].Key < items[j].Key
	})
}

func readerFeedItemAfterCursor(item model.ReaderFeedItem, cursor readerFeedCursor) bool {
	if cursor.Mode == "chronological" {
		eventAt := item.VisibleEventAt()
		if !eventAt.Equal(cursor.EventAt) {
			return eventAt.Before(cursor.EventAt)
		}
		resourceKey := item.ResourceIdentity()
		if resourceKey != cursor.ResourceKey {
			return resourceKey > cursor.ResourceKey
		}
		return item.Key > cursor.Key
	}
	if item.Score != cursor.Score {
		return item.Score < cursor.Score
	}
	if !item.CreatedAt.Equal(cursor.CreatedAt) {
		return item.CreatedAt.Before(cursor.CreatedAt)
	}
	return item.Key > cursor.Key
}

func readerFeedPage(items []model.ReaderFeedItem, mode string, sources []string, cursor readerFeedCursor, limit int) *model.ReaderFeedPage {
	start := 0
	if cursor.Mode != "" {
		start = sort.Search(len(items), func(index int) bool {
			return readerFeedItemAfterCursor(items[index], cursor)
		})
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	next := ""
	if end < len(items) {
		next = makeFeedCursor(mode, sources, items[end-1])
	}
	return &model.ReaderFeedPage{
		Items:      append([]model.ReaderFeedItem(nil), items[start:end]...),
		NextCursor: next,
		Mode:       mode,
	}
}

func (r *PGXReaderVNextRepository) ListFeedWithSources(ctx context.Context, mode, after string, sources []string, limit int) (*model.ReaderFeedPage, error) {
	if mode != "" && mode != "recommended" && mode != "chronological" {
		return nil, fmt.Errorf("%w: invalid feed mode", ErrInvalidReaderCursor)
	}
	mode = modeOrDefault(mode)
	normalizedSources, err := normalizeRepositoryFeedSources(sources)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	cursor, err := feedCursor(after)
	if err != nil {
		return nil, err
	}
	if cursor.Mode != "" && (cursor.Mode != mode || !sameRepositoryFeedSources(cursor.Sources, normalizedSources)) {
		return nil, fmt.Errorf("%w: feed cursor parameters changed", ErrInvalidReaderCursor)
	}
	items, err := r.buildFeedItemsForMode(ctx, mode, normalizedSources)
	if err != nil {
		return nil, err
	}
	items = scoreReaderFeedItems(items)
	sortReaderFeedItems(items, mode)
	return readerFeedPage(items, mode, normalizedSources, cursor, limit), nil
}

func modeOrDefault(mode string) string {
	if mode == "chronological" {
		return "chronological"
	}
	return "recommended"
}

func (r *PGXReaderVNextRepository) buildFeedItemsForMode(ctx context.Context, mode string, sourceFilters ...[]string) ([]model.ReaderFeedItem, error) {
	chronological := mode == "chronological"
	var sources []string
	if len(sourceFilters) > 0 {
		sources = sourceFilters[0]
	}
	items := make([]model.ReaderFeedItem, 0, 120)
	if hasRepositoryFeedSource(sources, "reading") {
		appended, err := r.appendReadingFeedItems(ctx, items, chronological)
		if err != nil {
			return nil, err
		}
		items = appended
	}
	if hasRepositoryFeedSource(sources, "inbox") {
		appended, err := r.appendInboxFeedItems(ctx, items, chronological)
		if err != nil {
			return nil, err
		}
		items = appended
	}
	if hasRepositoryFeedSource(sources, "subscription") {
		appended, err := r.appendSubscriptionFeedItems(ctx, items, chronological)
		if err != nil {
			return nil, err
		}
		items = appended
	}
	// A URL may be present both in the saved library and in RSS. Keep the first
	// source in the merge order, so Reading remains the canonical card.
	seenItems := make(map[string]struct{}, len(items))
	out := make([]model.ReaderFeedItem, 0, len(items))
	for _, item := range items {
		dedupeKey := item.DedupeIdentity()
		if dedupeKey != "" {
			if _, ok := seenItems[dedupeKey]; ok {
				continue
			}
			seenItems[dedupeKey] = struct{}{}
		}
		out = append(out, item)
	}
	return out, nil
}

// appendReadingFeedItems collects the saved-library slice of the feed. Hidden
// rows are filtered in SQL before ranking and dedupe.
func (r *PGXReaderVNextRepository) appendReadingFeedItems(ctx context.Context, items []model.ReaderFeedItem, chronological bool) ([]model.ReaderFeedItem, error) {
	orderBy := "l.created_at DESC,l.id DESC"
	if chronological {
		orderBy = "l.created_at DESC,l.id ASC"
	}
	rows, err := r.db.Query(ctx, `
		SELECT l.id,l.url,COALESCE(l.title,''),COALESCE(l.summary,''),l.created_at,
			COALESCE(e.read,false),COALESCE(e.read_later,false)
		FROM links l LEFT JOIN reader_engagement e ON e.link_id=l.id
		LEFT JOIN reader_feed_hides hidden ON hidden.item_key='link:'||l.id::text
		WHERE l.status='done' AND l.deleted_at IS NULL AND COALESCE(l.library_kind,'reading')='reading' AND hidden.item_key IS NULL
		ORDER BY `+orderBy+` LIMIT 1000`)
	if err != nil {
		return nil, fmt.Errorf("feed links: %w", err)
	}
	for rows.Next() {
		var item model.ReaderFeedItem
		var id uuid.UUID
		if err := rows.Scan(&id, &item.URL, &item.Title, &item.Summary, &item.CreatedAt, &item.Read, &item.ReadLater); err != nil {
			rows.Close()
			return nil, err
		}
		item.Key = "link:" + id.String()
		item.Source = "reading"
		item.LinkID = &id
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// appendInboxFeedItems collects the still-pending captures. Only pending rows
// belong in the feed: confirmed ones already surface through the reading slice
// and discarded ones must stay gone.
func (r *PGXReaderVNextRepository) appendInboxFeedItems(ctx context.Context, items []model.ReaderFeedItem, chronological bool) ([]model.ReaderFeedItem, error) {
	orderBy := "inbox.created_at DESC,inbox.id DESC"
	if chronological {
		orderBy = "inbox.created_at DESC,inbox.id ASC"
	}
	rows, err := r.db.Query(ctx, `
		SELECT inbox.id,inbox.url,COALESCE(inbox.title,''),COALESCE(inbox.summary,''),inbox.created_at
		FROM reader_inbox inbox
		LEFT JOIN reader_feed_hides hidden ON hidden.item_key='inbox:'||inbox.id::text
		WHERE inbox.status='pending' AND inbox.deleted_at IS NULL
			AND (inbox.expires_at IS NULL OR inbox.expires_at > NOW())
			AND hidden.item_key IS NULL
		ORDER BY `+orderBy+` LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("feed inbox: %w", err)
	}
	for rows.Next() {
		var item model.ReaderFeedItem
		var id uuid.UUID
		if err := rows.Scan(&id, &item.URL, &item.Title, &item.Summary, &item.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		item.Key = "inbox:" + id.String()
		item.Source = "inbox"
		item.InboxID = &id
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// appendSubscriptionFeedItems collects the RSS slice. A feed item that was
// already saved carries its link id forward so the later dedupe pass can
// recognise it as the same resource as the saved-library entry.
func (r *PGXReaderVNextRepository) appendSubscriptionFeedItems(ctx context.Context, items []model.ReaderFeedItem, chronological bool) ([]model.ReaderFeedItem, error) {
	orderBy := "COALESCE(fi.published_at,fi.created_at) DESC,fi.id DESC"
	if chronological {
		orderBy = "COALESCE(fi.published_at,fi.created_at) DESC,CASE WHEN COALESCE(fs.link_id,fi.link_id) IS NOT NULL THEN 'link:'||COALESCE(fs.link_id,fi.link_id)::text ELSE 'feed_item:'||fi.id::text END ASC"
	}
	rows, err := r.db.Query(ctx, `
		SELECT fi.id,COALESCE(fs.link_id,fi.link_id),fi.url,COALESCE(fi.title,''),COALESCE(fi.summary,''),fi.published_at,
			(fi.read_at IS NOT NULL),fi.read_later,(fs.feed_item_id IS NOT NULL),fi.created_at
		FROM feed_items fi LEFT JOIN reader_feed_hides hidden ON hidden.item_key='subscription:'||fi.id::text
		LEFT JOIN reader_feed_saves fs ON fs.feed_item_id=fi.id
		WHERE hidden.item_key IS NULL
		ORDER BY `+orderBy+` LIMIT 1000`)
	if err != nil {
		return nil, fmt.Errorf("feed subscription items: %w", err)
	}
	for rows.Next() {
		var item model.ReaderFeedItem
		var id uuid.UUID
		var linkedID pgtype.UUID
		var published pgtype.Timestamptz
		if err := rows.Scan(&id, &linkedID, &item.URL, &item.Title, &item.Summary, &published, &item.Read, &item.ReadLater, &item.Saved, &item.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		item.Key = "subscription:" + id.String()
		item.Source = "subscription"
		item.FeedItemID = &id
		if linkedID.Valid {
			value := uuid.UUID(linkedID.Bytes)
			item.LinkID = &value
		}
		if published.Valid {
			publishedAt := published.Time
			item.PublishedAt = &publishedAt
		}
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (r *PGXReaderVNextRepository) FeedbackFeed(ctx context.Context, itemKey, action string) (model.ReaderFeedFeedback, error) {
	itemKey = strings.TrimSpace(itemKey)
	kind, id, err := parseReaderFeedItemKey(itemKey)
	if err != nil {
		return model.ReaderFeedFeedback{}, err
	}
	if err := validateReaderFeedAction(kind, action); err != nil {
		return model.ReaderFeedFeedback{}, err
	}

	result := model.ReaderFeedFeedback{ItemKey: itemKey, Action: action}
	err = r.withTx(ctx, func(tx database.Querier) error {
		if err := ensureReaderFeedItem(ctx, tx, kind, id); err != nil {
			return err
		}
		if action == "save" && kind == "subscription" {
			linkID, err := r.saveSubscriptionFeedItem(ctx, tx, id)
			if err != nil {
				return err
			}
			result.LinkID = &linkID
		}
		if action == "unsave" && kind == "subscription" {
			linkID, err := r.unsaveSubscriptionFeedItem(ctx, tx, id)
			if err != nil {
				return err
			}
			result.LinkID = linkID
		}
		if action == "hide" {
			if _, err := tx.Exec(ctx, `INSERT INTO reader_feed_hides (item_key) VALUES ($1) ON CONFLICT (item_key) DO UPDATE SET created_at=NOW()`, itemKey); err != nil {
				return fmt.Errorf("hide reader feed item: %w", err)
			}
		} else if _, err := tx.Exec(ctx, `DELETE FROM reader_feed_hides WHERE item_key=$1`, itemKey); err != nil {
			return fmt.Errorf("clear reader feed hide: %w", err)
		}
		return nil
	})
	if err != nil {
		return model.ReaderFeedFeedback{}, err
	}
	return result, nil
}

// saveSubscriptionFeedItem atomically reuses the canonical URL identity or
// creates one Feed-managed reading link, then records the save association.
func (r *PGXReaderVNextRepository) saveSubscriptionFeedItem(ctx context.Context, db database.Querier, feedItemID uuid.UUID) (uuid.UUID, error) {
	var url, title, summary string
	if err := db.QueryRow(ctx, `SELECT url,COALESCE(title,''),COALESCE(summary,'') FROM feed_items WHERE id=$1 FOR UPDATE`, feedItemID).Scan(&url, &title, &summary); err != nil {
		return uuid.Nil, ErrNotFound
	}
	identity, err := urlidentity.Normalize(url)
	if err != nil {
		return uuid.Nil, ErrInvalidReaderFeedItem
	}
	// All writers that create a link for this canonical identity share this
	// transaction lock, including distinct feed items saved concurrently.
	if err := lockCanonicalLinkIdentity(ctx, db, identity); err != nil {
		return uuid.Nil, err
	}
	var existingLinkID uuid.UUID
	err = db.QueryRow(ctx, `SELECT link_id FROM reader_feed_saves WHERE feed_item_id=$1`, feedItemID).Scan(&existingLinkID)
	if err == nil {
		if _, err := r.restoreLinkLifecycleOn(ctx, db, existingLinkID); err != nil {
			return uuid.Nil, err
		}
		// The association may have disappeared while this save waited for the
		// common Link lock behind a concurrent unsave.
		err = db.QueryRow(ctx, `SELECT link_id FROM reader_feed_saves WHERE feed_item_id=$1`, feedItemID).Scan(&existingLinkID)
		if err == nil {
			return existingLinkID, nil
		}
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("read feed save association: %w", err)
	}
	matched, err := findCanonicalLink(ctx, db, identity)
	if err != nil {
		return uuid.Nil, err
	}
	var linkID uuid.UUID
	if matched == nil {
		err = db.QueryRow(ctx, `INSERT INTO links (url,source_kind,source_key,input_title,title,summary,tags,status,content,content_document,content_format,content_source,content_revision,library_kind,library_kind_locked,feed_managed,first_collected_at,created_at,updated_at) VALUES ($1,'subscription',$2,$3,$3,$4,'{}','done','',NULL,'markdown','user',1,'reading',true,true,NOW(),NOW(),NOW()) RETURNING id`, url, identity, title, summary).Scan(&linkID)
		if err != nil {
			return uuid.Nil, fmt.Errorf("create subscription saved link: %w", err)
		}
	} else {
		linkID = *matched
		if _, err := r.restoreLinkLifecycleOn(ctx, db, linkID); err != nil {
			return uuid.Nil, err
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO reader_feed_saves (feed_item_id,link_id) VALUES ($1,$2)`, feedItemID, linkID); err != nil {
		return uuid.Nil, fmt.Errorf("write feed save association: %w", err)
	}
	return linkID, nil
}

func (r *PGXReaderVNextRepository) unsaveSubscriptionFeedItem(ctx context.Context, db database.Querier, feedItemID uuid.UUID) (*uuid.UUID, error) {
	var savedLink, analyzedLink pgtype.UUID
	err := db.QueryRow(ctx, `SELECT save.link_id,item.link_id
		FROM feed_items item LEFT JOIN reader_feed_saves save ON save.feed_item_id=item.id
		WHERE item.id=$1`, feedItemID).Scan(&savedLink, &analyzedLink)
	if err != nil {
		return nil, err
	}
	var visibleLinkID *uuid.UUID
	if analyzedLink.Valid {
		value := uuid.UUID(analyzedLink.Bytes)
		visibleLinkID = &value
	}
	if !savedLink.Valid {
		return visibleLinkID, nil
	}
	linkID := uuid.UUID(savedLink.Bytes)
	var (
		feedManaged bool
		linkStatus  model.LinkStatus
	)
	if err := db.QueryRow(ctx, `SELECT feed_managed,status FROM links WHERE id=$1 FOR UPDATE`, linkID).Scan(&feedManaged, &linkStatus); err != nil {
		return nil, fmt.Errorf("lock feed save link: %w", err)
	}
	tag, err := db.Exec(ctx, `DELETE FROM reader_feed_saves WHERE feed_item_id=$1 AND link_id=$2`, feedItemID, linkID)
	if err != nil {
		return nil, fmt.Errorf("delete feed save association: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	var remaining bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reader_feed_saves WHERE link_id=$1)`, linkID).Scan(&remaining); err != nil {
		return nil, err
	}
	if !remaining && feedManaged {
		err = r.trashUnclaimedFeedManagedLinkOn(ctx, db, linkID, linkStatus)
	}
	if err != nil {
		return nil, err
	}
	return visibleLinkID, nil
}

func (r *PGXReaderVNextRepository) trashUnclaimedFeedManagedLinkOn(
	ctx context.Context,
	db database.Querier,
	linkID uuid.UUID,
	status model.LinkStatus,
) error {
	if r.linkLifecycleQueue == nil {
		if status == model.LinkStatusPending || status == model.LinkStatusProcessing {
			return errors.New("trash Feed-managed in-flight Link: lifecycle queue is not configured")
		}
		return terminalizeAndDeleteLockedLinkOn(ctx, db, linkID)
	}
	tx, ok := db.(pgx.Tx)
	if !ok {
		return errors.New("trash Feed-managed Link: transaction-bound lifecycle queue requires pgx.Tx")
	}
	if err := r.linkLifecycleQueue.CancelAllActiveTx(ctx, tx, linkID); err != nil {
		return fmt.Errorf("cancel Feed-managed Link work: %w", err)
	}
	return terminalizeAndDeleteLockedLinkOn(ctx, db, linkID)
}

// parseReaderFeedItemKey splits a "<kind>:<uuid>" feed key. The key is required
// to round-trip byte for byte, because it doubles as the primary key of the
// hide table: two spellings of the same UUID would otherwise hide different
// logical items.
func parseReaderFeedItemKey(itemKey string) (string, uuid.UUID, error) {
	kind, rawID, ok := strings.Cut(itemKey, ":")
	if !ok || strings.TrimSpace(rawID) == "" {
		return "", uuid.UUID{}, ErrInvalidReaderFeedItem
	}
	id, parseErr := uuid.Parse(rawID)
	if parseErr != nil {
		return "", uuid.UUID{}, ErrInvalidReaderFeedItem
	}
	if itemKey != kind+":"+id.String() {
		return "", uuid.UUID{}, ErrInvalidReaderFeedItem
	}
	if kind != "link" && kind != "subscription" && kind != "inbox" {
		return "", uuid.UUID{}, ErrInvalidReaderFeedItem
	}
	return kind, id, nil
}

// validateReaderFeedAction rejects actions the target kind cannot carry. Only
// Only subscription items can be saved. Hide is the single persisted negative
// action for every Feed source.
func validateReaderFeedAction(kind, action string) error {
	if action != "save" && action != "unsave" && action != "hide" {
		return ErrInvalidReaderFeedItem
	}
	if (action == "save" || action == "unsave") && kind != "subscription" {
		return ErrInvalidReaderFeedItem
	}
	return nil
}

func ensureReaderFeedItem(ctx context.Context, db database.Querier, kind string, kindID uuid.UUID) error {
	table := ""
	switch kind {
	case "link":
		table = "links"
	case "subscription":
		table = "feed_items"
	case "inbox":
		table = "reader_inbox"
	default:
		return ErrInvalidReaderFeedItem
	}
	var exists bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id=$1)`, kindID).Scan(&exists); err != nil {
		return fmt.Errorf("check reader feed item: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func (r *PGXReaderVNextRepository) RelatedTags(ctx context.Context, linkID *uuid.UUID, limit int) ([]string, error) {
	if limit <= 0 || limit > 50 {
		limit = 12
	}
	var tags []string
	if linkID != nil {
		if err := r.db.QueryRow(ctx, `SELECT COALESCE(tags,'{}') FROM links WHERE id=$1 AND status='done' AND deleted_at IS NULL`, *linkID).Scan(&tags); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("read related tag source: %w", err)
		}
	}
	return r.cooccurrenceRelatedTags(ctx, tags, limit)
}

// cooccurrenceRelatedTags returns tags that co-occur with the seed tags, or the
// installation's most-used tags when there is no seed.
func (r *PGXReaderVNextRepository) cooccurrenceRelatedTags(ctx context.Context, tags []string, limit int) ([]string, error) {
	var rows pgx.Rows
	var err error
	if len(tags) > 0 {
		rows, err = r.db.Query(ctx, `
				SELECT candidate FROM (
					SELECT unnest(l.tags) AS candidate, count(*) AS uses
					FROM links l WHERE l.status='done' AND l.deleted_at IS NULL AND l.tags && $1::text[]
					GROUP BY candidate
				) related
				-- The seed-tag exclusion lives in the outer WHERE, not in a
				-- HAVING on the aggregate: HAVING runs before SELECT output
				-- aliases exist, so it cannot name the candidate alias the way
				-- GROUP BY can, and it cannot repeat the expression either
				-- because unnest() is a set-returning function.
				WHERE candidate <> ALL($1::text[])
				ORDER BY uses DESC,candidate LIMIT $2`, tags, limit)
	} else {
		rows, err = r.db.Query(ctx, `SELECT tag FROM (SELECT unnest(tags) AS tag,count(*) AS uses FROM links WHERE status='done' AND deleted_at IS NULL AND tags IS NOT NULL GROUP BY tag ORDER BY uses DESC,tag LIMIT $1) related`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("related tags: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		out = append(out, tag)
	}
	return out, rows.Err()
}

const (
	readerTagActivityRows = `
		SELECT 'tag'::text AS kind, source.tag AS activity_key,
			MAX(source.event_at) AS last_at,
			lower(btrim(source.tag)) AS normalized_key
		FROM (
			SELECT GREATEST(l.created_at,l.first_collected_at,l.last_recollected_at) AS event_at,
				unnest(l.tags) AS tag
			FROM links l
			WHERE l.library_kind='reading' AND l.status='done'
				AND l.deleted_at IS NULL AND l.tags IS NOT NULL
		) source
		GROUP BY source.tag`
	readerDomainActivityRows = `
		SELECT 'domain'::text AS kind, l.domain AS activity_key,
			MAX(GREATEST(l.created_at,l.first_collected_at,l.last_recollected_at)) AS last_at,
			lower(btrim(l.domain)) AS normalized_key
		FROM links l
		WHERE l.library_kind='reading' AND l.status='done'
			AND l.deleted_at IS NULL AND l.domain IS NOT NULL AND l.domain <> ''
		GROUP BY l.domain`
)

func readerActivityRows(kind string) (string, error) {
	switch kind {
	case model.ReaderActivityKindAll:
		return readerTagActivityRows + " UNION ALL " + readerDomainActivityRows, nil
	case model.ReaderActivityKindTag:
		return readerTagActivityRows, nil
	case model.ReaderActivityKindDomain:
		return readerDomainActivityRows, nil
	default:
		return "", fmt.Errorf("%w: invalid activity kind", ErrInvalidReaderCursor)
	}
}

func (r *PGXReaderVNextRepository) ListActivity(ctx context.Context, query model.ReaderActivityQuery) (model.ReaderActivityPage, error) {
	if query.Limit <= 0 || query.Limit > 1000 {
		query.Limit = 100
	}
	base, err := readerActivityRows(query.Kind)
	if err != nil {
		return model.ReaderActivityPage{}, err
	}
	if query.After != nil && query.Kind != model.ReaderActivityKindAll && query.After.Kind != query.Kind {
		return model.ReaderActivityPage{}, fmt.Errorf("%w: activity cursor kind mismatch", ErrInvalidReaderCursor)
	}

	statement := `SELECT kind,activity_key,last_at,normalized_key FROM (` + base + `) activity`
	args := make([]any, 0, 5)
	if query.After != nil {
		statement += `
			WHERE last_at < $1
				OR (last_at = $1 AND (
					kind COLLATE "C" > $2 COLLATE "C"
					OR (kind = $2 AND (
						normalized_key COLLATE "C" > $3 COLLATE "C"
						OR (normalized_key = $3 AND activity_key COLLATE "C" > $4 COLLATE "C")
					))
				))`
		args = append(args, query.After.LastAt, query.After.Kind, query.After.NormalizedKey, query.After.Key)
	}
	args = append(args, query.Limit+1)
	statement += fmt.Sprintf(`
		ORDER BY last_at DESC, kind COLLATE "C" ASC, normalized_key COLLATE "C" ASC, activity_key COLLATE "C" ASC
		LIMIT $%d`, len(args))

	rows, err := r.db.Query(ctx, statement, args...)
	if err != nil {
		return model.ReaderActivityPage{}, err
	}
	defer rows.Close()
	items := make([]model.ReaderActivity, 0)
	for rows.Next() {
		var item model.ReaderActivity
		if err := rows.Scan(&item.Kind, &item.Key, &item.LastAt, &item.NormalizedKey); err != nil {
			return model.ReaderActivityPage{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return model.ReaderActivityPage{}, err
	}
	hasMore := len(items) > query.Limit
	if hasMore {
		items = items[:query.Limit]
	}
	return model.ReaderActivityPage{Items: items, HasMore: hasMore}, nil
}

func (r *PGXReaderVNextRepository) UpdateLinkMetadata(ctx context.Context, patch model.ReaderLinkMetadataPatch) (model.ReaderLinkMetadataUpdate, error) {
	var result model.ReaderLinkMetadataUpdate
	err := r.withTx(ctx, func(tx database.Querier) error {
		return r.updateLinkMetadata(ctx, tx, patch, &result)
	})
	if err != nil {
		return model.ReaderLinkMetadataUpdate{}, err
	}
	return result, nil
}

func (r *PGXReaderVNextRepository) updateLinkMetadata(ctx context.Context, tx database.Querier, patch model.ReaderLinkMetadataPatch, result *model.ReaderLinkMetadataUpdate) error {
	var (
		found        bool
		changed      bool
		tupleChanged bool
	)
	err := tx.QueryRow(ctx, `
			WITH target AS (
				SELECT id,title,summary,tags,metadata_revision
				FROM links
				WHERE id=$4 AND deleted_at IS NULL
					AND status='done' AND library_kind='reading' AND metadata_revision=$5
				FOR UPDATE
			), updated AS (
				UPDATE links AS link
				SET title=$1,
					summary=$2,
					tags=COALESCE($3::text[],'{}'::text[]),
					metadata_revision=link.metadata_revision+1,
					updated_at=NOW()
				FROM target
				WHERE link.id=target.id
					AND target.metadata_revision < $6
					AND (target.title IS DISTINCT FROM $1 OR
						target.summary IS DISTINCT FROM $2 OR
						target.tags IS DISTINCT FROM COALESCE($3::text[],'{}'::text[]))
				RETURNING link.metadata_revision
			)
			SELECT
				EXISTS (SELECT 1 FROM target),
				COALESCE((SELECT metadata_revision FROM updated), (SELECT metadata_revision FROM target), 0),
				COALESCE((SELECT tags IS DISTINCT FROM COALESCE($3::text[],'{}'::text[]) FROM target), false),
				EXISTS (SELECT 1 FROM updated),
				COALESCE((SELECT target.title IS DISTINCT FROM $1
					OR target.summary IS DISTINCT FROM $2
					OR target.tags IS DISTINCT FROM COALESCE($3::text[],'{}'::text[])
					FROM target), false)`,
		patch.Title, patch.Summary, patch.Tags, patch.LinkID, patch.ExpectedRevision, model.LinkMetadataMaxRevision,
	).Scan(&found, &result.MetadataRevision, &result.TagsChanged, &changed, &tupleChanged)
	if err != nil {
		return fmt.Errorf("update link metadata: %w", err)
	}
	if !found || tupleChanged && !changed {
		return ErrRevisionConflict
	}
	return nil
}
