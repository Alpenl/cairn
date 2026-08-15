package repository

import (
	"context"
	"fmt"
	"strings"

	"webtag/internal/model"
)

// statusWhereClause builds the leading "status" predicate plus its
// positional arg (if any). When filter.Statuses is empty it returns the
// historical literal `status = 'done'` (no arg) so the default path stays
// byte-for-byte identical and keeps matching the idx_links_done_created
// partial index. When non-empty it returns `status = ANY($1::text[])`
// with the slice as the first positional arg, so callers can mix
// pending/processing/failed/done in one query.
func statusWhereClause(statuses []string, argPos int) (clause string, arg any, hasArg bool, nextArgPos int) {
	if len(statuses) == 0 {
		return "status = 'done'", nil, false, argPos
	}
	return fmt.Sprintf("status = ANY($%d::text[])", argPos), statuses, true, argPos + 1
}

// ListDone 列出链接列表，根据 filter 选择搜索 / cursor / offset 路径。
// filter.Query 非空时进入 Phase 9 混合搜索（语义近邻 ∪ ILIKE，top-N 截断，
// 不分页）——优先级最高，分页字段被忽略。否则 filter.Cursor 决定 cursor 或
// offset 分页。filter.Statuses 为空时只返回 status='done'（历史行为）；非空
// 时按给定状态集合过滤（pending / processing / failed / done 的任意子集）。
func (r *PGXLinkRepository) ListDone(ctx context.Context, filter ListLinksFilter) ([]model.Link, int, error) {
	if filter.Query != nil {
		return r.searchHybrid(ctx, filter)
	}
	if filter.Cursor {
		return r.listDoneByCursor(ctx, filter)
	}
	return r.listDoneByOffset(ctx, filter)
}

func (r *PGXLinkRepository) listDoneByOffset(ctx context.Context, filter ListLinksFilter) ([]model.Link, int, error) {
	listSQL, countSQL, listArgs, countArgs := buildListDoneSQL(filter)

	rows, err := r.db.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list links: %w", err)
	}
	defer rows.Close()

	links := make([]model.Link, 0)
	total := 0
	for rows.Next() {
		link, rowTotal, err := scanLinkListWithTotal(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan link row: %w", err)
		}
		links = append(links, link)
		total = rowTotal
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate links: %w", err)
	}

	// COUNT(*) OVER() only emits a value when at least one row is returned.
	// When the page is empty (e.g. offset past the end, or zero matches)
	// fall back to a dedicated COUNT(*) so callers still get the right
	// total to render pagination controls.
	if len(links) == 0 {
		if err := r.db.QueryRow(ctx, countSQL, countArgs...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count links: %w", err)
		}
	}

	return links, total, nil
}

// listDoneByCursor implements the cursor-mode listing. It deliberately
// skips the windowed COUNT(*) — cursor clients do not paginate by
// total. Returned total is 0 sentinel; the service layer surfaces this
// via the absence of a meaningful Total value in the response.
func (r *PGXLinkRepository) listDoneByCursor(ctx context.Context, filter ListLinksFilter) ([]model.Link, int, error) {
	listSQL, listArgs := buildListDoneCursorSQL(filter)

	rows, err := r.db.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list links by cursor: %w", err)
	}
	defer rows.Close()

	links := make([]model.Link, 0)
	for rows.Next() {
		link, err := scanLinkList(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan link row: %w", err)
		}
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate cursor links: %w", err)
	}
	return links, 0, nil
}

// buildListDoneSQL returns the page query (with windowed COUNT(*)) plus an
// explicit COUNT(*) fallback used when the page is empty. listArgs are the
// positional parameters for the page query (filter args + limit + offset);
// countArgs are the same filter args without the limit/offset pair.
func buildListDoneSQL(filter ListLinksFilter) (listSQL string, countSQL string, listArgs []any, countArgs []any) {
	var where strings.Builder
	countArgs = make([]any, 0, 5)
	argPos := 1

	statusClause, statusArg, statusHasArg, argPos := statusWhereClause(filter.Statuses, argPos)
	if statusHasArg {
		countArgs = append(countArgs, statusArg)
	}

	if len(filter.Tags) > 0 {
		fmt.Fprintf(&where, " AND tags @> $%d::text[]", argPos)
		countArgs = append(countArgs, filter.Tags)
		argPos++
	}
	if filter.Domain != nil && *filter.Domain != "" {
		fmt.Fprintf(&where, " AND domain = $%d", argPos)
		countArgs = append(countArgs, *filter.Domain)
		argPos++
	}
	if filter.ContentType != nil && *filter.ContentType != "" {
		fmt.Fprintf(&where, " AND content_type = $%d", argPos)
		countArgs = append(countArgs, *filter.ContentType)
		argPos++
	}
	if filter.LibraryKind != nil {
		fmt.Fprintf(&where, " AND library_kind = $%d", argPos)
		countArgs = append(countArgs, *filter.LibraryKind)
		argPos++
	}
	if filter.LowConfidence != nil {
		fmt.Fprintf(&where, " AND is_low_confidence = $%d", argPos)
		countArgs = append(countArgs, *filter.LowConfidence)
		argPos++
	}
	if filter.CreatedFrom != nil {
		fmt.Fprintf(&where, " AND created_at >= $%d", argPos)
		countArgs = append(countArgs, *filter.CreatedFrom)
		argPos++
	}
	if filter.CreatedBefore != nil {
		fmt.Fprintf(&where, " AND created_at < $%d", argPos)
		countArgs = append(countArgs, *filter.CreatedBefore)
		argPos++
	}

	whereClause := where.String()
	countSQL = "SELECT count(*) FROM links WHERE deleted_at IS NULL AND " + statusClause + whereClause
	// id DESC tiebreaker: created_at collisions otherwise leave the
	// cursor client unable to advance past a duplicate timestamp. Adding
	// the tiebreaker to the offset path too keeps both modes ordered
	// identically so a switch from page= to after= does not skip rows.
	listSQL = "SELECT " + linkListColumnsWithTotal + " FROM links WHERE deleted_at IS NULL AND " + statusClause + whereClause +
		fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d", argPos, argPos+1)

	listArgs = make([]any, 0, len(countArgs)+2)
	listArgs = append(listArgs, countArgs...)
	listArgs = append(listArgs, filter.Limit, filter.Offset)

	return listSQL, countSQL, listArgs, countArgs
}

// buildListDoneCursorSQL builds the cursor-mode SQL: same filter
// clauses as buildListDoneSQL but with no windowed count. When
// filter.After is non-nil, an additional (created_at, id) tuple
// comparison continues from the cursor; when After is nil the call is
// the "start of stream" first page.
func buildListDoneCursorSQL(filter ListLinksFilter) (listSQL string, listArgs []any) {
	var where strings.Builder
	listArgs = make([]any, 0, 7)
	argPos := 1

	statusClause, statusArg, statusHasArg, argPos := statusWhereClause(filter.Statuses, argPos)
	if statusHasArg {
		listArgs = append(listArgs, statusArg)
	}

	if len(filter.Tags) > 0 {
		fmt.Fprintf(&where, " AND tags @> $%d::text[]", argPos)
		listArgs = append(listArgs, filter.Tags)
		argPos++
	}
	if filter.Domain != nil && *filter.Domain != "" {
		fmt.Fprintf(&where, " AND domain = $%d", argPos)
		listArgs = append(listArgs, *filter.Domain)
		argPos++
	}
	if filter.ContentType != nil && *filter.ContentType != "" {
		fmt.Fprintf(&where, " AND content_type = $%d", argPos)
		listArgs = append(listArgs, *filter.ContentType)
		argPos++
	}
	if filter.LibraryKind != nil {
		fmt.Fprintf(&where, " AND library_kind = $%d", argPos)
		listArgs = append(listArgs, *filter.LibraryKind)
		argPos++
	}
	if filter.LowConfidence != nil {
		fmt.Fprintf(&where, " AND is_low_confidence = $%d", argPos)
		listArgs = append(listArgs, *filter.LowConfidence)
		argPos++
	}
	if filter.CreatedFrom != nil {
		fmt.Fprintf(&where, " AND created_at >= $%d", argPos)
		listArgs = append(listArgs, *filter.CreatedFrom)
		argPos++
	}
	if filter.CreatedBefore != nil {
		fmt.Fprintf(&where, " AND created_at < $%d", argPos)
		listArgs = append(listArgs, *filter.CreatedBefore)
		argPos++
	}

	if filter.After != nil {
		fmt.Fprintf(&where, " AND (created_at, id) < ($%d, $%d)", argPos, argPos+1)
		listArgs = append(listArgs, filter.After.CreatedAt, filter.After.ID)
		argPos += 2
	}

	listSQL = "SELECT " + linkListColumns + " FROM links WHERE deleted_at IS NULL AND " + statusClause + where.String() +
		fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", argPos)
	listArgs = append(listArgs, filter.Limit)
	return listSQL, listArgs
}
