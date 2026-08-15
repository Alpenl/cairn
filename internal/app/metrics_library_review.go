package app

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"webtag/internal/observability"
)

const libraryReviewGaugeTTL = 60 * time.Second

var libraryReviewMetricKinds = []string{"classification_uncertain", "migration_suggestion", "note_conflict", "merge_conflict"}
var libraryReviewMetricStatuses = []string{"pending", "applied", "dismissed"}

func registerLibraryReviewItemsGauge(metrics *observability.Metrics, pool *pgxpool.Pool) {
	if metrics == nil || pool == nil {
		return
	}
	counter := &cachedLibraryReviewCounts{pool: pool, ttl: libraryReviewGaugeTTL, values: map[string]float64{}}
	for _, kind := range libraryReviewMetricKinds {
		for _, status := range libraryReviewMetricStatuses {
			kind, status := kind, status
			metrics.RegisterConstGaugeFunc("library", "review_items", "Library review items partitioned by bounded kind and status.", prometheus.Labels{"kind": kind, "status": status}, func() float64 { return counter.Value(kind, status) })
		}
	}
}

type cachedLibraryReviewCounts struct {
	pool   *pgxpool.Pool
	ttl    time.Duration
	mu     sync.Mutex
	values map[string]float64
	at     time.Time
	primed bool
}

func (c *cachedLibraryReviewCounts) Value(kind, status string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.primed || time.Since(c.at) >= c.ttl {
		c.refreshLocked()
	}
	return c.values[kind+":"+status]
}

func (c *cachedLibraryReviewCounts) refreshLocked() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "SET LOCAL app.library_review_metrics_scope = 'metrics'"); err != nil {
		return
	}
	rows, err := tx.Query(ctx, `SELECT kind, status, count(*) FROM library_review_items GROUP BY kind, status`)
	if err != nil {
		return
	}
	values := map[string]float64{}
	for rows.Next() {
		var kind, status string
		var count int
		if rows.Scan(&kind, &status, &count) != nil {
			rows.Close()
			return
		}
		values[kind+":"+status] = float64(count)
	}
	if rows.Err() != nil {
		rows.Close()
		return
	}
	rows.Close()
	if tx.Commit(ctx) != nil {
		return
	}
	c.values, c.at, c.primed = values, time.Now(), true
}
