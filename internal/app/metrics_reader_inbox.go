package app

import (
	"context"
	"sync"
	"time"

	"webtag/internal/observability"
	"webtag/internal/service"
)

const readerInboxOrphanBacklogGaugeTTL = 60 * time.Second

type readerInboxOrphanCounter interface {
	CountInboxDispatchOrphans(context.Context, string) (int64, error)
}

func registerReaderInboxOrphanBacklogGauge(metrics *observability.Metrics, counter readerInboxOrphanCounter) {
	if metrics == nil || counter == nil {
		return
	}
	cached := &cachedReaderInboxOrphanBacklog{counter: counter, ttl: readerInboxOrphanBacklogGaugeTTL}
	metrics.RegisterGaugeFunc(
		"reader_inbox_dispatch",
		"orphan_backlog",
		"Number of active Inbox proposal attempts without an exact active River job.",
		cached.Value,
	)
}

type cachedReaderInboxOrphanBacklog struct {
	counter readerInboxOrphanCounter
	ttl     time.Duration

	mu     sync.Mutex
	value  float64
	at     time.Time
	primed bool
}

func (c *cachedReaderInboxOrphanBacklog) Value() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.primed && time.Since(c.at) < c.ttl {
		return c.value
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	count, err := c.counter.CountInboxDispatchOrphans(ctx, service.ReaderInboxSummaryJobKind)
	if err != nil {
		return c.value
	}
	c.value, c.at, c.primed = float64(count), time.Now(), true
	return c.value
}
