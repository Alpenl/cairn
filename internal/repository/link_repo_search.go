package repository

import (
	"context"
	"fmt"
	"strings"

	"webtag/internal/model"
)

const keywordSearchLimit = 50

func (r *PGXLinkRepository) searchKeyword(ctx context.Context, filter ListLinksFilter) ([]model.Link, int, error) {
	listSQL, args := buildKeywordSearchSQL(filter)

	rows, err := r.db.Query(ctx, listSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("keyword search links: %w", err)
	}
	defer rows.Close()

	links := make([]model.Link, 0, keywordSearchLimit)
	for rows.Next() {
		link, err := scanLinkList(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan keyword search row: %w", err)
		}
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate keyword search rows: %w", err)
	}
	return links, len(links), nil
}

// searchFilterPredicates appends the list filters used by keyword search.
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
	if filter.LibraryKind != nil {
		fmt.Fprintf(&where, " AND library_kind = $%d", argPos)
		args = append(args, *filter.LibraryKind)
		argPos++
	}
	if filter.LowConfidence != nil {
		fmt.Fprintf(&where, " AND is_low_confidence = $%d", argPos)
		args = append(args, *filter.LowConfidence)
		argPos++
	}
	if filter.CreatedFrom != nil {
		fmt.Fprintf(&where, " AND created_at >= $%d", argPos)
		args = append(args, *filter.CreatedFrom)
		argPos++
	}
	if filter.CreatedBefore != nil {
		fmt.Fprintf(&where, " AND created_at < $%d", argPos)
		args = append(args, *filter.CreatedBefore)
		argPos++
	}
	return where.String(), args, argPos
}

func buildKeywordSearchSQL(filter ListLinksFilter) (listSQL string, args []any) {
	pattern := ilikePattern(derefString(filter.Query))
	args = []any{pattern}
	filterClause, args, _ := searchFilterPredicates(filter, args, 2)
	limit := filter.Limit
	if limit < 1 || limit > keywordSearchLimit {
		limit = keywordSearchLimit
	}

	listSQL = "SELECT " + linkListColumns + " FROM links WHERE " + filterClause +
		" AND " + keywordMatchClause(1) +
		fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT %d", limit)
	return listSQL, args
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
