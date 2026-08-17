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
// It reuses the identity rules the reconcile pass already established —
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

// applyTodoProjectionsOn is the shared body of both the whole-installation
// reconcile and the single-host replace. existing must already cover every
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
		noteID, parseErr := uuid.Parse(hostID)
		if parseErr != nil {
			return source, nil
		}
		err := db.QueryRow(ctx, readerNoteTodoSourceSQL, noteID).Scan(&source.body, &source.hostRevision)
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

// readerExistingHostTodoProjections is the host-scoped twin of
// readerExistingTodoProjections. It also returns the soft-deleted rows because
// they are the tombstones the replace pass must honour, and it orders by id so
// two concurrent writers touching the same host take the row locks in the same
// order.
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
