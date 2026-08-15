// link_backfill.go is the Phase 9 link content-vector backfill — the links
// counterpart of internal/concept/backfill.go. The parse pipeline writes a
// content vector when a link finishes analysis, but two classes of done links
// carry no current-model vector: links parsed before Phase 9 shipped, and
// links whose parse-time embedding call failed (fail-soft skips the write).
// Either way they miss the q= semantic leg. This optional repair implementation
// can scan done links whose vector is missing or stale and re-embed them under
// the current model. Runtime wires it into the shared River queue when the
// embedding capability is enabled.
//
// The design mirrors concept.Backfiller exactly so the two runners are
// structurally symmetric (the DEV-PLAN's "并列一个 link 专用 runner"):
//
//   - Disabled when no embedding model is configured (Embedder.Enabled() ==
//     false) — nothing to embed against, and the q= path degrades to ILIKE.
//   - Runs once per durable maintenance job; River periodically invokes the job
//     and retries hard scan failures.
//   - Idempotent + resumable via an ascending id cursor. The scan predicate
//     (embedding IS NULL OR embedding_model IS DISTINCT FROM current, status =
//     'done') self-excludes already-filled rows, so a crash-and-restart
//     re-scans from the start but re-embeds only what is still missing.
//   - Embeds the same title+summary+body input the parse-time write uses
//     (buildContentEmbeddingInput) so a back-filled vector is interchangeable.
//   - Fail-soft per batch: an embedding/write failure is logged at WARN and
//     skipped; the cursor advances so one bad batch never strands the rest.
//   - Rate-limited between batches (default 500ms) and fully cancellable.
package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"webtag/internal/observability"
	"webtag/internal/repository"
)

const (
	defaultLinkBackfillBatchSize     = 64
	defaultLinkBackfillBatchInterval = 500 * time.Millisecond
)

// LinkBackfillStore is the narrow persistence surface the link backfill runner
// needs. *repository.PGXLinkRepository satisfies it; the narrow interface keeps
// the runner unit-testable without a database.
type LinkBackfillStore interface {
	// ListLinksNeedingEmbedding returns up to limit done links whose content
	// vector is missing or stale, in ascending id order strictly after afterID.
	// An empty slice signals the scan is drained.
	ListLinksNeedingEmbedding(ctx context.Context, currentModel string, afterID uuid.UUID, limit int) ([]repository.LinkBackfillCandidate, error)
	// UpdateLinkEmbedding writes a freshly-computed content vector + model onto
	// the links row only while the metadata tuple used for embedding remains
	// current. applied is false when a stale candidate lost that CAS; this is
	// expected and the candidate is retried from fresh metadata on a later run.
	UpdateLinkEmbedding(ctx context.Context, id uuid.UUID, expectedMetadataRevision int64, embedding []float32, embeddingModel string) (applied bool, err error)
}

// LinkBackfillOptions wires the runner. Store and Embedder are required; the
// runner no-ops when Embedder is nil or disabled. BatchSize / BatchInterval
// fall back to the Spec defaults (64 / 500ms) when non-positive. Logger may be
// nil (log calls degrade to no-ops).
type LinkBackfillOptions struct {
	Store         LinkBackfillStore
	Embedder      RetrievalEmbedder
	BatchSize     int
	BatchInterval time.Duration
	Logger        *slog.Logger
}

// LinkBackfiller fills missing / stale link content vectors when explicitly
// invoked. Run is a no-op when the embedder is disabled.
type LinkBackfiller struct {
	store         LinkBackfillStore
	embedder      RetrievalEmbedder
	batchSize     int
	batchInterval time.Duration
	logger        *slog.Logger
}

// NewLinkBackfiller constructs a LinkBackfiller, substituting the Spec defaults
// for any non-positive batch knob.
func NewLinkBackfiller(opts LinkBackfillOptions) *LinkBackfiller {
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultLinkBackfillBatchSize
	}
	interval := opts.BatchInterval
	if interval <= 0 {
		interval = defaultLinkBackfillBatchInterval
	}
	return &LinkBackfiller{
		store:         opts.Store,
		embedder:      opts.Embedder,
		batchSize:     batchSize,
		batchInterval: interval,
		logger:        opts.Logger,
	}
}

// Run scans for done links missing a current-model content vector and re-embeds
// them batch by batch. Returns when the scan drains, the context is cancelled,
// or the store scan errors hard. Per-batch embedding/write failures are logged
// and skipped (fail-soft). A metadata-CAS miss is a stale skip, rather than a
// failed write: it consumed a candidate whose source tuple changed while the
// vector was being computed. Returns filled / failed / skipped counts; the
// cancellation error is returned only when the run was interrupted.
func (b *LinkBackfiller) Run(ctx context.Context) (filled int, failed int, skipped int, err error) {
	if b == nil || b.store == nil || b.embedder == nil || !b.embedder.Enabled() {
		return 0, 0, 0, nil
	}
	model := b.embedder.Model()
	cursor := uuid.Nil

	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return filled, failed, skipped, ctxErr
		}

		batch, scanErr := b.store.ListLinksNeedingEmbedding(ctx, model, cursor, b.batchSize)
		if scanErr != nil {
			return filled, failed, skipped, scanErr
		}
		if len(batch) == 0 {
			return filled, failed, skipped, nil // drained
		}

		batchFilled, batchFailed, batchSkipped := b.processBatch(ctx, batch, model)
		filled += batchFilled
		failed += batchFailed
		skipped += batchSkipped
		cursor = batch[len(batch)-1].ID

		if sleepErr := linkBackfillSleep(ctx, b.batchInterval); sleepErr != nil {
			return filled, failed, skipped, sleepErr
		}
	}
}

// processBatch embeds one batch's content inputs and writes the vectors back.
// The whole batch is embedded in a single Embed call to amortize the round
// trip. A candidate whose embedding input is blank (no title/summary/body) is
// skipped without an upstream call. An embedding failure fails the batch soft
// (every row counts as failed, left for the next start); a per-row write
// failure fails just that row.
func (b *LinkBackfiller) processBatch(ctx context.Context, batch []repository.LinkBackfillCandidate, model string) (filled int, failed int, skipped int) {
	texts := make([]string, 0, len(batch))
	usable := make([]repository.LinkBackfillCandidate, 0, len(batch))
	for _, c := range batch {
		text := buildContentEmbeddingInput(deref(c.Title), deref(c.Summary), deref(c.InputText))
		if strings.TrimSpace(text) == "" {
			// No usable text to embed — count as failed so the row is re-tried
			// on a future start (in case input_text gets backfilled), but never
			// make an empty upstream call.
			failed++
			continue
		}
		texts = append(texts, text)
		usable = append(usable, c)
	}
	if len(usable) == 0 {
		return 0, failed, 0
	}

	vectors, err := b.embedder.Embed(ctx, texts)
	if err != nil || len(vectors) != len(usable) {
		b.logf("warn", "link backfill batch embedding failed; skipping batch",
			"batch_size", len(usable), "err", observability.SafeError(err))
		return 0, failed + len(usable), 0
	}

	for i, c := range usable {
		if len(vectors[i]) == 0 {
			b.logf("warn", "link backfill skipping link with empty embedding vector", "link_id", c.ID)
			failed++
			continue
		}
		applied, err := b.store.UpdateLinkEmbedding(ctx, c.ID, c.MetadataRevision, vectors[i], model)
		if err != nil {
			b.logf("warn", "link backfill embedding write failed; leaving link for next run",
				"link_id", c.ID, "err", observability.SafeError(err))
			failed++
			continue
		}
		if !applied {
			b.logf("info", "link backfill skipped stale metadata revision", "link_id", c.ID)
			skipped++
			continue
		}
		filled++
	}
	return filled, failed, skipped
}

// linkBackfillSleep sleeps for d but returns early (with the context error) if
// ctx is cancelled first, so the rate-limit pause is interruptible by graceful
// shutdown.
func linkBackfillSleep(ctx context.Context, d time.Duration) error {
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

func (b *LinkBackfiller) logf(level, msg string, args ...any) {
	if b == nil || b.logger == nil {
		return
	}
	if level == "warn" {
		b.logger.Warn(msg, args...)
		return
	}
	b.logger.Info(msg, args...)
}

// LinkBackfillErrIsCancel reports whether err is a context cancellation /
// deadline, so the app-level runner can distinguish a clean shutdown interrupt
// from a real failure when logging the summary.
func LinkBackfillErrIsCancel(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
