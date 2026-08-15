// Package concept — Phase 8 embedding backfill.
//
// The v3 vector gate (gate.go) degrades to a vector-less local create
// whenever the embedding service is unavailable at tag-resolution time, and
// legacy concepts created before the v3 migration carry no vector at all.
// Either way the concept misses the model-scoped nearest-neighbour query and
// can never auto-merge or recall a candidate. This optional repair task can
// scan concepts whose vector is missing or stale and re-embed them under the
// current model. The default Runtime does not construct or run it.
//
// Design:
//
//   - Disabled when no embedding model is configured (Embedder.Enabled() ==
//     false) — there is nothing to embed against, and the retrieval path is
//     off anyway.
//
//   - Runs once when explicitly invoked. It is not a ticker loop: once the
//     scan drains, Run returns.
//
//   - Idempotent + resumable via an ascending id cursor. The scan query
//     (`embedding IS NULL OR embedding_model <> current`) self-excludes rows
//     already filled under the current model, so a crash-and-restart re-scans
//     from the start but re-embeds only what is still missing. No checkpoint
//     table needed.
//
//   - Embeds primary_name — the same surface form the vector gate embeds when
//     it admits a new concept — so a back-filled vector is interchangeable
//     with a gate-produced one in nearest-neighbour space.
//
//   - Fail-soft per batch: a batch whose embedding call or write fails is
//     logged at WARN and skipped; the runner advances the cursor past it and
//     keeps going so one bad batch never strands the rest. A final INFO line
//     summarizes filled / failed counts.
//
//   - Rate-limited between batches (default 500ms) so a large backfill does
//     not saturate the embedding upstream, and fully cancellable: a graceful
//     shutdown cancels the context mid-flight and the runner stops at the next
//     batch boundary (or interruptible sleep).
package concept

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// BackfillCandidate is one concept row the backfill scan returns: its id (the
// cursor + update key) and primary_name (the text to embed).
type BackfillCandidate struct {
	ID          uuid.UUID
	PrimaryName string
}

// BackfillStore is the narrow persistence surface the backfill runner needs.
// PGXConceptRepository satisfies it; the narrow interface keeps the runner
// unit-testable without a database and free of the resolver's write surface.
type BackfillStore interface {
	// ListConceptsNeedingEmbedding returns up to limit concepts whose vector
	// is missing or stale (embedding_model != currentModel), in ascending id
	// order strictly after afterID. An empty slice signals the scan is drained.
	ListConceptsNeedingEmbedding(ctx context.Context, currentModel string, afterID uuid.UUID, limit int) ([]BackfillCandidate, error)
	// UpdateConceptEmbedding writes a freshly-computed vector + model onto the
	// concept row.
	UpdateConceptEmbedding(ctx context.Context, id uuid.UUID, embedding []float32, embeddingModel string) error
}

const (
	defaultBackfillBatchSize     = 64
	defaultBackfillBatchInterval = 500 * time.Millisecond
)

// BackfillOptions wires the backfill runner. Store and Embedder are required;
// the runner no-ops when Embedder is nil or disabled. BatchSize / BatchInterval
// fall back to the Spec defaults (64 / 500ms) when non-positive. Logger may be
// nil (log calls degrade to no-ops).
type BackfillOptions struct {
	Store         BackfillStore
	Embedder      Embedder
	BatchSize     int
	BatchInterval time.Duration
	Logger        *slog.Logger
}

// Backfiller fills missing / stale concept vectors when explicitly invoked.
// Run is a no-op when the embedder is disabled.
type Backfiller struct {
	store         BackfillStore
	embedder      Embedder
	batchSize     int
	batchInterval time.Duration
	logger        *slog.Logger
}

// NewBackfiller constructs a Backfiller, substituting the Spec defaults for any
// non-positive batch knob.
func NewBackfiller(opts BackfillOptions) *Backfiller {
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBackfillBatchSize
	}
	interval := opts.BatchInterval
	if interval <= 0 {
		interval = defaultBackfillBatchInterval
	}
	return &Backfiller{
		store:         opts.Store,
		embedder:      opts.Embedder,
		batchSize:     batchSize,
		batchInterval: interval,
		logger:        opts.Logger,
	}
}

// Run scans for concepts missing a current-model vector and re-embeds them
// batch by batch. It returns when the scan drains, the context is cancelled,
// or the store scan errors hard. Per-batch embedding / write failures are
// logged and skipped (fail-soft) — they never abort the run. Returns the
// number of concepts filled and the number that failed (sum of every skipped
// batch's size); the context-cancellation error is returned only when the run
// was interrupted, never wrapped over a successful drain.
func (b *Backfiller) Run(ctx context.Context) (filled int, failed int, err error) {
	if b == nil || b.store == nil || b.embedder == nil || !b.embedder.Enabled() {
		return 0, 0, nil
	}
	model := b.embedder.Model()
	cursor := uuid.Nil

	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return filled, failed, ctxErr
		}

		batch, scanErr := b.store.ListConceptsNeedingEmbedding(ctx, model, cursor, b.batchSize)
		if scanErr != nil {
			// A hard scan error (DB down, query error) is not recoverable by
			// advancing the cursor — stop and surface it. The caller logs the
			// summary it has so far.
			return filled, failed, scanErr
		}
		if len(batch) == 0 {
			return filled, failed, nil // drained
		}

		batchFilled, batchFailed := b.processBatch(ctx, batch, model)
		filled += batchFilled
		failed += batchFailed
		// Advance the cursor past the whole batch regardless of per-row
		// outcome: a failed row stays missing and will be re-scanned on the
		// next process start, but skipping it now keeps the cursor monotonic
		// so the scan terminates.
		cursor = batch[len(batch)-1].ID

		// Rate-limit between batches; an interruptible sleep so shutdown does
		// not wait out the full interval.
		if err := sleepCtx(ctx, b.batchInterval); err != nil {
			return filled, failed, err
		}
	}
}

// processBatch embeds one batch's primary_names and writes the vectors back.
// The whole batch is embedded in a single Embed call to amortize the round
// trip; an embedding failure fails the entire batch soft (every row counts as
// failed and is left for the next process start). A per-row write failure
// fails just that row.
func (b *Backfiller) processBatch(ctx context.Context, batch []BackfillCandidate, model string) (filled int, failed int) {
	texts := make([]string, len(batch))
	for i, c := range batch {
		texts[i] = c.PrimaryName
	}

	vectors, err := b.embedder.Embed(ctx, texts)
	if err != nil || len(vectors) != len(batch) {
		b.logf("warn", "backfill batch embedding failed; skipping batch",
			"batch_size", len(batch), "err", errString(err))
		return 0, len(batch)
	}

	for i, c := range batch {
		if len(vectors[i]) == 0 {
			b.logf("warn", "backfill skipping concept with empty embedding vector",
				"concept_id", c.ID, "primary_name", c.PrimaryName)
			failed++
			continue
		}
		if err := b.store.UpdateConceptEmbedding(ctx, c.ID, vectors[i], model); err != nil {
			b.logf("warn", "backfill embedding write failed; leaving concept for next run",
				"concept_id", c.ID, "primary_name", c.PrimaryName, "err", err.Error())
			failed++
			continue
		}
		filled++
	}
	return filled, failed
}

// sleepCtx sleeps for d but returns early (with the context error) if ctx is
// cancelled first, so the backfill rate-limit pause is interruptible by
// graceful shutdown.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (b *Backfiller) logf(level, msg string, args ...any) {
	if b == nil || b.logger == nil {
		return
	}
	switch level {
	case "warn":
		b.logger.Warn(msg, args...)
	default:
		b.logger.Info(msg, args...)
	}
}

// ErrIsCancel reports whether err is a context cancellation / deadline, so the
// app-level runner can distinguish a clean shutdown interrupt from a real
// failure when logging the summary.
func ErrIsCancel(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
