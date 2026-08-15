package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"webtag/internal/concept"
)

// IsUniqueViolation reports whether err is a Postgres unique_violation
// (SQLSTATE 23505). Exposed so resolver code can branch on "someone
// raced me to insert the same concept" without sniffing strings.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// RecalculateDisplayName picks the most-used surface_tag for the
// concept and writes it to concept.display_name. Called after every
// AttachLinkConcept (best-effort: a failure logs+continues) and
// inside the merge tx after the loser's surfaces have been
// re-pointed at the winner.
//
// The mode() WITHIN GROUP variant is shorter but pg_trgm
// installations sometimes lag the version that ships it; the
// GROUP BY + ORDER BY count form works on every PG 10+. Tie-break
// is lexicographic surface ASC so two concurrent recalcs produce
// the same value regardless of ordering.
func (r *PGXConceptRepository) RecalculateDisplayName(ctx context.Context, conceptID uuid.UUID) error {
	if conceptID == uuid.Nil {
		return nil
	}
	_, err := r.db.Exec(ctx,
		`UPDATE concept
		 SET display_name = (
		   SELECT surface_tag FROM link_concept
		   WHERE concept_id = $1
		   GROUP BY surface_tag
		   ORDER BY count(*) DESC, surface_tag ASC
		   LIMIT 1
		 ),
		 updated_at = now()
		 WHERE id = $1`,
		conceptID,
	)
	if err != nil {
		return fmt.Errorf("recalc display_name %s: %w", conceptID, err)
	}
	return nil
}

// ListDisplayNamesByLinkIDs returns, for each requested link, the
// list of concept display names attached to it (one per attached
// concept). COALESCE falls back to primary_name when display_name
// has not been recalculated yet — a brand-new concept's first
// resolution is the typical case.
//
// Output keys are link IDs that actually have at least one concept
// attached; links with zero attachments are absent from the map (the
// LinkReadService treats absence as "use the original link.tags
// column" so single-tag legacy rows keep displaying).
//
// Ordering inside each slice is by surface_tag for stability —
// reordering the chip set on each request is bad UX. The
// ListLinkConceptsForLinks-style query plan: index on
// link_concept(link_id) gives a tight index scan + nested loop into
// concept.id; for typical page size ~100 links * 3 concepts each the
// total work is ~300 row fetches.
func (r *PGXConceptRepository) ListDisplayNamesByLinkIDs(ctx context.Context, linkIDs []uuid.UUID) (map[uuid.UUID][]string, error) {
	if len(linkIDs) == 0 {
		return map[uuid.UUID][]string{}, nil
	}
	rows, err := r.db.Query(ctx,
		`SELECT lc.link_id, COALESCE(c.display_name, c.primary_name)
		 FROM link_concept lc
		 JOIN concept c ON c.id = lc.concept_id
		 WHERE lc.link_id = ANY($1)
		 ORDER BY lc.link_id, COALESCE(c.display_name, c.primary_name)`,
		linkIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("list display names: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID][]string, len(linkIDs))
	for rows.Next() {
		var lid uuid.UUID
		var name string
		if err := rows.Scan(&lid, &name); err != nil {
			return nil, fmt.Errorf("scan display name row: %w", err)
		}
		out[lid] = append(out[lid], name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate display name rows: %w", err)
	}
	return out, nil
}

// ConceptExportRow is one canonical vocabulary row streamed by
// GET /api/export/concepts. Aliases contains every surface form registered
// for the concept in this installation.
type ConceptExportRow struct {
	ID          uuid.UUID
	PrimaryName string
	DisplayName string // COALESCE(display_name, primary_name)
	Aliases     []string
	UseCount    int
	CreatedAt   time.Time
}

// conceptExportBatch matches the link export's batch size so each round trip
// and alias aggregation stays bounded.
const conceptExportBatch = 500

// StreamConcepts scans the installation vocabulary by a stable
// (created_at, id) keyset and streams each concept with its aliases. yield
// errors stop the stream immediately. The LEFT JOIN aggregation avoids N+1
// reads without loading the whole vocabulary into memory.
func (r *PGXConceptRepository) StreamConcepts(ctx context.Context, yield func(ConceptExportRow) error) error {
	var (
		afterCreated *time.Time
		afterID      *uuid.UUID
	)
	for {
		rows, err := r.queryConceptPage(ctx, afterCreated, afterID)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for i := range rows {
			if err := yield(rows[i]); err != nil {
				return err
			}
		}
		if len(rows) < conceptExportBatch {
			return nil
		}
		last := rows[len(rows)-1]
		afterCreated = &last.CreatedAt
		afterID = &last.ID
	}
}

// queryConceptPage reads one page. The strict ascending keyset comparison
// prevents duplicates and gaps across page boundaries.
func (r *PGXConceptRepository) queryConceptPage(ctx context.Context, afterCreated *time.Time, afterID *uuid.UUID) ([]ConceptExportRow, error) {
	const base = `SELECT c.id, c.primary_name, COALESCE(c.display_name, c.primary_name) AS display_name,
		c.use_count, c.created_at,
		COALESCE(array_remove(array_agg(a.alias ORDER BY a.alias), NULL), '{}') AS aliases
		FROM concept c
		LEFT JOIN concept_alias a ON a.concept_id = c.id`
	var (
		query string
		args  []any
	)
	if afterCreated != nil && afterID != nil {
		query = base + ` WHERE (c.created_at, c.id) > ($1, $2)
			GROUP BY c.id ORDER BY c.created_at ASC, c.id ASC LIMIT $3`
		args = []any{*afterCreated, *afterID, conceptExportBatch}
	} else {
		query = base + ` GROUP BY c.id ORDER BY c.created_at ASC, c.id ASC LIMIT $1`
		args = []any{conceptExportBatch}
	}
	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list concepts for export: %w", err)
	}
	defer rows.Close()

	out := make([]ConceptExportRow, 0, conceptExportBatch)
	for rows.Next() {
		var row ConceptExportRow
		if err := rows.Scan(&row.ID, &row.PrimaryName, &row.DisplayName, &row.UseCount, &row.CreatedAt, &row.Aliases); err != nil {
			return nil, fmt.Errorf("scan concept export row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate concept export rows: %w", err)
	}
	return out, nil
}

// GetConcept fetches one concept row by ID. Returns (nil, nil) on
// not-found so the admin merge view can distinguish "concept gone"
// (race: another merge already absorbed it) from "DB error" and
// degrade to an empty label rather than 500ing the whole list.
func (r *PGXConceptRepository) GetConcept(ctx context.Context, id uuid.UUID) (*concept.Concept, error) {
	if id == uuid.Nil {
		return nil, nil
	}
	var c concept.Concept
	var qid *string
	err := r.db.QueryRow(ctx,
		`SELECT id, primary_name, wikidata_qid, use_count, created_at, updated_at
		 FROM concept WHERE id = $1`,
		id,
	).Scan(&c.ID, &c.PrimaryName, &qid, &c.UseCount, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get concept %s: %w", id, err)
	}
	if qid != nil {
		c.WikidataQID = *qid
	}
	return &c, nil
}
