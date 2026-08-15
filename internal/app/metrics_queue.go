package app

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"webtag/internal/observability"
)

// queuePendingGaugeTTL bounds how often the queue-depth gauge hits the
// database. Prometheus scrapes can arrive every few seconds and from
// multiple replicas; without a cache each scrape would fire a COUNT(*)
// against river_job. 60s mirrors registerPendingProposalsGauge — well
// inside any reasonable backlog alert window (parse latency is seconds to
// a couple minutes) while keeping DB load negligible.
const queuePendingGaugeTTL = 60 * time.Second

const riverCapacitySnapshotSQL = `
SELECT
    count(*) FILTER (WHERE state = 'cancelled')::bigint,
    count(*) FILTER (WHERE state = 'completed')::bigint,
    count(*) FILTER (WHERE state = 'discarded')::bigint,
    COALESCE(
        EXTRACT(EPOCH FROM (
            clock_timestamp() - MIN(finalized_at) FILTER (
                WHERE state IN ('cancelled', 'completed', 'discarded')
                  AND finalized_at IS NOT NULL
            )
        )),
        0
    )::double precision,
    pg_catalog.pg_table_size('public.river_job')::bigint,
    pg_catalog.pg_indexes_size('public.river_job')::bigint,
    CASE
        WHEN $1::bigint < 0 THEN 0
        ELSE count(*) FILTER (
            WHERE state IN ('cancelled', 'completed', 'discarded')
              AND finalized_at IS NOT NULL
              AND finalized_at < clock_timestamp() - ($1::double precision * INTERVAL '1 millisecond')
        )
    END::bigint
FROM public.river_job`

// registerQueuePendingGauge wires the webtag_queue_jobs_pending GaugeFunc.
// It replaces the retired QueueInFlight gauge (the in-memory queue埋点 that
// had zero producers after Phase 13). Now the source of truth is the
// river_job table itself: the value func counts jobs in River's "available"
// state — enqueued and waiting for a worker to pick them up — which is the
// observable backlog operators alert on. running / scheduled / retryable
// states are deliberately excluded: "available" is the actionable
// "work is queued but not yet started" signal; the others are transient
// execution / retry bookkeeping that would muddy a backlog alert.
//
// The value func is invoked on every Prometheus scrape, so it serves a
// cached count and only re-queries when the cached value is older than
// queuePendingGaugeTTL. A query error keeps the last known value (no log
// spam on a transient DB blip) so the gauge degrades to stale-but-present.
//
// pool==nil is a no-op so opt-out / test wiring without a pool does not
// register a gauge that would always error.
func registerQueuePendingGauge(metrics *observability.Metrics, pool *pgxpool.Pool) {
	if metrics == nil || pool == nil {
		return
	}
	c := &cachedQueuePending{pool: pool, ttl: queuePendingGaugeTTL}
	metrics.RegisterGaugeFunc(
		"queue",
		"jobs_pending",
		"Number of parse jobs queued and waiting for a worker (river_job state='available'), cached and refreshed at most every 60s. A rising value means parsing throughput is falling behind submissions.",
		c.value,
	)
}

type riverCapacityQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type riverCapacitySnapshot struct {
	cancelled      float64
	completed      float64
	discarded      float64
	oldestAge      float64
	tableBytes     float64
	indexBytes     float64
	cleanupOverdue float64
	querySuccess   float64
}

type cachedRiverCapacity struct {
	queryer           riverCapacityQuerier
	ttl               time.Duration
	retentionMillis   int64
	mu                sync.Mutex
	snapshot0         riverCapacitySnapshot
	lastAttemptedAt   time.Time
	hasAttemptedQuery bool
}

func registerRiverCapacityGauges(
	metrics *observability.Metrics,
	queryer riverCapacityQuerier,
	retention time.Duration,
) {
	if metrics == nil || queryer == nil {
		return
	}
	retentionMillis := retention.Milliseconds()
	if retention < 0 {
		retentionMillis = -1
	}
	capacity := &cachedRiverCapacity{
		queryer: queryer, ttl: queuePendingGaugeTTL, retentionMillis: retentionMillis,
	}
	for _, state := range []string{"cancelled", "completed", "discarded"} {
		state := state
		metrics.RegisterConstGaugeFunc(
			"river",
			"terminal_jobs",
			"Retained River terminal rows partitioned by bounded state.",
			prometheus.Labels{"state": state},
			func() float64 { return capacity.terminal(state) },
		)
	}
	metrics.RegisterGaugeFunc(
		"river", "oldest_terminal_age_seconds",
		"Age in seconds of the oldest retained River terminal row.",
		func() float64 { return capacity.current().oldestAge },
	)
	metrics.RegisterGaugeFunc(
		"river", "job_table_bytes",
		"Bytes occupied by the River job table excluding indexes.",
		func() float64 { return capacity.current().tableBytes },
	)
	metrics.RegisterGaugeFunc(
		"river", "job_index_bytes",
		"Bytes occupied by indexes on the River job table.",
		func() float64 { return capacity.current().indexBytes },
	)
	metrics.RegisterGaugeFunc(
		"river", "cleanup_overdue_jobs",
		"Terminal River rows older than the configured retention window; always zero while infinite retention is selected.",
		func() float64 { return capacity.current().cleanupOverdue },
	)
	metrics.RegisterGaugeFunc(
		"river", "capacity_query_success",
		"Whether the latest cached River capacity query succeeded (1) or failed (0).",
		func() float64 { return capacity.current().querySuccess },
	)
}

func (c *cachedRiverCapacity) terminal(state string) float64 {
	snapshot := c.current()
	switch state {
	case "cancelled":
		return snapshot.cancelled
	case "completed":
		return snapshot.completed
	case "discarded":
		return snapshot.discarded
	default:
		return 0
	}
}

func (c *cachedRiverCapacity) current() riverCapacitySnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.hasAttemptedQuery && time.Since(c.lastAttemptedAt) < c.ttl {
		return c.snapshot0
	}
	c.lastAttemptedAt = time.Now()
	c.hasAttemptedQuery = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cancelled, completed, discarded, tableBytes, indexBytes, cleanupOverdue int64
	var oldestAge float64
	err := c.queryer.QueryRow(ctx, riverCapacitySnapshotSQL, c.retentionMillis).Scan(
		&cancelled,
		&completed,
		&discarded,
		&oldestAge,
		&tableBytes,
		&indexBytes,
		&cleanupOverdue,
	)
	if err != nil {
		c.snapshot0.querySuccess = 0
		return c.snapshot0
	}
	c.snapshot0 = riverCapacitySnapshot{
		cancelled:      float64(cancelled),
		completed:      float64(completed),
		discarded:      float64(discarded),
		oldestAge:      oldestAge,
		tableBytes:     float64(tableBytes),
		indexBytes:     float64(indexBytes),
		cleanupOverdue: float64(cleanupOverdue),
		querySuccess:   1,
	}
	return c.snapshot0
}

// cachedQueuePending memoises the river_job available-count for ttl so the
// GaugeFunc does not query the DB on every scrape. Safe for concurrent
// scrapes via mu.
type cachedQueuePending struct {
	pool *pgxpool.Pool
	ttl  time.Duration

	mu        sync.Mutex
	value0    float64
	fetchedAt time.Time
	primed    bool
}

func (c *cachedQueuePending) value() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.primed && time.Since(c.fetchedAt) < c.ttl {
		return c.value0
	}

	// Bound the query so a stalled DB cannot wedge a scrape indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var n int64
	err := c.pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE state = 'available'`).Scan(&n)
	if err != nil {
		// Keep the last good value; if never primed, report 0 (no backlog
		// observed yet) without marking primed so the next scrape retries
		// rather than serving a stale zero for a full TTL.
		return c.value0
	}
	c.value0 = float64(n)
	c.fetchedAt = time.Now()
	c.primed = true
	return c.value0
}
