package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/database"
	"webtag/internal/model"
	"webtag/internal/notetitle"
)

func scanReaderNote(row readerScanner) (*model.ReaderNote, error) {
	var out model.ReaderNote
	if err := row.Scan(&out.ID, &out.Title, &out.PublishedContent, &out.PublishedRevision, &out.DraftContent, &out.DraftRevision, &out.DraftUpdatedAt, &out.DeletedAt, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return nil, err
	}
	return &out, nil
}

const readerNoteColumns = `id, title, published_content, published_revision, draft_content, draft_revision, draft_updated_at, deleted_at, created_at, updated_at`

// CreateNote runs in a transaction because a note created with published
// content is immediately a TODO source; the note row and its projections must
// become visible together.
func (r *PGXReaderVNextRepository) CreateNote(ctx context.Context, note model.ReaderNote) (*model.ReaderNote, error) {
	var created *model.ReaderNote
	err := r.withTx(ctx, func(db database.Querier) error {
		item, err := scanReaderNote(db.QueryRow(ctx, `
			INSERT INTO reader_notes (title, published_content, published_revision, draft_content, draft_revision)
			VALUES (COALESCE(NULLIF($1,''),'未命名笔记'),$2,CASE WHEN $2 <> '' THEN 1 ELSE 0 END,$3::text,CASE WHEN $3::text IS NOT NULL THEN 1 ELSE 0 END)
			RETURNING `+readerNoteColumns, note.Title, note.PublishedContent, note.DraftContent))
		if err != nil {
			return fmt.Errorf("create note: %w", err)
		}
		created = item
		return r.replaceNoteTodoProjectionsOn(ctx, db, item.ID)
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (r *PGXReaderVNextRepository) ListNotes(ctx context.Context, after string, limit int) ([]model.ReaderNote, int, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	at, id, err := parseReaderCursor(after)
	if err != nil {
		return nil, 0, "", err
	}
	args := []any{}
	sql := `SELECT ` + readerNoteColumns + ` FROM reader_notes WHERE deleted_at IS NULL`
	if !at.IsZero() {
		parsedID, parseErr := uuid.Parse(id)
		if parseErr != nil {
			return nil, 0, "", fmt.Errorf("%w: invalid note cursor", ErrInvalidReaderCursor)
		}
		sql += fmt.Sprintf(` AND (updated_at < $%d OR (updated_at = $%d AND id < $%d))`, len(args)+1, len(args)+1, len(args)+2)
		args = append(args, at, parsedID)
	}
	sql += fmt.Sprintf(` ORDER BY updated_at DESC, id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, "", fmt.Errorf("list notes: %w", err)
	}
	defer rows.Close()
	items := make([]model.ReaderNote, 0)
	for rows.Next() {
		item, err := scanReaderNote(rows)
		if err != nil {
			return nil, 0, "", fmt.Errorf("scan note: %w", err)
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, "", err
	}
	var count int
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM reader_notes WHERE deleted_at IS NULL`).Scan(&count); err != nil {
		return nil, 0, "", fmt.Errorf("count notes: %w", err)
	}
	if len(items) == limit {
		last := items[len(items)-1]
		return items, count, readerCursor(last.UpdatedAt, last.ID.String()), nil
	}
	return items, count, "", nil
}

func (r *PGXReaderVNextRepository) SearchPublishedNotes(ctx context.Context, query string, limit int) ([]model.ReaderNoteSearch, int, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []model.ReaderNoteSearch{}, 0, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, title,
			left(regexp_replace(published_content, '\s+', ' ', 'g'), 240),
			published_revision, count(*) OVER (), updated_at
		FROM reader_notes
		WHERE deleted_at IS NULL AND published_revision > 0
			AND (title ILIKE $1 OR published_content ILIKE $1)
		ORDER BY updated_at DESC, id DESC
		LIMIT $2`, "%"+query+"%", limit)
	if err != nil {
		return nil, 0, fmt.Errorf("search published notes: %w", err)
	}
	defer rows.Close()
	items := make([]model.ReaderNoteSearch, 0)
	total := 0
	for rows.Next() {
		var item model.ReaderNoteSearch
		var totalRows int64
		if err := rows.Scan(&item.ID, &item.Title, &item.Snippet, &item.PublishedRevision, &totalRows, &item.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan note search result: %w", err)
		}
		itemTotal := int(totalRows)
		if itemTotal > total {
			total = itemTotal
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (r *PGXReaderVNextRepository) GetNote(ctx context.Context, id uuid.UUID) (*model.ReaderNote, error) {
	item, err := scanReaderNote(r.db.QueryRow(ctx, `SELECT `+readerNoteColumns+` FROM reader_notes WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get note: %w", err)
	}
	return item, nil
}

func (r *PGXReaderVNextRepository) SaveNoteDraft(ctx context.Context, command model.ReaderNoteDraftCommand) (*model.ReaderNote, error) {
	// The data-modifying CTE and the live-row probe share one PostgreSQL
	// statement snapshot. A separate existence query after a zero-row UPDATE
	// could observe a concurrent delete and misreport a missing note as stale.
	var (
		outcome                                         string
		id                                              pgtype.UUID
		title, publishedContent, draftContent           pgtype.Text
		publishedRevision, draftRevision                pgtype.Int8
		draftUpdatedAt, deletedAt, createdAt, updatedAt pgtype.Timestamptz
	)
	err := r.db.QueryRow(ctx, `
		WITH updated AS (
			UPDATE reader_notes
			SET draft_content=$1, draft_revision=draft_revision+1, draft_updated_at=NOW(), updated_at=NOW()
			WHERE id=$2 AND deleted_at IS NULL AND draft_revision=$3
			RETURNING `+readerNoteColumns+`
		), live AS (
			SELECT 1 FROM reader_notes WHERE id=$2 AND deleted_at IS NULL
		)
		SELECT 'updated', `+readerNoteColumns+` FROM updated
		UNION ALL
		SELECT CASE WHEN EXISTS (SELECT 1 FROM live) THEN 'stale' ELSE 'missing' END,
			NULL::uuid, NULL::text, NULL::text, NULL::bigint, NULL::text, NULL::bigint,
			NULL::timestamptz, NULL::timestamptz, NULL::timestamptz, NULL::timestamptz
		WHERE NOT EXISTS (SELECT 1 FROM updated)`, command.Content, command.NoteID, command.ExpectedDraftRevision).
		Scan(&outcome, &id, &title, &publishedContent, &publishedRevision, &draftContent, &draftRevision, &draftUpdatedAt, &deletedAt, &createdAt, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("save note draft: %w", err)
	}
	if outcome == "missing" {
		return nil, ErrNotFound
	}
	if outcome == "stale" {
		return nil, ErrRevisionConflict
	}
	if outcome != "updated" || !id.Valid || !title.Valid || !publishedContent.Valid || !publishedRevision.Valid || !draftRevision.Valid || !createdAt.Valid || !updatedAt.Valid {
		return nil, fmt.Errorf("save note draft: invalid outcome %q", outcome)
	}
	var draftContentValue *string
	if draftContent.Valid {
		draftContentValue = &draftContent.String
	}
	var draftUpdatedAtValue, deletedAtValue *time.Time
	if draftUpdatedAt.Valid {
		draftUpdatedAtValue = &draftUpdatedAt.Time
	}
	if deletedAt.Valid {
		deletedAtValue = &deletedAt.Time
	}
	return &model.ReaderNote{
		ID:                id.Bytes,
		Title:             title.String,
		PublishedContent:  publishedContent.String,
		PublishedRevision: publishedRevision.Int64,
		DraftContent:      draftContentValue,
		DraftRevision:     draftRevision.Int64,
		DraftUpdatedAt:    draftUpdatedAtValue,
		DeletedAt:         deletedAtValue,
		CreatedAt:         createdAt.Time,
		UpdatedAt:         updatedAt.Time,
	}, nil
}

func (r *PGXReaderVNextRepository) DiscardNoteDraft(ctx context.Context, command model.ReaderNoteDiscardDraftCommand) error {
	result, err := r.db.Exec(ctx, `
		UPDATE reader_notes
		SET draft_content=NULL, draft_updated_at=NULL, draft_revision=draft_revision+1, updated_at=NOW()
		WHERE id=$1 AND deleted_at IS NULL AND draft_revision=$2
			AND draft_content IS NOT NULL`, command.NoteID, command.ExpectedDraftRevision)
	if err != nil {
		return fmt.Errorf("discard note draft: %w", err)
	}
	if result.RowsAffected() == 0 {
		// A note with no draft is already in the requested state. Keep this
		// idempotent, but distinguish a stale revision from a missing note.
		var exists bool
		if err := r.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reader_notes WHERE id=$1 AND deleted_at IS NULL)`, command.NoteID).Scan(&exists); err != nil {
			return fmt.Errorf("check note draft discard: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		var revision int64
		var hasDraft bool
		if err := r.db.QueryRow(ctx, `SELECT draft_revision, draft_content IS NOT NULL FROM reader_notes WHERE id=$1 AND deleted_at IS NULL`, command.NoteID).Scan(&revision, &hasDraft); err != nil {
			return fmt.Errorf("read note draft discard state: %w", err)
		}
		if revision != command.ExpectedDraftRevision || hasDraft {
			return ErrRevisionConflict
		}
	}
	return nil
}

func (r *PGXReaderVNextRepository) PublishNote(ctx context.Context, command model.ReaderNotePublishCommand) (*model.ReaderNote, error) {
	var out *model.ReaderNote
	err := r.withTx(ctx, func(db database.Querier) error {
		var current model.ReaderNote
		row := db.QueryRow(ctx, `SELECT `+readerNoteColumns+` FROM reader_notes WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, command.NoteID)
		item, err := scanReaderNote(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		current = *item
		if current.DraftRevision != command.ExpectedDraftRevision || current.PublishedRevision != command.ExpectedPublishedRevision {
			return ErrRevisionConflict
		}
		content := current.PublishedContent
		if current.DraftContent != nil {
			content = *current.DraftContent
		}
		if readerNoteContentCanonicalEmpty(content) {
			return ErrReaderNoteContentEmpty
		}
		// A retry after a completed publish does not need a reanchor batch and
		// must not perturb revision, history, operations, or timestamps.
		if content == current.PublishedContent {
			out = &current
			return nil
		}
		reanchorOps, err := readerReanchorOpsJSON(command.ReanchorOps)
		if err != nil {
			return err
		}
		if err := r.validateNoteReanchorSet(ctx, db, command.NoteID, command.ReanchorOps); err != nil {
			return err
		}
		newRevision := current.PublishedRevision + 1
		title := notetitle.Derive(content)
		if _, err := db.Exec(ctx, `
			UPDATE reader_notes SET title=$1,published_content=$2, published_revision=$3,
				draft_content=NULL, draft_revision=draft_revision+1, draft_updated_at=NULL, updated_at=NOW()
			WHERE id=$4`, title, content, newRevision, command.NoteID); err != nil {
			return fmt.Errorf("publish note: %w", err)
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO reader_note_history (note_id, revision, title, content, reanchor_ops)
			VALUES ($1,$2,$3,$4,$5::jsonb)
			ON CONFLICT (note_id,revision) DO NOTHING`, command.NoteID, newRevision, title, content, reanchorOps); err != nil {
			return fmt.Errorf("record note history: %w", err)
		}
		if err := r.applyNoteReanchorOps(ctx, db, command.NoteID, current.PublishedRevision, newRevision, content, command.ReanchorOps); err != nil {
			return err
		}
		if err := r.replaceNoteTodoProjectionsOn(ctx, db, command.NoteID); err != nil {
			return err
		}
		out, err = scanReaderNote(db.QueryRow(ctx, `SELECT `+readerNoteColumns+` FROM reader_notes WHERE id=$1`, command.NoteID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readerNoteContentCanonicalEmpty deliberately matches the Reader's wire
// contract. Unicode whitespace is Markdown content, not an empty draft.
func readerNoteContentCanonicalEmpty(content string) bool {
	return strings.Trim(content, " \t\r\n") == ""
}

// validateNoteReanchorSet runs while PublishNote/RestoreNoteRevision hold the
// note row FOR UPDATE. AppendThoughtOps acquires that same host row FOR SHARE
// before it can materialize a live note thought, so a writer either commits
// before this query and is included, or is ordered after the transition. The
// row locks below keep each included active thought stable while it is checked.
func (r *PGXReaderVNextRepository) validateNoteReanchorSet(ctx context.Context, db database.Querier, noteID uuid.UUID, rawOps []json.RawMessage) error {
	rows, err := db.Query(ctx, `SELECT reader_thoughts.id
		FROM reader_thoughts
		WHERE reader_thoughts.host_kind='note'
			AND reader_thoughts.host_id=$1
			AND reader_thoughts.deleted=false
			AND NOT EXISTS (
				SELECT 1 FROM reader_thought_tombstones tombstone
				WHERE tombstone.thought_id=reader_thoughts.id
			)
		ORDER BY reader_thoughts.id
		FOR UPDATE`, noteID.String())
	if err != nil {
		return fmt.Errorf("list active note thoughts: %w", err)
	}
	defer rows.Close()
	active := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan active note thought: %w", err)
		}
		active[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate active note thoughts: %w", err)
	}
	provided := make(map[string]struct{}, len(rawOps))
	for _, raw := range rawOps {
		op, err := decodeReaderNoteReanchorOp(raw)
		if err != nil {
			return err
		}
		if _, exists := provided[op.ThoughtID]; exists {
			return ErrReaderNoteReanchorIncomplete
		}
		provided[op.ThoughtID] = struct{}{}
	}
	if len(active) != len(provided) {
		return ErrReaderNoteReanchorIncomplete
	}
	for id := range active {
		if _, exists := provided[id]; !exists {
			return ErrReaderNoteReanchorIncomplete
		}
	}
	return nil
}

type readerNoteReanchorOp struct {
	ThoughtID string `json:"thought_id"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	Target    struct {
		Kind    string `json:"kind"`
		HostID  string `json:"host_id"`
		Version struct {
			NoteRevision int64 `json:"note_revision"`
		} `json:"version"`
	} `json:"target"`
	Quote map[string]any `json:"quote"`
	Range struct {
		Start int `json:"start"`
		End   int `json:"end"`
	} `json:"range"`
}

func decodeReaderNoteReanchorOp(raw json.RawMessage) (readerNoteReanchorOp, error) {
	var op readerNoteReanchorOp
	if err := json.Unmarshal(raw, &op); err != nil || strings.TrimSpace(op.ThoughtID) == "" {
		return readerNoteReanchorOp{}, ErrInvalidReaderReanchor
	}
	if op.Status != "reanchored" && op.Status != "historical" {
		return readerNoteReanchorOp{}, ErrInvalidReaderReanchor
	}
	if op.Status == "reanchored" {
		if err := validateReaderNoteReanchorAnchor(op); err != nil {
			return readerNoteReanchorOp{}, err
		}
	}
	return op, nil
}

// validateReaderNoteReanchorAnchor guards the fields that only a successfully
// reanchored op is allowed to carry: a recognised reason, a note target pinned
// to a concrete revision, and a quote whose range is a usable slice. Historical
// ops skip this because they carry no anchor at all.
func validateReaderNoteReanchorAnchor(op readerNoteReanchorOp) error {
	if op.Reason != "diff-context" && op.Reason != "unique-quote" {
		return ErrInvalidReaderReanchor
	}
	if op.Target.Kind != "note" || strings.TrimSpace(op.Target.HostID) == "" || op.Target.Version.NoteRevision <= 0 {
		return ErrInvalidReaderReanchor
	}
	if op.Quote == nil {
		return ErrInvalidReaderReanchor
	}
	exact, ok := op.Quote["exact"].(string)
	if !ok || exact == "" || op.Range.Start < 0 || op.Range.End <= op.Range.Start || op.Range.End > 1<<24 {
		return ErrInvalidReaderReanchor
	}
	return nil
}

// readerUTF16Slice mirrors JavaScript String#slice offsets. Reanchor ranges
// are produced by the Reader client, whose offsets count UTF-16 code units;
// slicing Go bytes here would accept a range that points at different text for
// non-BMP characters.
func readerUTF16Slice(value string, start, end int) (string, bool) {
	if start < 0 || end <= start {
		return "", false
	}
	offset := 0
	byteStart, byteEnd := -1, -1
	for byteOffset, r := range value {
		if offset == start {
			byteStart = byteOffset
		}
		width := utf16.RuneLen(r)
		if width < 0 {
			width = 1
		}
		nextOffset := offset + width
		byteEndOffset := byteOffset + len(string(r))
		if nextOffset == start {
			byteStart = byteEndOffset
		}
		if nextOffset == end {
			byteEnd = byteEndOffset
			break
		}
		offset = nextOffset
	}
	if byteStart < 0 && offset == start {
		byteStart = len(value)
	}
	if byteEnd < 0 {
		if offset == end {
			byteEnd = len(value)
		} else if byteStart >= 0 {
			// The requested end fell in the middle of a UTF-16 code point or
			// outside the string; neither is a valid client range.
			return "", false
		}
	}
	if byteStart < 0 || byteEnd < byteStart {
		return "", false
	}
	return value[byteStart:byteEnd], true
}

type readerReanchorMatch struct {
	start  int
	end    int
	reason string
}

func readerUTF16Occurrences(source, needle string) []int {
	sourceUnits := utf16.Encode([]rune(source))
	needleUnits := utf16.Encode([]rune(needle))
	if len(needleUnits) == 0 || len(needleUnits) > len(sourceUnits) {
		return nil
	}
	positions := make([]int, 0, 1)
	for start := 0; start <= len(sourceUnits)-len(needleUnits); start++ {
		matched := true
		for offset, unit := range needleUnits {
			if sourceUnits[start+offset] != unit {
				matched = false
				break
			}
		}
		if matched {
			positions = append(positions, start)
		}
	}
	return positions
}

func readerUTF16UnitsEqual(left, right []uint16) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func readerReanchorContextMatches(sourceUnits []uint16, start, end int, prefix, suffix string) bool {
	prefixUnits := utf16.Encode([]rune(prefix))
	if len(prefixUnits) > 0 {
		if start < len(prefixUnits) || !readerUTF16UnitsEqual(sourceUnits[start-len(prefixUnits):start], prefixUnits) {
			return false
		}
	}
	suffixUnits := utf16.Encode([]rune(suffix))
	if len(suffixUnits) > 0 {
		if end+len(suffixUnits) > len(sourceUnits) || !readerUTF16UnitsEqual(sourceUnits[end:end+len(suffixUnits)], suffixUnits) {
			return false
		}
	}
	return true
}

func readerReanchorDiffContextCandidates(sourceUnits []uint16, prefix, suffix string) []readerReanchorMatch {
	if prefix == "" || suffix == "" {
		return nil
	}
	candidates := make([]readerReanchorMatch, 0, 1)
	for _, prefixStart := range readerUTF16Occurrences(string(utf16.Decode(sourceUnits)), prefix) {
		start := prefixStart + len(utf16.Encode([]rune(prefix)))
		for _, suffixStart := range readerUTF16Occurrences(string(utf16.Decode(sourceUnits)), suffix) {
			if suffixStart <= start {
				continue
			}
			candidates = append(candidates, readerReanchorMatch{start: start, end: suffixStart, reason: "diff-context"})
		}
	}
	return candidates
}

func readerReanchorMatchForQuote(content, exact, prefix, suffix string) (readerReanchorMatch, bool) {
	sourceUnits := utf16.Encode([]rune(content))
	exactPositions := readerUTF16Occurrences(content, exact)
	if len(exactPositions) == 0 {
		candidates := readerReanchorDiffContextCandidates(sourceUnits, prefix, suffix)
		if len(candidates) == 1 {
			return candidates[0], true
		}
		if len(candidates) > 1 {
			return readerReanchorMatch{reason: "ambiguous-quote"}, false
		}
		return readerReanchorMatch{reason: "missing-quote"}, false
	}
	if prefix == "" && suffix == "" {
		if len(exactPositions) == 1 {
			start := exactPositions[0]
			return readerReanchorMatch{start: start, end: start + len(utf16.Encode([]rune(exact))), reason: "unique-quote"}, true
		}
		return readerReanchorMatch{reason: "ambiguous-quote"}, false
	}
	contextual := make([]readerReanchorMatch, 0, len(exactPositions))
	exactLength := len(utf16.Encode([]rune(exact)))
	for _, start := range exactPositions {
		end := start + exactLength
		if readerReanchorContextMatches(sourceUnits, start, end, prefix, suffix) {
			contextual = append(contextual, readerReanchorMatch{start: start, end: end, reason: "unique-quote"})
		}
	}
	if len(contextual) == 1 {
		return contextual[0], true
	}
	if len(contextual) > 1 || len(exactPositions) > 1 {
		return readerReanchorMatch{reason: "ambiguous-quote"}, false
	}
	return readerReanchorMatch{reason: "missing-quote"}, false
}

// decodeReaderReanchorQuote unpacks the stored selector. The quote map itself
// is returned so the caller can rewrite it in place, which keeps any selector
// field the Reader client added but this code does not understand.
func decodeReaderReanchorQuote(raw json.RawMessage) (map[string]json.RawMessage, string, string, string, error) {
	var quote map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &quote) != nil || quote == nil {
		return nil, "", "", "", ErrInvalidReaderReanchor
	}
	var exact, prefix, suffix string
	if value, ok := quote["exact"]; !ok || json.Unmarshal(value, &exact) != nil || exact == "" {
		return nil, "", "", "", ErrInvalidReaderReanchor
	}
	if value, ok := quote["prefix"]; ok && json.Unmarshal(value, &prefix) != nil {
		return nil, "", "", "", ErrInvalidReaderReanchor
	}
	if value, ok := quote["suffix"]; ok && json.Unmarshal(value, &suffix) != nil {
		return nil, "", "", "", ErrInvalidReaderReanchor
	}
	return quote, exact, prefix, suffix, nil
}

// readerReanchorRewriteContext replaces the selector context with the 32 UTF-16
// units surrounding the match in the current content. Carrying the old context
// forward would leave a selector that no longer disambiguates the new text.
func readerReanchorRewriteContext(quote map[string]json.RawMessage, content string, contentUnits []uint16, match readerReanchorMatch) {
	quote["prefix"] = json.RawMessage(`""`)
	quote["suffix"] = json.RawMessage(`""`)
	if match.start > 0 {
		contextStart := match.start - 32
		if contextStart < 0 {
			contextStart = 0
		}
		if value, valid := readerUTF16Slice(content, contextStart, match.start); valid {
			quote["prefix"], _ = json.Marshal(value)
		}
	}
	if match.end < len(contentUnits) {
		contextEnd := match.end + 32
		if contextEnd > len(contentUnits) {
			contextEnd = len(contentUnits)
		}
		if value, valid := readerUTF16Slice(content, match.end, contextEnd); valid {
			quote["suffix"], _ = json.Marshal(value)
		}
	}
}

func readerReanchorQuoteForContent(content string, raw json.RawMessage) (json.RawMessage, string, error) {
	quote, exact, prefix, suffix, err := decodeReaderReanchorQuote(raw)
	if err != nil {
		return nil, "missing-quote", err
	}
	match, ok := readerReanchorMatchForQuote(content, exact, prefix, suffix)
	if !ok {
		if match.reason == "" {
			match.reason = "missing-quote"
		}
		return nil, match.reason, ErrInvalidReaderReanchor
	}
	contentUnits := utf16.Encode([]rune(content))
	matchedExact, valid := readerUTF16Slice(content, match.start, match.end)
	if !valid {
		return nil, "missing-quote", ErrInvalidReaderReanchor
	}
	quote["exact"], _ = json.Marshal(matchedExact)
	readerReanchorRewriteContext(quote, content, contentUnits, match)
	quote["start"], _ = json.Marshal(match.start)
	quote["end"], _ = json.Marshal(match.end)
	encoded, err := json.Marshal(quote)
	if err != nil {
		return nil, match.reason, err
	}
	return encoded, match.reason, nil
}

func (r *PGXReaderVNextRepository) applyNoteReanchorOps(
	ctx context.Context,
	db database.Querier,
	noteID uuid.UUID,
	previousRevision int64,
	nextRevision int64,
	content string,
	rawOps []json.RawMessage,
) error {
	seen := make(map[string]struct{}, len(rawOps))
	for _, raw := range rawOps {
		op, err := decodeReaderNoteReanchorOp(raw)
		if err != nil {
			return err
		}
		if _, ok := seen[op.ThoughtID]; ok {
			return ErrInvalidReaderReanchor
		}
		seen[op.ThoughtID] = struct{}{}
		if err := r.applyNoteReanchorOp(ctx, db, noteID, previousRevision, nextRevision, content, op); err != nil {
			return err
		}
	}
	return nil
}

// applyNoteReanchorOp moves one thought onto the new note revision, or
// tombstones it when the publisher reported it could not be reanchored.
func (r *PGXReaderVNextRepository) applyNoteReanchorOp(
	ctx context.Context,
	db database.Querier,
	noteID uuid.UUID,
	previousRevision int64,
	nextRevision int64,
	content string,
	op readerNoteReanchorOp,
) error {
	item, err := scanReaderThought(db.QueryRow(ctx, `SELECT `+readerThoughtColumns+` FROM reader_thoughts WHERE id=$1 AND deleted=false FOR UPDATE`, op.ThoughtID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidReaderReanchor
	}
	if err != nil {
		return fmt.Errorf("read note reanchor thought: %w", err)
	}
	if item.HostKind != "note" || item.HostID != noteID.String() {
		return ErrInvalidReaderReanchor
	}
	if op.Status == "historical" {
		reason := strings.TrimSpace(op.Reason)
		if reason == "" {
			reason = "not-reanchored"
		}
		return r.markThoughtTombstoneOn(ctx, db, op.ThoughtID, "note_reanchor_"+reason)
	}
	if err := validateNoteReanchorOp(item, op, noteID, previousRevision, nextRevision, content); err != nil {
		return err
	}
	return r.commitNoteReanchor(ctx, db, noteID, nextRevision, item, op)
}

// validateNoteReanchorOp proves the op moves the thought off exactly the
// previous revision onto the new one, and that the new range still quotes the
// same text. Anything else would silently re-point a thought at other prose.
func validateNoteReanchorOp(item *model.ReaderThought, op readerNoteReanchorOp, noteID uuid.UUID, previousRevision, nextRevision int64, content string) error {
	var previousTarget struct {
		Kind    string `json:"kind"`
		Version struct {
			NoteRevision int64 `json:"note_revision"`
		} `json:"version"`
	}
	if err := json.Unmarshal(item.Target, &previousTarget); err != nil || previousTarget.Kind != "note" || previousTarget.Version.NoteRevision != previousRevision {
		return ErrInvalidReaderReanchor
	}
	if op.Target.HostID != noteID.String() || op.Target.Version.NoteRevision != nextRevision {
		return ErrInvalidReaderReanchor
	}
	exact, _ := op.Quote["exact"].(string)
	rangedText, ok := readerUTF16Slice(content, op.Range.Start, op.Range.End)
	if !ok || rangedText != exact {
		return ErrInvalidReaderReanchor
	}
	return nil
}

// commitNoteReanchor writes the reanchored thought: a new op, the refreshed
// materialised row, and the tombstone cleared.
func (r *PGXReaderVNextRepository) commitNoteReanchor(
	ctx context.Context,
	db database.Querier,
	noteID uuid.UUID,
	nextRevision int64,
	item *model.ReaderThought,
	op readerNoteReanchorOp,
) error {
	quote := make(map[string]any, len(op.Quote)+2)
	for key, value := range op.Quote {
		quote[key] = value
	}
	quote["start"] = op.Range.Start
	quote["end"] = op.Range.End
	target, err := json.Marshal(op.Target)
	if err != nil {
		return fmt.Errorf("encode note reanchor target: %w", err)
	}
	quoteRaw, err := json.Marshal(quote)
	if err != nil {
		return fmt.Errorf("encode note reanchor quote: %w", err)
	}
	linkID := ""
	if item.LinkID != nil {
		linkID = item.LinkID.String()
	}
	payload, err := json.Marshal(struct {
		Body   string          `json:"body"`
		Quote  json.RawMessage `json:"quote"`
		Source string          `json:"source"`
		LinkID string          `json:"link_id,omitempty"`
	}{Body: item.Body, Quote: quoteRaw, Source: item.Source, LinkID: linkID})
	if err != nil {
		return fmt.Errorf("encode note reanchor payload: %w", err)
	}
	thoughtOp, sequence, duplicate, err := r.appendDerivedThoughtOp(ctx, db, model.ReaderThoughtOp{
		OpID:          "note-reanchor-" + noteID.String() + "-" + strconv.FormatInt(nextRevision, 10) + "-" + op.ThoughtID,
		DeviceID:      "reader-note-publish",
		OperationKind: "update",
		AnnotationID:  op.ThoughtID,
		HostKind:      "note",
		HostID:        noteID.String(),
		Target:        target,
		Payload:       payload,
	})
	if err != nil {
		return fmt.Errorf("append note reanchor op: %w", err)
	}
	if !duplicate {
		if err := r.materializeThought(ctx, db, thoughtOp, sequence); err != nil {
			return err
		}
	}
	if _, err := db.Exec(ctx, `DELETE FROM reader_thought_tombstones WHERE thought_id=$1`, op.ThoughtID); err != nil {
		return fmt.Errorf("clear note reanchor tombstone: %w", err)
	}
	// Clearing the tombstone brings the Thought back as a TODO source, which
	// the materialization above could not yet see.
	return r.replaceThoughtTodoProjectionsOn(ctx, db, op.ThoughtID)
}

func readerReanchorOpsJSON(ops []json.RawMessage) ([]byte, error) {
	if len(ops) > 500 {
		return nil, ErrInvalidReaderReanchor
	}
	if len(ops) == 0 {
		return []byte(`[]`), nil
	}
	encoded := make([]json.RawMessage, 0, len(ops))
	for _, raw := range ops {
		if len(raw) == 0 || len(raw) > 128*1024 || !json.Valid(raw) {
			return nil, ErrInvalidReaderReanchor
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return nil, ErrInvalidReaderReanchor
		}
		if _, err := decodeReaderNoteReanchorOp(raw); err != nil {
			return nil, err
		}
		encoded = append(encoded, append(json.RawMessage(nil), raw...))
	}
	return json.Marshal(encoded)
}

func (r *PGXReaderVNextRepository) ListNoteHistory(ctx context.Context, noteID uuid.UUID, limit int) ([]model.ReaderNoteHistory, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `SELECT id,note_id,revision,title,content,reanchor_ops,created_at FROM reader_note_history WHERE note_id=$1 ORDER BY revision DESC LIMIT $2`, noteID, limit)
	if err != nil {
		return nil, fmt.Errorf("list note history: %w", err)
	}
	defer rows.Close()
	out := make([]model.ReaderNoteHistory, 0)
	for rows.Next() {
		var item model.ReaderNoteHistory
		if err := rows.Scan(&item.ID, &item.NoteID, &item.Revision, &item.Title, &item.Content, &item.ReanchorOps, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *PGXReaderVNextRepository) RestoreNoteRevision(ctx context.Context, command model.ReaderNoteRestoreCommand) (*model.ReaderNote, error) {
	var out *model.ReaderNote
	err := r.withTx(ctx, func(db database.Querier) error {
		var err error
		out, err = r.restoreNoteRevisionOn(ctx, db, command)
		return err
	})
	return out, err
}

func (r *PGXReaderVNextRepository) restoreNoteRevisionOn(ctx context.Context, db database.Querier, command model.ReaderNoteRestoreCommand) (*model.ReaderNote, error) {
	current, err := lockNoteForRevisionRestore(ctx, db, command)
	if err != nil {
		return nil, err
	}
	content, err := noteHistoryContentForRestore(ctx, db, command)
	if err != nil {
		return nil, err
	}
	title := notetitle.Derive(content)
	reanchorOps, err := readerReanchorOpsJSON(command.ReanchorOps)
	if err != nil {
		return nil, err
	}
	if err := r.validateNoteReanchorSet(ctx, db, command.NoteID, command.ReanchorOps); err != nil {
		return nil, err
	}
	updated, err := updateRestoredNote(ctx, db, command.NoteID, current, title, content)
	if err != nil {
		return nil, err
	}
	newRevision := updated.PublishedRevision
	if _, err := db.Exec(ctx, `INSERT INTO reader_note_history (note_id,revision,title,content,reanchor_ops) VALUES ($1,$2,$3,$4,$5::jsonb) ON CONFLICT (note_id,revision) DO NOTHING`, command.NoteID, newRevision, title, content, reanchorOps); err != nil {
		return updated, fmt.Errorf("record restored note history: %w", err)
	}
	if err := r.applyNoteReanchorOps(ctx, db, command.NoteID, current.PublishedRevision, newRevision, content, command.ReanchorOps); err != nil {
		return updated, err
	}
	if err := r.replaceNoteTodoProjectionsOn(ctx, db, command.NoteID); err != nil {
		return updated, err
	}
	return updated, nil
}

func lockNoteForRevisionRestore(ctx context.Context, db database.Querier, command model.ReaderNoteRestoreCommand) (*model.ReaderNote, error) {
	if command.Revision <= 0 {
		return nil, ErrNotFound
	}
	current, err := scanReaderNote(db.QueryRow(ctx, `SELECT `+readerNoteColumns+` FROM reader_notes WHERE id=$1 FOR UPDATE`, command.NoteID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock note for revision restore: %w", err)
	}
	if current.DeletedAt != nil {
		return nil, ErrNotFound
	}
	if current.DraftRevision != command.ExpectedDraftRevision || current.PublishedRevision != command.ExpectedPublishedRevision {
		return nil, ErrRevisionConflict
	}
	if current.DraftContent != nil && !readerNoteContentCanonicalEmpty(*current.DraftContent) && *current.DraftContent != current.PublishedContent {
		return nil, ErrReaderNoteDraftDirty
	}
	return current, nil
}

func noteHistoryContentForRestore(ctx context.Context, db database.Querier, command model.ReaderNoteRestoreCommand) (string, error) {
	var content string
	err := db.QueryRow(ctx, `SELECT content FROM reader_note_history WHERE note_id=$1 AND revision=$2`, command.NoteID, command.Revision).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return content, err
}

func updateRestoredNote(ctx context.Context, db database.Querier, noteID uuid.UUID, current *model.ReaderNote, title, content string) (*model.ReaderNote, error) {
	updated, err := scanReaderNote(db.QueryRow(ctx, `UPDATE reader_notes
		SET title=$1,published_content=$2,published_revision=$3,
			draft_content=NULL,draft_revision=$4,draft_updated_at=NULL,updated_at=NOW()
		WHERE id=$5 AND deleted_at IS NULL
			AND published_revision=$6 AND draft_revision=$7
		RETURNING `+readerNoteColumns,
		title, content, current.PublishedRevision+1, current.DraftRevision+1, noteID, current.PublishedRevision, current.DraftRevision))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRevisionConflict
	}
	if err != nil {
		return nil, fmt.Errorf("restore note revision: %w", err)
	}
	return updated, nil
}
