package repository

import (
	"context"
	"fmt"
	"strings"

	sqldb "database/sql"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pgvector/pgvector-go"

	"webtag/internal/alloc"
)

const updateLinkEmbeddingForParseSQL = `UPDATE links l
	 SET embedding = $3, embedding_model = $4
	 WHERE l.id = $1
	   AND l.deleted_at IS NULL
	   AND l.status = 'done'
	   AND l.title IS NOT DISTINCT FROM $5
	   AND l.summary IS NOT DISTINCT FROM $6
	   AND EXISTS (
	       SELECT 1
	       FROM parse_jobs p
	       WHERE p.id = $2
	         AND p.link_id = l.id
	         AND p.status = 'done'
	         AND p.id = (
	             SELECT latest.id
	             FROM parse_jobs latest
	             WHERE latest.link_id = l.id
	             ORDER BY latest.created_at DESC, latest.id DESC
	             LIMIT 1
	         )
	   )`

// LinkBackfillCandidate is one links row the Phase 9 content-vector backfill
// scan returns: its id (the cursor + update key) plus the three text columns
// the embedding input is built from (title + summary + the stored body in
// input_text). The runner truncates input_text to the body rune budget and
// joins the parts — the same input shape persistAnalysisResult uses at parse
// time, so a back-filled vector lands in the same place a freshly-parsed one
// would.
//
// Title / Summary / InputText are pointers because all three are nullable on
// the links row; the runner treats a nil/blank part as "absent" when joining.
type LinkBackfillCandidate struct {
	ID               uuid.UUID
	Title            *string
	Summary          *string
	InputText        *string
	MetadataRevision int64
}

// ListLinksNeedingEmbedding returns up to `limit` done links whose content
// vector is missing OR was embedded by a model other than `currentModel`,
// scanned in ascending id order strictly after `afterID`. Mirrors
// ListConceptsNeedingEmbedding exactly (id cursor → resumable + idempotent: a
// row re-embedded under the current model no longer matches the predicate, so
// a restart re-scans from the start but skips already-filled rows for free).
//
// Only status='done' rows participate: pending/processing/failed/skeleton rows
// have no finished analysis to embed, and the q= search only ever queries done
// rows, so embedding the rest would be wasted upstream cost.
func (r *PGXLinkRepository) ListLinksNeedingEmbedding(ctx context.Context, currentModel string, afterID uuid.UUID, limit int) ([]LinkBackfillCandidate, error) {
	currentModel = strings.TrimSpace(currentModel)
	if currentModel == "" {
		return nil, fmt.Errorf("list links needing embedding: empty current model")
	}
	if limit < 1 {
		limit = 1
	}
	rows, err := r.db.Query(ctx,
		`SELECT id, title, summary, input_text, metadata_revision
			 FROM links
			 WHERE deleted_at IS NULL
			   AND id > $1
			   AND status = 'done'
			   AND (embedding IS NULL OR embedding_model IS DISTINCT FROM $2)
			 ORDER BY id
			 LIMIT $3`,
		afterID, currentModel, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list links needing embedding: %w", err)
	}
	defer rows.Close()

	out := make([]LinkBackfillCandidate, 0, alloc.Hint(limit))
	for rows.Next() {
		var (
			c       LinkBackfillCandidate
			title   pgtype.Text
			summary pgtype.Text
			body    sqldb.NullString
		)
		if err := rows.Scan(&c.ID, &title, &summary, &body, &c.MetadataRevision); err != nil {
			return nil, fmt.Errorf("scan link backfill candidate: %w", err)
		}
		c.Title = textPointer(title)
		c.Summary = textPointer(summary)
		if body.Valid {
			s := body.String
			c.InputText = &s
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate link backfill candidates: %w", err)
	}
	return out, nil
}

// UpdateLinkEmbedding writes a freshly-computed content vector + model onto a
// installation-owned links row only while the caller's metadata revision still
// matches. The two columns move in lockstep (a vector without its model name
// is unusable for the model-scoped nearest-neighbour query), so both are set
// in one statement. applied reports whether that UPDATE affected its row. A
// false result with nil error is the expected stale-revision no-op: the
// metadata trigger already cleared the vector and a later scan will rebuild
// it from the current text. updated_at is deliberately NOT bumped: the
// content vector is a derived artifact, not a user-visible change, and
// bumping updated_at would reshuffle the (created_at, id) list ordering /
// cursor stream for no reason. An empty vector or model is rejected rather
// than silently NULL-ing the columns.
func (r *PGXLinkRepository) UpdateLinkEmbedding(ctx context.Context, id uuid.UUID, expectedMetadataRevision int64, embedding []float32, embeddingModel string) (bool, error) {
	embeddingModel = strings.TrimSpace(embeddingModel)
	if id == uuid.Nil {
		return false, fmt.Errorf("update link embedding: nil id")
	}
	if expectedMetadataRevision < 1 {
		return false, fmt.Errorf("update link embedding: invalid metadata revision")
	}
	if len(embedding) == 0 || embeddingModel == "" {
		return false, fmt.Errorf("update link embedding: empty embedding or model")
	}
	commandTag, err := r.db.Exec(ctx,
		`UPDATE links
			 SET embedding = $2, embedding_model = $3
			 WHERE id = $1 AND deleted_at IS NULL
			   AND metadata_revision = $4`,
		id, pgvector.NewVector(embedding), embeddingModel, expectedMetadataRevision,
	)
	if err != nil {
		return false, fmt.Errorf("update link embedding %v: %w", id, err)
	}
	return commandTag.RowsAffected() == 1, nil
}

// UpdateLinkEmbeddingForParse writes a vector only when parseJobID is still
// the latest completed attempt for this link and the link remains done.
// A zero-row update is an expected stale-attempt no-op, not an error.
func (r *PGXLinkRepository) UpdateLinkEmbeddingForParse(
	ctx context.Context,
	id, parseJobID uuid.UUID,
	expectedTitle, expectedSummary *string,
	embedding []float32,
	embeddingModel string,
) error {
	embeddingModel = strings.TrimSpace(embeddingModel)
	if id == uuid.Nil || parseJobID == uuid.Nil {
		return fmt.Errorf("update parse link embedding: nil link or parse job id")
	}
	if len(embedding) == 0 || embeddingModel == "" {
		return fmt.Errorf("update parse link embedding: empty embedding or model")
	}
	_, err := r.db.Exec(ctx,
		updateLinkEmbeddingForParseSQL,
		id, parseJobID, pgvector.NewVector(embedding), embeddingModel, expectedTitle, expectedSummary,
	)
	if err != nil {
		return fmt.Errorf("update parse link embedding %v for job %v: %w", id, parseJobID, err)
	}
	return nil
}
