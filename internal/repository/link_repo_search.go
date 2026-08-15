package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/pgvector/pgvector-go"

	"webtag/internal/model"
)

// hybridSearchLimit is the fixed top-N cap of the q= hybrid search. The Phase 9
// contract returns at most 50 rows and does not paginate; the service layer
// surfaces len(items) as the response Total.
const hybridSearchLimit = 50

// searchHybrid runs the Phase 9 q= search. With filter.QueryEmbedding set it
// merges the semantic nearest-neighbour leg (top-N by cosine, model-scoped)
// with the ILIKE keyword leg, deduped by id, semantic hits ordered first.
// With QueryEmbedding nil it degrades to pure ILIKE(title/summary/tags). Both
// legs honour the same status/tags/domain/content_type predicates. Always
// top-N truncated; total is len(rows) (no pagination).
func (r *PGXLinkRepository) searchHybrid(ctx context.Context, filter ListLinksFilter) ([]model.Link, int, error) {
	listSQL, args := buildHybridSearchSQL(filter)

	rows, err := r.db.Query(ctx, listSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("hybrid search links: %w", err)
	}
	defer rows.Close()

	links := make([]model.Link, 0, hybridSearchLimit)
	for rows.Next() {
		link, err := scanLinkList(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan hybrid search row: %w", err)
		}
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate hybrid search rows: %w", err)
	}
	return links, len(links), nil
}

// searchFilterPredicates assembles the shared WHERE predicates (status +
// tags/domain/content_type) both search legs apply, appending their args to
// `args` and advancing the positional counter. Factored out so the semantic
// and keyword legs stay byte-for-byte consistent and neither leg can drift
// from the offset/cursor list path's filter semantics.
func searchFilterPredicates(filter ListLinksFilter, args []any, argPos int) (clause string, outArgs []any, nextArgPos int) {
	var where strings.Builder
	where.WriteString("deleted_at IS NULL")

	statusClause, statusArg, statusHasArg, argPos := statusWhereClause(filter.Statuses, argPos)
	fmt.Fprintf(&where, " AND %s", statusClause)
	if statusHasArg {
		args = append(args, statusArg)
	}
	if len(filter.Tags) > 0 {
		fmt.Fprintf(&where, " AND tags @> $%d::text[]", argPos)
		args = append(args, filter.Tags)
		argPos++
	}
	if filter.Domain != nil && *filter.Domain != "" {
		fmt.Fprintf(&where, " AND domain = $%d", argPos)
		args = append(args, *filter.Domain)
		argPos++
	}
	if filter.ContentType != nil && *filter.ContentType != "" {
		fmt.Fprintf(&where, " AND content_type = $%d", argPos)
		args = append(args, *filter.ContentType)
		argPos++
	}
	return where.String(), args, argPos
}

// buildHybridSearchSQL constructs the q= search query + its positional args.
// Routes to the pure-ILIKE builder when no query vector is available and to
// the semantic∪keyword merge builder when one is.
func buildHybridSearchSQL(filter ListLinksFilter) (listSQL string, args []any) {
	pattern := ilikePattern(derefString(filter.Query))
	if len(filter.QueryEmbedding) == 0 || strings.TrimSpace(filter.EmbeddingModel) == "" {
		return buildKeywordOnlySQL(filter, pattern)
	}
	return buildSemanticMergeSQL(filter, pattern)
}

// buildKeywordOnlySQL is the EMBEDDING_MODEL-off / vectorization-failed
// fallback: pure ILIKE(title/summary/tags) ordered newest-first, top-N
// truncated. $1 is always the ILIKE pattern; filter predicate args follow.
func buildKeywordOnlySQL(filter ListLinksFilter, pattern string) (string, []any) {
	args := []any{pattern}
	filterClause, args, _ := searchFilterPredicates(filter, args, 2)

	listSQL := "SELECT " + linkListColumns + " FROM links WHERE " + filterClause +
		" AND " + keywordMatchClause(1) +
		fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT %d", hybridSearchLimit)
	return listSQL, args
}

// buildSemanticMergeSQL is the EMBEDDING_MODEL-on path: a CTE merge of the
// semantic nearest-neighbour leg and the ILIKE keyword leg, deduped by id,
// semantic hits ordered first (by ascending cosine distance), keyword-only
// hits after (newest-first). Positional args, in order: $1 query vector,
// $2 embedding model, $3 ILIKE pattern, then the filter predicate args (shared
// across both legs).
func buildSemanticMergeSQL(filter ListLinksFilter, pattern string) (string, []any) {
	args := []any{pgvector.NewVector(filter.QueryEmbedding), strings.TrimSpace(filter.EmbeddingModel), pattern}
	argPos := 4
	filterClause, args, _ := searchFilterPredicates(filter, args, argPos)

	semantic := "SELECT id, row_number() OVER (ORDER BY embedding <=> $1) AS rnk" +
		" FROM links WHERE " + filterClause +
		" AND embedding IS NOT NULL AND embedding_model = $2" +
		fmt.Sprintf(" ORDER BY embedding <=> $1 LIMIT %d", hybridSearchLimit)

	keyword := "SELECT id FROM links WHERE " + filterClause + " AND " + keywordMatchClause(3)

	// leg 0 = semantic (ordered by rnk), leg 1 = keyword-only (ordered by
	// created_at DESC). The keyword leg excludes ids already in semantic so the
	// UNION dedup keeps each link once with its semantic rank.
	merged := "WITH semantic AS (" + semantic + "), " +
		"keyword AS (" + keyword + ") " +
		"SELECT " + prefixedLinkListColumns("l") + " FROM links l JOIN (" +
		"SELECT id, 0 AS leg, rnk AS ord FROM semantic " +
		"UNION ALL " +
		"SELECT k.id, 1 AS leg, NULL::bigint AS ord FROM keyword k WHERE k.id NOT IN (SELECT id FROM semantic)" +
		") m ON m.id = l.id " +
		fmt.Sprintf("ORDER BY m.leg, m.ord, l.created_at DESC, l.id DESC LIMIT %d", hybridSearchLimit)

	return merged, args
}

// keywordMatchClause builds the ILIKE keyword predicate against title,
// summary, and any element of the tags array, all bound to the single
// positional arg at patternPos.
func keywordMatchClause(patternPos int) string {
	return fmt.Sprintf(
		"(title ILIKE $%d OR summary ILIKE $%d OR EXISTS (SELECT 1 FROM unnest(tags) AS t WHERE t ILIKE $%d))",
		patternPos, patternPos, patternPos,
	)
}

// prefixedLinkListColumns renders linkListColumns with a table alias prefix so
// the merge query's final SELECT can disambiguate columns from the joined
// links row.
func prefixedLinkListColumns(alias string) string {
	parts := strings.Split(linkListColumns, ", ")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ", ")
}

// ilikePattern wraps the user query in %…% and escapes LIKE metacharacters so a
// query containing %, _, or \ matches literally rather than as a wildcard.
func ilikePattern(query string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
	return "%" + escaped + "%"
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
