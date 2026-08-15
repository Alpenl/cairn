package repository

import (
	"context"
	"fmt"
	"strings"
	"webtag/internal/alloc"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"webtag/internal/concept"
)

// Compile-time guard: PGXConceptRepository satisfies the backfill store
// surface so the Phase 8 backfill runner can scan + update vectors.
var _ concept.BackfillStore = (*PGXConceptRepository)(nil)

// ListConceptsNeedingEmbedding returns up to `limit` concepts whose vector
// is missing OR was embedded by a model other than `currentModel`, scanned
// in ascending id order starting strictly after `afterID`. Passing
// uuid.Nil for afterID starts from the beginning (all-zero UUID sorts
// first). The id cursor makes the scan resumable and idempotent: a row
// re-embedded under the current model on a previous pass no longer matches
// the WHERE clause, so a process restart re-scans from the start but skips
// already-filled rows for free.
//
// Each returned row carries the text the backfill embeds — primary_name,
// the same surface form the v3 vector gate embeds when it admits a new
// concept (gate.go: Embed([]string{surfaceTag}) → CreateConcept with
// PrimaryName == surfaceTag). Keeping the backfill text identical to the
// gate text means a back-filled vector lands in the same place a freshly
// gated one would, so nearest-neighbour comparisons stay coherent across
// the two creation paths.
func (r *PGXConceptRepository) ListConceptsNeedingEmbedding(ctx context.Context, currentModel string, afterID uuid.UUID, limit int) ([]concept.BackfillCandidate, error) {
	currentModel = strings.TrimSpace(currentModel)
	if currentModel == "" {
		return nil, fmt.Errorf("list concepts needing embedding: empty current model")
	}
	if limit < 1 {
		limit = 1
	}
	rows, err := r.db.Query(ctx,
		`SELECT id, primary_name
		 FROM concept
		 WHERE id > $1
		   AND (embedding IS NULL OR embedding_model IS DISTINCT FROM $2)
		 ORDER BY id
		 LIMIT $3`,
		afterID, currentModel, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list concepts needing embedding: %w", err)
	}
	defer rows.Close()

	out := make([]concept.BackfillCandidate, 0, alloc.Hint(limit))
	for rows.Next() {
		var c concept.BackfillCandidate
		if err := rows.Scan(&c.ID, &c.PrimaryName); err != nil {
			return nil, fmt.Errorf("scan backfill candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backfill candidates: %w", err)
	}
	return out, nil
}

// UpdateConceptEmbedding writes the freshly-computed vector + model onto a
// concept row. The two columns move in lockstep — a vector without its
// model name is unusable for the model-scoped nearest-neighbour query — so
// both are set in one statement. updated_at is bumped so the change is
// observable. An empty vector or model is rejected rather than silently
// NULL-ing the columns, since the caller only invokes this with a valid
// embedding from a successful Embed call.
func (r *PGXConceptRepository) UpdateConceptEmbedding(ctx context.Context, id uuid.UUID, embedding []float32, embeddingModel string) error {
	embeddingModel = strings.TrimSpace(embeddingModel)
	if id == uuid.Nil {
		return fmt.Errorf("update concept embedding: nil id")
	}
	if len(embedding) == 0 || embeddingModel == "" {
		return fmt.Errorf("update concept embedding: empty embedding or model")
	}
	_, err := r.db.Exec(ctx,
		`UPDATE concept
		 SET embedding = $2, embedding_model = $3, updated_at = now()
		 WHERE id = $1`,
		id, pgvector.NewVector(embedding), embeddingModel,
	)
	if err != nil {
		return fmt.Errorf("update concept embedding %v: %w", id, err)
	}
	return nil
}
