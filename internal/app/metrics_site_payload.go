package app

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/observability"
)

const (
	sitePayloadBacklogGaugeTTL   = 60 * time.Second
	sitePayloadBacklogScope      = "discover"
	sitePayloadBacklogCountQuery = `SELECT count(*) FROM links WHERE payload_purge_due_at <= NOW() AND payload_purged_at IS NULL`
)

// registerSitePayloadCleanupBacklogGauge exposes overdue site capture payloads
// only as an aggregate. It deliberately never selects URL, content, metadata,
// or row identifiers, so the metrics path remains outside the payload data
// boundary while still providing the required alert signal.
func registerSitePayloadCleanupBacklogGauge(metrics *observability.Metrics, pool *pgxpool.Pool) {
	if metrics == nil || pool == nil {
		return
	}
	counter := &cachedSitePayloadBacklog{pool: pool, ttl: sitePayloadBacklogGaugeTTL}
	metrics.RegisterGaugeFunc("site", "payload_cleanup_backlog", "Number of expired site capture payloads awaiting cleanup.", counter.Value)
}

type cachedSitePayloadBacklog struct {
	pool *pgxpool.Pool
	ttl  time.Duration

	mu     sync.Mutex
	value  float64
	at     time.Time
	primed bool
}

func (c *cachedSitePayloadBacklog) Value() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.primed && time.Since(c.at) < c.ttl {
		return c.value
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return c.value
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "SET LOCAL app.site_payload_cleanup_scope = '"+sitePayloadBacklogScope+"'"); err != nil {
		return c.value
	}
	var count int
	if err = tx.QueryRow(ctx, sitePayloadBacklogCountQuery).Scan(&count); err != nil {
		return c.value
	}
	if err = tx.Commit(ctx); err != nil {
		return c.value
	}
	c.value, c.at, c.primed = float64(count), time.Now(), true
	return c.value
}
