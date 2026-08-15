package app

import (
	"context"
	"sync"
	"time"

	"webtag/internal/observability"
)

// pendingProposalCounter is the slice of the proposal store the gauge
// reads. PGXConceptProposalRepository satisfies it.
type pendingProposalCounter interface {
	CountPendingProposals(ctx context.Context) (int, error)
}

// pendingProposalsGaugeTTL bounds how often the gauge hits the database.
// Prometheus scrapes can arrive every few seconds and from multiple
// replicas; without a cache each scrape would fire a COUNT(*) against
// concept_merge_proposal. 60s is well inside any reasonable backlog
// alert window (the queue moves at human review cadence) while keeping
// the DB load negligible.
const pendingProposalsGaugeTTL = 60 * time.Second

// registerPendingProposalsGauge wires the concept_merge_proposals_pending
// GaugeFunc. The value func is invoked on every Prometheus scrape, so it
// must be cheap: it serves a cached count and only re-queries the store
// when the cached value is older than pendingProposalsGaugeTTL. A query
// error keeps the last known value (and logs nothing — a transient DB
// blip should not spam logs on every scrape) so the gauge degrades to
// stale-but-present rather than disappearing.
//
// counter==nil is a no-op so opt-out / test wiring without a proposal
// store does not register a gauge that would always read zero.
func registerPendingProposalsGauge(metrics *observability.Metrics, counter pendingProposalCounter) {
	if metrics == nil || counter == nil {
		return
	}
	c := &cachedProposalCount{counter: counter, ttl: pendingProposalsGaugeTTL}
	metrics.RegisterGaugeFunc(
		"concept",
		"merge_proposals_pending",
		"Number of pending concept merge proposals awaiting human review (cached, refreshed at most every 60s). v3's vector-gate ambiguous band is the sole producer; a rising value means review is falling behind.",
		c.value,
	)
}

// cachedProposalCount memoises the pending-proposal count for ttl so the
// GaugeFunc does not query the DB on every scrape. Safe for concurrent
// scrapes via mu.
type cachedProposalCount struct {
	counter pendingProposalCounter
	ttl     time.Duration

	mu        sync.Mutex
	value0    float64
	fetchedAt time.Time
	primed    bool
}

func (c *cachedProposalCount) value() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.primed && time.Since(c.fetchedAt) < c.ttl {
		return c.value0
	}

	// Bound the query so a stalled DB cannot wedge a scrape indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	n, err := c.counter.CountPendingProposals(ctx)
	if err != nil {
		// Keep the last good value; if we never primed, report 0 (no
		// backlog observed yet) without marking primed so the next scrape
		// retries the query rather than serving a stale zero for a full TTL.
		return c.value0
	}
	c.value0 = float64(n)
	c.fetchedAt = time.Now()
	c.primed = true
	return c.value0
}
