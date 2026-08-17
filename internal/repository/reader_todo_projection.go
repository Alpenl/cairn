package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// replaceLinkThoughtTodoProjectionsOn covers the one Thought lifecycle edge
// that is not written by Go: a Link delete tombstones its Thoughts through a
// database trigger. Without this the Thoughts stop being TODO sources while
// their projections stay live, and no later read would notice.
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

// ReaderTodoProjectionBackfillID names the one-shot backfill in the ledger.
// It is a constant rather than a parameter because "has this installation been
// back-filled" must be one question with one answer.
const ReaderTodoProjectionBackfillID = "reader-todo-projection-v1"

// readerTodoProjectionBackfillLockKey serializes concurrent backfill runners.
// Two deploy jobs racing would otherwise both find an empty ledger, both
// rebuild, and one would fail on the primary key after doing all the work.
const readerTodoProjectionBackfillLockKey = "reader-todo-projection-backfill"

// ReaderTodoProjectionBackfillResult reports what one backfill call did.
// AlreadyComplete distinguishes "this run rebuilt the projection" from "a
// previous run already did", which is what makes a repeated call observably
// idempotent rather than merely harmless.
type ReaderTodoProjectionBackfillResult struct {
	AlreadyComplete bool
	ProjectedCount  int
	CompletedAt     time.Time
}

// BackfillTodoProjections rebuilds every projection from the current Thoughts
// and published Notes, verifies the result against those sources, and only
// then records completion — all inside one transaction. A verification failure
// therefore rolls back both the projections and the ledger row: a failed run
// can never leave a "completed" marker behind, and the next run starts from
// the same place this one did.
func (r *PGXReaderVNextRepository) BackfillTodoProjections(ctx context.Context) (ReaderTodoProjectionBackfillResult, error) {
	var out ReaderTodoProjectionBackfillResult
	err := r.withTx(ctx, func(db database.Querier) error {
		if _, err := db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, readerTodoProjectionBackfillLockKey); err != nil {
			return fmt.Errorf("lock todo projection backfill: %w", err)
		}
		var completedAt time.Time
		var projectedCount int
		err := db.QueryRow(ctx, `
			SELECT projected_count,completed_at
			FROM reader_todo_projection_backfills
			WHERE id=$1`, ReaderTodoProjectionBackfillID).Scan(&projectedCount, &completedAt)
		if err == nil {
			out = ReaderTodoProjectionBackfillResult{AlreadyComplete: true, ProjectedCount: projectedCount, CompletedAt: completedAt}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read todo projection backfill ledger: %w", err)
		}

		sources, err := listTodoHostSourcesOn(ctx, db)
		if err != nil {
			return err
		}
		desired := checklistTodosForSources(sources)
		if err := r.reconcileTodoProjectionsOn(ctx, db, desired); err != nil {
			return err
		}
		if err := verifyTodoProjectionsOn(ctx, db, desired); err != nil {
			return err
		}
		if err := db.QueryRow(ctx, `
			INSERT INTO reader_todo_projection_backfills (id,projected_count)
			VALUES ($1,$2)
			RETURNING projected_count,completed_at`, ReaderTodoProjectionBackfillID, len(desired)).
			Scan(&projectedCount, &completedAt); err != nil {
			return fmt.Errorf("record todo projection backfill: %w", err)
		}
		out = ReaderTodoProjectionBackfillResult{ProjectedCount: projectedCount, CompletedAt: completedAt}
		return nil
	})
	if err != nil {
		return ReaderTodoProjectionBackfillResult{}, err
	}
	return out, nil
}

// ErrReaderTodoProjectionUnverified reports that the rebuilt projection does
// not match the sources it was built from. It is deliberately fatal to the
// backfill transaction: recording completion over an unverified projection
// would turn a recoverable state into a permanent one.
var ErrReaderTodoProjectionUnverified = errors.New("reader todo projection failed verification")

const readerLiveTodoProjectionsSQL = `
SELECT origin_kind,COALESCE(origin_host_id,''),origin_ref,text,done,host_revision
FROM reader_todos
WHERE origin_kind <> 'standalone' AND deleted_at IS NULL`

// verifyTodoProjectionsOn re-reads what was just written and checks it against
// the sources. Every live projection must correspond to a source block, and
// every source block must either be live or carry a tombstone.
func verifyTodoProjectionsOn(ctx context.Context, db database.Querier, desired []model.ReaderTodo) error {
	wanted := make(map[string]model.ReaderTodo, len(desired))
	for _, todo := range desired {
		wanted[readerTodoProjectionKey(todo.OriginKind, valueOrEmpty(todo.OriginHostID), todo.OriginRef)] = todo
	}
	rows, err := db.Query(ctx, readerLiveTodoProjectionsSQL)
	if err != nil {
		return fmt.Errorf("verify todo projections: %w", err)
	}
	live := make(map[string]struct{}, len(desired))
	for rows.Next() {
		var originKind, hostID, text string
		var originRef []byte
		var done bool
		var hostRevision int64
		if err := rows.Scan(&originKind, &hostID, &originRef, &text, &done, &hostRevision); err != nil {
			rows.Close()
			return fmt.Errorf("scan verified todo projection: %w", err)
		}
		key := readerTodoProjectionKey(originKind, hostID, originRef)
		todo, ok := wanted[key]
		if !ok {
			rows.Close()
			return fmt.Errorf("%w: %s/%s has no source block", ErrReaderTodoProjectionUnverified, originKind, hostID)
		}
		if todo.Text != text || todo.Done != done || todo.HostRevision != hostRevision {
			rows.Close()
			return fmt.Errorf("%w: %s/%s disagrees with its source block", ErrReaderTodoProjectionUnverified, originKind, hostID)
		}
		live[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read verified todo projections: %w", err)
	}
	rows.Close()
	return verifyTodoProjectionTombstonesOn(ctx, db, wanted, live)
}

// verifyTodoProjectionTombstonesOn accounts for every source block that is not
// live. The only legitimate reason is a tombstone the user created by
// dismissing it; anything else means the rebuild dropped a block.
func verifyTodoProjectionTombstonesOn(ctx context.Context, db database.Querier, wanted map[string]model.ReaderTodo, live map[string]struct{}) error {
	if len(wanted) == len(live) {
		return nil
	}
	existing, err := readerExistingTodoProjections(ctx, db)
	if err != nil {
		return err
	}
	tombstoned := make(map[string]struct{}, len(existing))
	for _, item := range existing {
		if item.deletedAt != nil {
			tombstoned[readerTodoProjectionKey(item.origin, valueOrEmpty(item.hostID), item.originRef)] = struct{}{}
		}
	}
	for key, todo := range wanted {
		if _, ok := live[key]; ok {
			continue
		}
		if _, ok := tombstoned[key]; ok {
			continue
		}
		return fmt.Errorf("%w: %s/%s was not projected and has no tombstone",
			ErrReaderTodoProjectionUnverified, todo.OriginKind, valueOrEmpty(todo.OriginHostID))
	}
	return nil
}

// readerTodoHostSourcesSQL enumerates every live TODO source in the
// installation: undeleted, untombstoned Thoughts and undeleted Notes at their
// published revision. Only the backfill and the explicit repair walk all of
// them; ordinary reads and writes work one host at a time.
const readerTodoHostSourcesSQL = `
SELECT 'thought'::text AS host_kind, t.id::text AS host_id, t.last_sequence AS host_revision, t.body,
	t.host_kind AS source_kind, t.host_id AS source_id, t.link_id
FROM reader_thoughts t
WHERE t.deleted=false
	AND NOT EXISTS (
		SELECT 1 FROM reader_thought_tombstones tt
		WHERE tt.thought_id=t.id
	)
UNION ALL
SELECT 'note'::text AS host_kind, n.id::text AS host_id, n.published_revision AS host_revision, n.published_content,
	'note'::text AS source_kind, n.id::text AS source_id, NULL::uuid AS link_id
FROM reader_notes n
WHERE n.deleted_at IS NULL
ORDER BY host_kind, host_id`

func listTodoHostSourcesOn(ctx context.Context, db database.Querier) ([]readerTodoHostSource, error) {
	rows, err := db.Query(ctx, readerTodoHostSourcesSQL)
	if err != nil {
		return nil, fmt.Errorf("list todo host sources: %w", err)
	}
	defer rows.Close()

	sources := make([]readerTodoHostSource, 0, 32)
	for rows.Next() {
		source := readerTodoHostSource{live: true}
		if err := rows.Scan(
			&source.originKind, &source.hostID, &source.hostRevision, &source.body,
			&source.sourceKind, &source.sourceID, &source.linkID,
		); err != nil {
			return nil, fmt.Errorf("scan todo host source: %w", err)
		}
		if source.sourceKind == "" || source.sourceID == "" {
			source.sourceKind, source.sourceID = source.originKind, source.hostID
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read todo host sources: %w", err)
	}
	return sources, nil
}

// checklistTodosForSources flattens a whole-installation source scan into the
// projections those sources own, through the same builder the single-host
// replace pass uses so a repair and a write agree byte for byte.
func checklistTodosForSources(sources []readerTodoHostSource) []model.ReaderTodo {
	out := make([]model.ReaderTodo, 0, len(sources))
	for _, source := range sources {
		out = append(out, readerChecklistTodos(source)...)
	}
	return out
}
