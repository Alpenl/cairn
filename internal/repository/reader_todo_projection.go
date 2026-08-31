package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/model"
	"webtag/internal/readertext"
)

// readerTodoHostKinds are the two host kinds that own TODO projections.
// A standalone TODO has no host and is never touched by a replace pass.
const (
	readerTodoHostThought = "thought"
	readerTodoHostNote    = "note"
)

// readerTodoHostSource is one authoritative TODO source read inside the
// caller's transaction. live=false means the host currently emits nothing:
// a deleted or tombstoned Thought, a trashed Note, or a Note that has never
// been published. Draft content is deliberately absent — a draft is not a
// published checklist and must not reach the projection.
type readerTodoHostSource struct {
	originKind   string
	hostID       string
	hostRevision int64
	body         string
	sourceKind   string
	sourceID     string
	linkID       *uuid.UUID
	live         bool
}

// replaceHostTodoProjectionsOn makes the stored projections of one host match
// what that host currently emits, inside the caller's transaction. It is the
// single write-time primitive behind every Thought and Note command: the
// caller says which host changed, and this reads the committed-in-transaction
// source itself rather than trusting a body threaded through the call chain.
//
// It reuses the projection identity rules already established —
// readertext.List for the blocks, idx_reader_todos_projection for the unique
// key, and the soft-deleted rows as tombstones that must not be resurrected.
func (r *PGXReaderVNextRepository) replaceHostTodoProjectionsOn(ctx context.Context, db database.Querier, originKind, hostID string) error {
	source, err := readTodoHostSourceOn(ctx, db, originKind, hostID)
	if err != nil {
		return err
	}
	existing, err := readerExistingHostTodoProjections(ctx, db, originKind, hostID)
	if err != nil {
		return err
	}
	return r.applyTodoProjectionsOn(ctx, db, readerChecklistTodos(source), existing)
}

// replaceThoughtTodoProjectionsOn refreshes one Thought's projections. Every
// Thought command funnels through materialization or tombstoning, so the two
// call sites below that name it cover create, update, delete, reattach,
// lifecycle tombstones, host restore, note reanchor, and checkbox writeback.
func (r *PGXReaderVNextRepository) replaceThoughtTodoProjectionsOn(ctx context.Context, db database.Querier, thoughtID string) error {
	return r.replaceHostTodoProjectionsOn(ctx, db, readerTodoHostThought, thoughtID)
}

// replaceNoteTodoProjectionsOn refreshes one Note's projections. Only the
// published revision is a source; a draft save or discard changes nothing the
// projection can see.
func (r *PGXReaderVNextRepository) replaceNoteTodoProjectionsOn(ctx context.Context, db database.Querier, noteID uuid.UUID) error {
	return r.replaceHostTodoProjectionsOn(ctx, db, readerTodoHostNote, noteID.String())
}

const readerLinkHostedThoughtsSQL = `
SELECT id FROM reader_thoughts WHERE host_kind='link' AND host_id=$1::text ORDER BY id`

// replaceLinkThoughtTodoProjectionsOn covers Link lifecycle transitions that
// retire every Thought anchored to one Link. Without this the Thoughts stop
// being TODO sources while their projections stay live, and no later read
// would notice.
func (r *PGXReaderVNextRepository) replaceLinkThoughtTodoProjectionsOn(ctx context.Context, db database.Querier, linkID uuid.UUID) error {
	rows, err := db.Query(ctx, readerLinkHostedThoughtsSQL, linkID)
	if err != nil {
		return fmt.Errorf("list link hosted thoughts: %w", err)
	}
	thoughtIDs := make([]string, 0, 8)
	for rows.Next() {
		var thoughtID string
		if err := rows.Scan(&thoughtID); err != nil {
			rows.Close()
			return fmt.Errorf("scan link hosted thought: %w", err)
		}
		thoughtIDs = append(thoughtIDs, thoughtID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read link hosted thoughts: %w", err)
	}
	for _, thoughtID := range thoughtIDs {
		if err := r.replaceThoughtTodoProjectionsOn(ctx, db, thoughtID); err != nil {
			return err
		}
	}
	return nil
}

// applyTodoProjectionsOn updates one host. existing must already cover every
// projection key inside the requested scope, including the soft-deleted ones:
// without the tombstones a refresh silently recreates a dismissed TODO.
func (r *PGXReaderVNextRepository) applyTodoProjectionsOn(
	ctx context.Context,
	db database.Querier,
	todos []model.ReaderTodo,
	existing []readerExistingTodoProjection,
) error {
	desired := make(map[string]struct{}, len(todos))
	for _, todo := range todos {
		if todo.OriginKind == "standalone" {
			continue
		}
		desired[readerTodoProjectionKey(todo.OriginKind, valueOrEmpty(todo.OriginHostID), todo.OriginRef)] = struct{}{}
	}
	deleted := make(map[string]struct{}, len(existing))
	for _, item := range existing {
		if item.deletedAt != nil {
			deleted[readerTodoProjectionKey(item.origin, valueOrEmpty(item.hostID), item.originRef)] = struct{}{}
		}
	}
	if err := r.refreshTodoProjections(ctx, db, todos, deleted); err != nil {
		return err
	}
	return dismissStaleTodoProjections(ctx, db, existing, desired)
}

const readerThoughtTodoSourceSQL = `
SELECT body,last_sequence,host_kind,host_id,link_id
FROM reader_thoughts
WHERE id=$1 AND deleted=false
	AND NOT EXISTS (
		SELECT 1 FROM reader_thought_tombstones tt
		WHERE tt.thought_id=reader_thoughts.id
	)`

const readerNoteTodoSourceSQL = `
SELECT published_content,published_revision
FROM reader_notes
WHERE id=$1 AND deleted_at IS NULL`

// readTodoHostSourceOn reads one host's current checklist source. A host that
// no longer exists, is deleted, or is tombstoned is reported as not live with
// an empty body, which makes the replace pass dismiss its projections instead
// of failing.
func readTodoHostSourceOn(ctx context.Context, db database.Querier, originKind, hostID string) (readerTodoHostSource, error) {
	source := readerTodoHostSource{originKind: originKind, hostID: hostID}
	switch originKind {
	case readerTodoHostThought:
		var hostKind, hostRefID string
		err := db.QueryRow(ctx, readerThoughtTodoSourceSQL, hostID).
			Scan(&source.body, &source.hostRevision, &hostKind, &hostRefID, &source.linkID)
		if errors.Is(err, pgx.ErrNoRows) {
			return source, nil
		}
		if err != nil {
			return readerTodoHostSource{}, fmt.Errorf("read thought todo source: %w", err)
		}
		source.sourceKind, source.sourceID = hostKind, hostRefID
		if source.sourceKind == "" || source.sourceID == "" {
			source.sourceKind, source.sourceID = readerTodoHostThought, hostID
		}
		source.live = true
		return source, nil
	case readerTodoHostNote:
		// A host id that is not a UUID cannot name a note, so the host emits
		// nothing. Reporting it as an error would fail the write that changed
		// some other host instead of retiring an impossible projection.
		noteID, err := uuid.Parse(hostID)
		if err != nil {
			return source, nil //nolint:nilerr // an unparseable host id is a dead host, not a failure
		}
		err = db.QueryRow(ctx, readerNoteTodoSourceSQL, noteID).Scan(&source.body, &source.hostRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			return source, nil
		}
		if err != nil {
			return readerTodoHostSource{}, fmt.Errorf("read note todo source: %w", err)
		}
		source.sourceKind, source.sourceID = readerTodoHostNote, hostID
		source.live = true
		return source, nil
	default:
		return readerTodoHostSource{}, fmt.Errorf("unsupported todo projection host kind %q", originKind)
	}
}

// readerChecklistTodos turns one host source into the projections it owns.
// The origin_ref carries both the block identity used as the unique key and
// the source pointer the Reader turns into a "jump to source" link.
func readerChecklistTodos(source readerTodoHostSource) []model.ReaderTodo {
	if !source.live {
		return nil
	}
	blocks := readertext.List(source.body)
	out := make([]model.ReaderTodo, 0, len(blocks))
	for _, block := range blocks {
		originRefValue := map[string]any{
			"block_ref":  block.BlockRef,
			"text":       block.Text,
			"occurrence": block.Occurrence,
		}
		if source.sourceKind != "" && source.sourceID != "" {
			originRefValue["source_kind"] = source.sourceKind
			originRefValue["source_id"] = source.sourceID
		}
		if source.linkID != nil {
			originRefValue["link_id"] = source.linkID.String()
		}
		originRef, _ := json.Marshal(originRefValue)
		originHostKind, originHostID := source.originKind, source.hostID
		out = append(out, model.ReaderTodo{
			Text:           block.Text,
			Done:           block.Done,
			OriginKind:     source.originKind,
			OriginHostKind: &originHostKind,
			OriginHostID:   &originHostID,
			OriginRef:      originRef,
			HostRevision:   source.hostRevision,
		})
	}
	return out
}

const readerExistingHostTodoProjectionsSQL = `
SELECT id,origin_kind,origin_host_id,origin_ref,deleted_at
FROM reader_todos
WHERE origin_kind=$1 AND origin_host_id=$2
ORDER BY id
FOR UPDATE`

// readerExistingHostTodoProjections also returns soft-deleted rows because
// they are the tombstones the replace pass must honour. Ordering by id gives
// concurrent writers touching the same host the same row-lock order.
func readerExistingHostTodoProjections(ctx context.Context, db database.Querier, originKind, hostID string) ([]readerExistingTodoProjection, error) {
	rows, err := db.Query(ctx, readerExistingHostTodoProjectionsSQL, originKind, hostID)
	if err != nil {
		return nil, fmt.Errorf("list existing host todo projections: %w", err)
	}
	existing, err := scanReaderExistingTodoProjections(rows)
	if err != nil {
		return nil, err
	}
	return existing, nil
}
