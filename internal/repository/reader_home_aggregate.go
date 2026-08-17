package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"webtag/internal/database"
	"webtag/internal/model"
)

const (
	readerHomeContinueReadingLimit = 3
	readerHomeRecentThoughtsLimit  = 5
)

type ReaderHomeFreshness string

const (
	ReaderHomeFreshnessFresh   ReaderHomeFreshness = "fresh"
	ReaderHomeFreshnessPartial ReaderHomeFreshness = "partial"
	ReaderHomeFreshnessStale   ReaderHomeFreshness = "stale"
)

// ReaderHomeAggregate is the persistence result for one authoritative Home
// read. All fields are produced inside the same repeatable-read transaction;
// a caller must treat the result as a whole rather than assembling it with
// the individual list methods below the Reader service. Partial results are
// never returned: any failed section aborts the transaction and returns an
// error instead. Partial and stale are still stable raw freshness states for
// an upper layer that adds an explicitly defined degraded or cached source;
// this repository has no such source and therefore only emits fresh.
type ReaderHomeAggregate struct {
	Freshness       ReaderHomeFreshness
	Counts          map[string]int
	ContinueReading []model.ReaderFeedItem
	RecentThoughts  []model.ReaderThought
	Todos           []model.ReaderTodo
}

type readerHomeSnapshotBeginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

const readerHomeTodoSourcesSQL = `
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

const readerHomeListTodosSQL = `SELECT ` + readerTodoColumns + ` FROM reader_todos WHERE deleted_at IS NULL ORDER BY done ASC, due_at ASC NULLS LAST, created_at DESC, id DESC`

const readerHomeCountsSQL = `
SELECT
	(SELECT count(*)::int FROM reader_inbox WHERE status='pending' AND deleted_at IS NULL AND expired_at IS NULL),
	(SELECT count(*)::int FROM reader_inbox WHERE status='pending' AND deleted_at IS NULL AND expired_at IS NOT NULL),
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

// LoadHomeAggregate reconciles source-owned TODO projections and reads every
// Home section in one repeatable-read transaction. The transaction is
// intentionally read-write because reconciliation is part of the authority
// of the returned TODO list and count.
func (r *PGXReaderVNextRepository) LoadHomeAggregate(ctx context.Context) (ReaderHomeAggregate, error) {
	beginner, ok := r.db.(readerHomeSnapshotBeginner)
	if !ok {
		return ReaderHomeAggregate{}, fmt.Errorf("load home aggregate: snapshot transaction unavailable")
	}

	// 这是一个读写快照：取完 RepeatableRead 快照后还会 FOR UPDATE 并写回 TODO
	// 投影。RR 下对快照之后提交的行加锁会直接抛 40001，于是两个并发的纯读请求
	// （…/home 与 …/todos 都会同步投影）必然有一方失败。序列化失败是可重试的，
	// 重开快照即可；不重试就会把一次 GET 变成 500。
	const homeAggregateMaxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < homeAggregateMaxAttempts; attempt++ {
		aggregate, err := r.loadHomeAggregateSnapshot(ctx, beginner)
		if err == nil {
			return aggregate, nil
		}
		if !isSerializationFailure(err) {
			return ReaderHomeAggregate{}, err
		}
		lastErr = err
	}
	return ReaderHomeAggregate{}, lastErr
}

func (r *PGXReaderVNextRepository) loadHomeAggregateSnapshot(ctx context.Context, beginner readerHomeSnapshotBeginner) (ReaderHomeAggregate, error) {
	tx, err := beginner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return ReaderHomeAggregate{}, fmt.Errorf("begin home aggregate snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	aggregate, err := r.loadHomeAggregateOn(ctx, tx)
	if err != nil {
		return ReaderHomeAggregate{}, fmt.Errorf("load home aggregate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ReaderHomeAggregate{}, fmt.Errorf("commit home aggregate snapshot: %w", err)
	}
	return aggregate, nil
}

// isSerializationFailure reports whether err is a retryable PostgreSQL
// serialization failure (SQLSTATE 40001) or deadlock (40P01).
func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40001" || pgErr.Code == "40P01"
}

func (r *PGXReaderVNextRepository) loadHomeAggregateOn(ctx context.Context, db database.Querier) (ReaderHomeAggregate, error) {
	sources, err := listHomeTodoSourcesOn(ctx, db)
	if err != nil {
		return ReaderHomeAggregate{}, err
	}
	projections := homeChecklistTodos(sources)
	// Home shares the general reconcile so a projection the user dismissed
	// stays dismissed no matter which read reaches the database first.
	if err := r.reconcileTodoProjectionsOn(ctx, db, projections); err != nil {
		return ReaderHomeAggregate{}, err
	}

	todos, err := listHomeTodosOn(ctx, db)
	if err != nil {
		return ReaderHomeAggregate{}, err
	}
	counts, err := homeCountsOn(ctx, db)
	if err != nil {
		return ReaderHomeAggregate{}, err
	}
	continueReading, err := listHomeContinueReadingOn(ctx, db, readerHomeContinueReadingLimit)
	if err != nil {
		return ReaderHomeAggregate{}, err
	}
	recentThoughts, err := listHomeRecentThoughtsOn(ctx, db, readerHomeRecentThoughtsLimit)
	if err != nil {
		return ReaderHomeAggregate{}, err
	}

	return ReaderHomeAggregate{
		Freshness:       ReaderHomeFreshnessFresh,
		Counts:          counts,
		ContinueReading: continueReading,
		RecentThoughts:  recentThoughts,
		Todos:           todos,
	}, nil
}

func listHomeTodoSourcesOn(ctx context.Context, db database.Querier) ([]readerTodoHostSource, error) {
	rows, err := db.Query(ctx, readerHomeTodoSourcesSQL)
	if err != nil {
		return nil, fmt.Errorf("list home todo sources: %w", err)
	}
	defer rows.Close()

	sources := make([]readerTodoHostSource, 0, 32)
	for rows.Next() {
		source := readerTodoHostSource{live: true}
		if err := rows.Scan(
			&source.originKind, &source.hostID, &source.hostRevision, &source.body,
			&source.sourceKind, &source.sourceID, &source.linkID,
		); err != nil {
			return nil, fmt.Errorf("scan home todo source: %w", err)
		}
		if source.sourceKind == "" || source.sourceID == "" {
			source.sourceKind, source.sourceID = source.originKind, source.hostID
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read home todo sources: %w", err)
	}
	return sources, nil
}

// homeChecklistTodos builds the same projections the single-host replace pass
// builds, so a Home read cannot rewrite an origin_ref that a Thought or Note
// write just stored.
func homeChecklistTodos(sources []readerTodoHostSource) []model.ReaderTodo {
	out := make([]model.ReaderTodo, 0, len(sources))
	for _, source := range sources {
		out = append(out, readerChecklistTodos(source)...)
	}
	return out
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
		item.ReasonCode = "continue_reading"
		item.ReasonText = fmt.Sprintf("已读 %.0f%%，继续阅读", progress*100)
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
