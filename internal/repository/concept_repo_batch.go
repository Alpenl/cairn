package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/concept"
)

// GetConceptIDsByAliases is the batch counterpart of GetConceptIDByAlias.
// Returns a map keyed by the lower-cased alias so callers can do a
// single SQL hop for an N-tag link instead of N. Aliases that miss are
// simply absent from the map — same convention as the singular form's
// (uuid.Nil, false, nil) return shape.
//
// Empty / whitespace-only aliases are filtered before the query so an
// "ARRAY[]" Postgres call is never issued; an empty input slice short-
// circuits to an empty map without touching the DB.
func (r *PGXConceptRepository) GetConceptIDsByAliases(ctx context.Context, aliases []string) (map[string]uuid.UUID, error) {
	if len(aliases) == 0 {
		return map[string]uuid.UUID{}, nil
	}
	normalized := make([]string, 0, len(aliases))
	seen := make(map[string]struct{}, len(aliases))
	for _, a := range aliases {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		normalized = append(normalized, a)
	}
	if len(normalized) == 0 {
		return map[string]uuid.UUID{}, nil
	}
	rows, err := r.db.Query(ctx,
		`SELECT alias, concept_id FROM concept_alias WHERE alias = ANY($1)`,
		normalized,
	)
	if err != nil {
		return nil, fmt.Errorf("batch alias lookup: %w", err)
	}
	defer rows.Close()
	out := make(map[string]uuid.UUID, len(normalized))
	for rows.Next() {
		var alias string
		var id uuid.UUID
		if err := rows.Scan(&alias, &id); err != nil {
			return nil, fmt.Errorf("scan batch alias row: %w", err)
		}
		out[alias] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate batch alias rows: %w", err)
	}
	return out, nil
}

// AttachLinkConceptsBatch writes (linkID, item.ConceptID, item.SurfaceTag)
// rows in a single multi-VALUES INSERT. Before it writes, the target Link row
// is locked and checked against the exact terminal parse metadata revision and
// tag tuple. That makes the delayed enrichment safe against a Reader CAS: if
// the save wins first, this returns attached=false without inserting stale
// edges; if this statement wins first, the later CAS waits for the lock and
// deletes these edges in its own atomic projection refresh.
//
// pgx supports parameterised slices via UNNEST which would be cleaner,
// but the column count is fixed at three so a hand-rolled VALUES list
// keeps the query plain SQL and avoids the slight per-row encoding
// overhead UNNEST incurs for very small batches (the common N=3..8
// case here).
//
// Each item adds 2 bound parameters (concept_id, surface_tag) on top
// of the shared $1 linkID, so the practical N ceiling before hitting
// Postgres's 65535-parameter limit is ~32k. The current caller
// (parse pipeline) caps tags at the analyzer's MaxTags (default 5),
// so we leave the limit implicit. Any future caller that wants to
// flush hundreds of items at once must shard the input before calling
// this method.
func (r *PGXConceptRepository) AttachLinkConceptsBatch(
	ctx context.Context,
	linkID uuid.UUID,
	expectedMetadataRevision int64,
	expectedTags []string,
	items []concept.AttachLinkConceptItem,
) (bool, error) {
	if linkID == uuid.Nil {
		return false, fmt.Errorf("attach link_concept batch: nil link id")
	}
	if expectedMetadataRevision <= 0 || len(items) == 0 {
		return false, nil
	}
	// Pre-allocate args at 2x len(items) plus the shared Link identity ($1),
	// exact metadata fence ($2/$3), and the per-item concept/surface pairs.
	// The placeholder string is built once with a
	// strings.Builder to avoid the quadratic concat path.
	args := make([]any, 0, 3+2*len(items))
	args = append(args, linkID, expectedMetadataRevision, expectedTags)
	var b strings.Builder
	b.Grow(320 + len(items)*16)
	b.WriteString(`WITH target AS MATERIALIZED (
			SELECT id
			FROM links
			WHERE id = $1 AND deleted_at IS NULL
				AND metadata_revision = $2
				AND tags IS NOT DISTINCT FROM COALESCE($3::text[], '{}'::text[])
			FOR UPDATE
		), inserted AS (
			INSERT INTO link_concept (link_id, concept_id, surface_tag)
			SELECT target.id, item.concept_id, item.surface_tag
			FROM target CROSS JOIN (VALUES `)
	written := 0
	for _, it := range items {
		if it.ConceptID == uuid.Nil {
			continue
		}
		if written > 0 {
			b.WriteByte(',')
		}
		// $1 identifies the guarded Link; $2/$3 are the metadata fence.
		// $N / $N+1 carry the per-row concept_id + surface_tag pair.
		fmt.Fprintf(&b, "($%d::uuid,$%d::text)", len(args)+1, len(args)+2)
		args = append(args, it.ConceptID, strings.TrimSpace(it.SurfaceTag))
		written++
	}
	if written == 0 {
		return false, nil
	}
	b.WriteString(`) AS item(concept_id, surface_tag)
		ON CONFLICT (link_id, concept_id) DO NOTHING
		RETURNING 1
	)
	SELECT EXISTS (SELECT 1 FROM target)`)
	var attached bool
	if err := r.db.QueryRow(ctx, b.String(), args...).Scan(&attached); err != nil {
		return false, fmt.Errorf("attach link_concept batch: %w", err)
	}
	return attached, nil
}

// IncrementUseCounts bumps use_count for every supplied concept ID in a
// single UPDATE. Each ID is incremented by exactly one regardless of
// how many times it appears in the input — duplicates are de-duped here
// as a defence-in-depth: the resolver layer also dedupes before calling,
// but keeping the guard here means a future caller that bypasses the
// resolver can't accidentally double-count. AttachLinkConcept is
// idempotent on (link_id, concept_id), so within a single link parse
// the same concept only attaches once and only counts once.
func (r *PGXConceptRepository) IncrementUseCounts(ctx context.Context, conceptIDs []uuid.UUID) error {
	if len(conceptIDs) == 0 {
		return nil
	}
	dedup := make([]uuid.UUID, 0, len(conceptIDs))
	seen := make(map[uuid.UUID]struct{}, len(conceptIDs))
	for _, id := range conceptIDs {
		if id == uuid.Nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		dedup = append(dedup, id)
	}
	if len(dedup) == 0 {
		return nil
	}
	if _, err := r.db.Exec(ctx,
		`UPDATE concept SET use_count = use_count + 1, updated_at = now()
		 WHERE id = ANY($1)`,
		dedup,
	); err != nil {
		return fmt.Errorf("increment use_counts: %w", err)
	}
	return nil
}

// RecalculateDisplayNames is the batch form of RecalculateDisplayName.
// One UPDATE...FROM rewrites every requested concept's display_name in
// a single statement; the subquery's GROUP BY uses the same "majority
// surface, tie-break lexicographic" rule as the singular path.
//
// Semantic difference vs the singular path: when a concept has zero
// link_concept rows, the singular UPDATE writes display_name = NULL
// (its subquery returns NULL); the batch version's winners CTE simply
// produces no row for that concept and the UPDATE...FROM join skips
// it, leaving display_name at its previous value. The batch behaviour
// is the safer one — a concept whose attachments all got removed
// elsewhere should not have its UI label silently cleared — and the
// pipeline never invokes this path for a freshly-zero concept anyway
// (it only calls it for concepts we just attached to).
//
// We use DISTINCT ON (concept_id) ordered by count(*) DESC,
// surface_tag ASC, which Postgres can satisfy with the existing
// (concept_id) btree on link_concept; the alternative of one window
// per concept requires a sort over the whole table.
func (r *PGXConceptRepository) RecalculateDisplayNames(ctx context.Context, conceptIDs []uuid.UUID) error {
	if len(conceptIDs) == 0 {
		return nil
	}
	dedup := make([]uuid.UUID, 0, len(conceptIDs))
	seen := make(map[uuid.UUID]struct{}, len(conceptIDs))
	for _, id := range conceptIDs {
		if id == uuid.Nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		dedup = append(dedup, id)
	}
	if len(dedup) == 0 {
		return nil
	}
	const recalcSQL = `UPDATE concept c
		 SET display_name = winners.surface_tag,
		     updated_at = now()
		 FROM (
		   SELECT DISTINCT ON (concept_id) concept_id, surface_tag
		   FROM (
		     SELECT concept_id, surface_tag, count(*) AS uses
		     FROM link_concept
		     WHERE concept_id = ANY($1)
		     GROUP BY concept_id, surface_tag
		   ) counts
		   ORDER BY concept_id, uses DESC, surface_tag ASC
		 ) winners
		 WHERE c.id = winners.concept_id`
	if _, err := r.db.Exec(ctx, recalcSQL, dedup); err != nil {
		return fmt.Errorf("recalc display_names: %w", err)
	}
	return nil
}

// DetachLinkConceptsExcept implements concept.ConceptStore. It deletes the
// link's link_concept rows whose concept_id is not in keepConceptIDs and
// returns the distinct concept IDs actually removed so the caller can
// recompute their display names. The Link row is locked and revalidated under
// the same metadata fence as AttachLinkConceptsBatch; this later reconciliation
// phase must not prune a newer tuple after a concurrent Reader save.
//
// Guard: an empty keep set is a no-op rather than a full wipe — callers
// reach this only after a successful non-empty attach, but the guard keeps
// a future mis-call from blanking a link's concept set.
func (r *PGXConceptRepository) DetachLinkConceptsExcept(
	ctx context.Context,
	linkID uuid.UUID,
	expectedMetadataRevision int64,
	expectedTags []string,
	keepConceptIDs []uuid.UUID,
) ([]uuid.UUID, error) {
	if linkID == uuid.Nil || expectedMetadataRevision <= 0 || len(keepConceptIDs) == 0 {
		return nil, nil
	}
	keep := make([]uuid.UUID, 0, len(keepConceptIDs))
	seenKeep := make(map[uuid.UUID]struct{}, len(keepConceptIDs))
	for _, id := range keepConceptIDs {
		if id == uuid.Nil {
			continue
		}
		if _, ok := seenKeep[id]; ok {
			continue
		}
		seenKeep[id] = struct{}{}
		keep = append(keep, id)
	}
	if len(keep) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(ctx,
		`WITH target AS MATERIALIZED (
				SELECT id
				FROM links
				WHERE id = $1 AND deleted_at IS NULL
					AND metadata_revision = $2
					AND tags IS NOT DISTINCT FROM COALESCE($3::text[], '{}'::text[])
				FOR UPDATE
			), deleted AS (
				DELETE FROM link_concept
				WHERE link_id IN (SELECT id FROM target)
					AND concept_id <> ALL($4)
				RETURNING concept_id
			)
			SELECT concept_id FROM deleted`,
		linkID, expectedMetadataRevision, expectedTags, keep,
	)
	if err != nil {
		return nil, fmt.Errorf("detach link_concepts: %w", err)
	}
	defer rows.Close()
	removed := make([]uuid.UUID, 0)
	seenRemoved := make(map[uuid.UUID]struct{})
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan detached concept row: %w", err)
		}
		if _, ok := seenRemoved[id]; ok {
			continue
		}
		seenRemoved[id] = struct{}{}
		removed = append(removed, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate detached concept rows: %w", err)
	}
	return removed, nil
}
