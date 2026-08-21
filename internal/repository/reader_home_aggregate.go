package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/model"
)

const (
	readerHomeContinueReadingLimit = 3
	readerHomeRecentThoughtsLimit  = 5
)

type readerHomeSnapshotBeginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

const readerHomeListTodosSQL = `SELECT ` + readerTodoColumns + ` FROM reader_todos WHERE deleted_at IS NULL ORDER BY done ASC, due_at ASC NULLS LAST, created_at DESC, id DESC`

const readerHomeCountsSQL = `
SELECT
	(SELECT count(*)::int FROM reader_inbox WHERE status='pending' AND deleted_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())),
	(SELECT count(*)::int FROM reader_inbox WHERE status='pending' AND deleted_at IS NULL AND expires_at IS NOT NULL AND expires_at <= NOW()),
	(SELECT count(*)::int FROM links WHERE status='done' AND library_kind='reading' AND deleted_at IS NULL),
	(SELECT count(*)::int FROM sites),
	(SELECT count(*)::int FROM feed_subscriptions WHERE active=true),
	(SELECT count(*)::int FROM reader_notes WHERE deleted_at IS NULL),
	(SELECT count(*)::int FROM reader_todos WHERE deleted_at IS NULL AND done=false),
	(SELECT count(*)::int FROM reader_thoughts t
		WHERE t.deleted=false
		AND NOT EXISTS (
			SELECT 1 FROM reader_thought_tombstones tt
			WHERE tt.thought_id=t.id
		)),
	(SELECT count(*)::int FROM feed_items WHERE read_at IS NULL AND NOT read_later)`

const readerHomeContinueReadingSQL = `
SELECT l.id,l.url,COALESCE(l.title,''),COALESCE(l.summary,''),
		COALESCE(e.read,false),COALESCE(e.read_later,false),COALESCE(e.progress,0),
		e.last_opened,l.created_at
FROM links l JOIN reader_engagement e ON e.link_id=l.id
WHERE l.status='done' AND l.library_kind='reading' AND l.deleted_at IS NULL
	AND e.progress > 0 AND e.progress < 1 AND e.last_opened IS NOT NULL
ORDER BY e.last_opened DESC,e.updated_at DESC,l.id DESC LIMIT $1`

const readerHomeRecentThoughtsSQL = `
SELECT ` + readerThoughtColumns + `
FROM reader_thoughts
WHERE deleted=false
	AND NOT EXISTS (
		SELECT 1 FROM reader_thought_tombstones tt
		WHERE tt.thought_id=reader_thoughts.id
	)
ORDER BY updated_at DESC,id DESC LIMIT $1`

// LoadHomeAggregate reads every Home section in one read-only repeatable-read
// transaction. Home used to reconcile the TODO projection here, which made a
// plain GET take row locks and write; two concurrent reads then had to lose a
// serialization race and retry. The projection is now maintained by whichever
// transaction changes a Thought or Note, so Home only has to read it.
func (r *PGXReaderVNextRepository) LoadHomeAggregate(ctx context.Context) (model.ReaderHomeAggregate, error) {
	beginner, ok := r.db.(readerHomeSnapshotBeginner)
	if !ok {
		return model.ReaderHomeAggregate{}, fmt.Errorf("load home aggregate: snapshot transaction unavailable")
	}
	return r.loadHomeAggregateSnapshot(ctx, beginner)
}

func (r *PGXReaderVNextRepository) loadHomeAggregateSnapshot(ctx context.Context, beginner readerHomeSnapshotBeginner) (model.ReaderHomeAggregate, error) {
	tx, err := beginner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return model.ReaderHomeAggregate{}, fmt.Errorf("begin home aggregate snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	aggregate, err := r.loadHomeAggregateOn(ctx, tx)
	if err != nil {
		return model.ReaderHomeAggregate{}, fmt.Errorf("load home aggregate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return model.ReaderHomeAggregate{}, fmt.Errorf("commit home aggregate snapshot: %w", err)
	}
	return aggregate, nil
}

func (r *PGXReaderVNextRepository) loadHomeAggregateOn(ctx context.Context, db database.Querier) (model.ReaderHomeAggregate, error) {
	todos, err := listHomeTodosOn(ctx, db)
	if err != nil {
		return model.ReaderHomeAggregate{}, err
	}
	counts, err := homeCountsOn(ctx, db)
	if err != nil {
		return model.ReaderHomeAggregate{}, err
	}
	continueReading, err := listHomeContinueReadingOn(ctx, db, readerHomeContinueReadingLimit)
	if err != nil {
		return model.ReaderHomeAggregate{}, err
	}
	recentThoughts, err := listHomeRecentThoughtsOn(ctx, db, readerHomeRecentThoughtsLimit)
	if err != nil {
		return model.ReaderHomeAggregate{}, err
	}

	return model.ReaderHomeAggregate{
		Freshness:       model.ReaderHomeFreshnessFresh,
		Counts:          counts,
		ContinueReading: continueReading,
		RecentThoughts:  recentThoughts,
		Todos:           todos,
	}, nil
}

func listHomeTodosOn(ctx context.Context, db database.Querier) ([]model.ReaderTodo, error) {
	rows, err := db.Query(ctx, readerHomeListTodosSQL)
	if err != nil {
		return nil, fmt.Errorf("list home todos: %w", err)
	}
	defer rows.Close()

	out := make([]model.ReaderTodo, 0, 32)
	for rows.Next() {
		item, err := scanReaderTodo(rows)
		if err != nil {
			return nil, fmt.Errorf("scan home todo: %w", err)
		}
		out = append(out, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read home todos: %w", err)
	}
	return out, nil
}

func homeCountsOn(ctx context.Context, db database.Querier) (map[string]int, error) {
	var pending, expired, reading, sites, subs, notes, todos, thoughts, unread int
	if err := db.QueryRow(ctx, readerHomeCountsSQL).Scan(
		&pending, &expired, &reading, &sites, &subs, &notes, &todos, &thoughts, &unread,
	); err != nil {
		return nil, fmt.Errorf("read home counts: %w", err)
	}
	return map[string]int{
		"pending":       pending,
		"reading":       reading,
		"sites":         sites,
		"subs":          subs,
		"notes":         notes,
		"inbox":         pending,
		"inbox_expired": expired,
		"links":         reading,
		"subscriptions": subs,
		"todos":         todos,
		"thoughts":      thoughts,
		"unread":        unread,
	}, nil
}

func listHomeContinueReadingOn(ctx context.Context, db database.Querier, limit int) ([]model.ReaderFeedItem, error) {
	rows, err := db.Query(ctx, readerHomeContinueReadingSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("list home continue reading: %w", err)
	}
	defer rows.Close()

	out := make([]model.ReaderFeedItem, 0, limit)
	for rows.Next() {
		var item model.ReaderFeedItem
		var id uuid.UUID
		var progress float32
		var lastOpened time.Time
		if err := rows.Scan(&id, &item.URL, &item.Title, &item.Summary, &item.Read, &item.ReadLater, &progress, &lastOpened, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan home continue reading: %w", err)
		}
		item.Key = "link:" + id.String()
		item.Source = "reading"
		item.LinkID = &id
		item.PublishedAt = &lastOpened
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read home continue reading: %w", err)
	}
	return out, nil
}

func listHomeRecentThoughtsOn(ctx context.Context, db database.Querier, limit int) ([]model.ReaderThought, error) {
	rows, err := db.Query(ctx, readerHomeRecentThoughtsSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("list home recent thoughts: %w", err)
	}
	defer rows.Close()

	out := make([]model.ReaderThought, 0, limit)
	for rows.Next() {
		item, err := scanReaderThought(rows)
		if err != nil {
			return nil, fmt.Errorf("scan home recent thought: %w", err)
		}
		out = append(out, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read home recent thoughts: %w", err)
	}
	return out, nil
}
