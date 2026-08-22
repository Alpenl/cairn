package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/database"
	"webtag/internal/model"
	"webtag/internal/readertext"
)

type readerTodoPageCursor struct {
	Done      bool       `json:"done"`
	DueAt     *time.Time `json:"due_at"`
	CreatedAt time.Time  `json:"created_at"`
	ID        uuid.UUID  `json:"id"`
}

func parseReaderTodoCursor(raw string) (readerTodoPageCursor, error) {
	if strings.TrimSpace(raw) == "" {
		return readerTodoPageCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return readerTodoPageCursor{}, fmt.Errorf("%w: invalid TODO cursor", ErrInvalidReaderCursor)
	}
	var cursor readerTodoPageCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.ID == uuid.Nil || cursor.CreatedAt.IsZero() {
		return readerTodoPageCursor{}, fmt.Errorf("%w: invalid TODO cursor", ErrInvalidReaderCursor)
	}
	return cursor, nil
}

func readerTodoCursor(item model.ReaderTodo) string {
	raw, _ := json.Marshal(readerTodoPageCursor{
		Done: item.Done, DueAt: item.DueAt, CreatedAt: item.CreatedAt, ID: item.ID,
	})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func scanReaderTodo(row readerScanner) (*model.ReaderTodo, error) {
	var out model.ReaderTodo
	var ref []byte
	if err := row.Scan(&out.ID, &out.Text, &out.DueAt, &out.Done, &out.OriginKind, &out.OriginHostKind, &out.OriginHostID, &ref, &out.HostRevision, &out.CompletedAt, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return nil, err
	}
	out.OriginRef = append(json.RawMessage(nil), ref...)
	out.Expired = out.DueAt != nil && out.DueAt.Before(time.Now()) && !out.Done
	return &out, nil
}

const readerTodoColumns = `id, text, due_at, done, origin_kind, origin_host_kind, origin_host_id, origin_ref, host_revision, completed_at, created_at, updated_at`

func (r *PGXReaderVNextRepository) CreateTodo(ctx context.Context, todo model.ReaderTodo) (*model.ReaderTodo, error) {
	created, err := scanReaderTodo(r.db.QueryRow(ctx, `
		INSERT INTO reader_todos (text,due_at,done,origin_kind,origin_host_kind,origin_host_id,origin_ref,completed_at)
		VALUES ($1,$2,$3,'standalone',NULL,NULL,$4::jsonb,$5)
		RETURNING `+readerTodoColumns, todo.Text, todo.DueAt, todo.Done, rawJSON(todo.OriginRef), todo.CompletedAt))
	if err != nil {
		return nil, fmt.Errorf("create todo: %w", err)
	}
	return created, nil
}

func (r *PGXReaderVNextRepository) upsertTodoProjection(ctx context.Context, db database.Querier, todo model.ReaderTodo) (*model.ReaderTodo, error) {
	var existingID uuid.UUID
	lookupErr := db.QueryRow(ctx, `
		SELECT id FROM reader_todos
		WHERE origin_kind=$1 AND origin_host_id=$2
			AND origin_ref->>'block_ref'=$3
			AND COALESCE(origin_ref->>'occurrence','1')=$4 AND deleted_at IS NULL
		FOR UPDATE`, todo.OriginKind, valueOrEmpty(todo.OriginHostID), originBlockRef(todo.OriginRef), originBlockOccurrence(todo.OriginRef)).Scan(&existingID)
	if lookupErr == nil {
		// The host is authoritative for a projected checkbox. A completion
		// command updates the host and projection in one transaction, so replaying
		// the projection cannot reopen a checked item or overwrite a standalone
		// TODO, while an edit made on another device is still reflected here.
		return scanReaderTodo(db.QueryRow(ctx, `
			UPDATE reader_todos SET text=$1,origin_host_kind=$2,origin_ref=$3::jsonb,host_revision=$4,
				done=$5,completed_at=CASE WHEN $5 THEN COALESCE(completed_at,NOW()) ELSE NULL END,updated_at=NOW()
			WHERE id=$6
			RETURNING `+readerTodoColumns, todo.Text, todo.OriginHostKind, rawJSON(todo.OriginRef), todo.HostRevision, todo.Done, existingID))
	}
	if !errors.Is(lookupErr, pgx.ErrNoRows) {
		return nil, lookupErr
	}
	// FOR UPDATE 锁不住还不存在的行：两个并发 host 写请求会同时走到这里各插一次，
	// 输家会拿到 23505。
	// ON CONFLICT 必须完整复述 idx_reader_todos_projection 的部分索引谓词才能被
	// 推断，DO UPDATE 与上面的 UPDATE 分支保持同样的"host 权威"语义。
	return scanReaderTodo(db.QueryRow(ctx, `
		INSERT INTO reader_todos (text,due_at,done,origin_kind,origin_host_kind,origin_host_id,origin_ref,host_revision,completed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,CASE WHEN $3 THEN COALESCE($9::timestamptz,NOW()) ELSE NULL END)
			ON CONFLICT (origin_kind,origin_host_id,(origin_ref->>'block_ref'),COALESCE(origin_ref->>'occurrence','1'))
				WHERE origin_kind <> 'standalone' AND deleted_at IS NULL
			DO UPDATE SET text=EXCLUDED.text,origin_host_kind=EXCLUDED.origin_host_kind,
				origin_ref=EXCLUDED.origin_ref,host_revision=EXCLUDED.host_revision,done=EXCLUDED.done,
				completed_at=CASE WHEN EXCLUDED.done THEN COALESCE(reader_todos.completed_at,NOW()) ELSE NULL END,
				updated_at=NOW()
			RETURNING `+readerTodoColumns, todo.Text, todo.DueAt, todo.Done, todo.OriginKind, todo.OriginHostKind, todo.OriginHostID, rawJSON(todo.OriginRef), todo.HostRevision, todo.CompletedAt))
}

// refreshTodoProjections writes back every projection the authoritative host
// still emits, except those whose key is already tombstoned — a dismissed
// projection must not be resurrected by the next refresh.
func (r *PGXReaderVNextRepository) refreshTodoProjections(ctx context.Context, db database.Querier, todos []model.ReaderTodo, deleted map[string]struct{}) error {
	for _, todo := range todos {
		if todo.OriginKind == "standalone" {
			continue
		}
		if _, ok := deleted[readerTodoProjectionKey(todo.OriginKind, valueOrEmpty(todo.OriginHostID), todo.OriginRef)]; ok {
			continue
		}
		if _, err := r.upsertTodoProjection(ctx, db, todo); err != nil {
			return fmt.Errorf("upsert todo projection: %w", err)
		}
	}
	return nil
}

// dismissStaleTodoProjections soft-deletes the still-active projections whose
// key the host no longer emits. Rows that are already deleted are left alone so
// their tombstone keeps its original timestamp.
func dismissStaleTodoProjections(ctx context.Context, db database.Querier, existing []readerExistingTodoProjection, desired map[string]struct{}) error {
	for _, item := range existing {
		if item.deletedAt != nil {
			continue
		}
		if _, ok := desired[readerTodoProjectionKey(item.origin, valueOrEmpty(item.hostID), item.originRef)]; ok {
			continue
		}
		if _, err := db.Exec(ctx, `UPDATE reader_todos SET deleted_at=COALESCE(deleted_at,NOW()),updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`, item.id); err != nil {
			return fmt.Errorf("dismiss stale todo projection: %w", err)
		}
	}
	return nil
}

type readerExistingTodoProjection struct {
	id        uuid.UUID
	origin    string
	hostID    *string
	originRef []byte
	deletedAt *time.Time
}

func scanReaderExistingTodoProjections(rows pgx.Rows) ([]readerExistingTodoProjection, error) {
	defer rows.Close()
	existing := make([]readerExistingTodoProjection, 0, 32)
	for rows.Next() {
		var item readerExistingTodoProjection
		var deletedAt pgtype.Timestamptz
		if err := rows.Scan(&item.id, &item.origin, &item.hostID, &item.originRef, &deletedAt); err != nil {
			return nil, fmt.Errorf("scan existing todo projection: %w", err)
		}
		if deletedAt.Valid {
			value := deletedAt.Time
			item.deletedAt = &value
		}
		existing = append(existing, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read existing todo projections: %w", err)
	}
	return existing, nil
}

func readerTodoProjectionKey(originKind, hostID string, originRef json.RawMessage) string {
	return originKind + "\x00" + hostID + "\x00" + originBlockRef(originRef) + "\x00" + originBlockOccurrence(originRef)
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func originBlockRef(raw json.RawMessage) string {
	var value struct {
		BlockRef string `json:"block_ref"`
	}
	_ = json.Unmarshal(raw, &value)
	return value.BlockRef
}

func originBlockOccurrence(raw json.RawMessage) string {
	var value struct {
		Occurrence int `json:"occurrence"`
	}
	_ = json.Unmarshal(raw, &value)
	if value.Occurrence <= 0 {
		return "1"
	}
	return strconv.Itoa(value.Occurrence)
}

func (r *PGXReaderVNextRepository) ListTodos(ctx context.Context, after string, limit int) (model.ReaderTodoPage, error) {
	if limit <= 0 || limit > 200 {
		return model.ReaderTodoPage{}, fmt.Errorf("%w: invalid TODO page limit", ErrInvalidReaderCursor)
	}
	cursor, err := parseReaderTodoCursor(after)
	if err != nil {
		return model.ReaderTodoPage{}, err
	}
	rows, err := r.db.Query(ctx, `SELECT `+readerTodoColumns+` FROM reader_todos
		WHERE deleted_at IS NULL
		AND ($1::boolean IS FALSE OR done::integer > $2 OR (done::integer = $2 AND (
			(due_at IS NULL)::integer > $3 OR ((due_at IS NULL)::integer = $3 AND (
				($3 = 0 AND due_at > $4) OR
				(($3 = 1 OR due_at = $4) AND (created_at < $5 OR (created_at = $5 AND id < $6)))
			))
		)))
		ORDER BY done ASC, due_at ASC NULLS LAST, created_at DESC, id DESC LIMIT $7`,
		// 开关必须取自解析后的游标，不能取自原始串：parseReaderTodoCursor 把纯空白
		// 视为"无游标"并返回零值 + nil error，若用 after != "" 判断，`?after=%20`
		// 会带着零值游标启用 keyset 谓词，静默过滤掉全部未完成 TODO。
		cursor.ID != uuid.Nil, boolInt(cursor.Done), boolInt(cursor.DueAt == nil), cursor.DueAt, cursor.CreatedAt, cursor.ID, limit+1)
	if err != nil {
		return model.ReaderTodoPage{}, fmt.Errorf("list todos: %w", err)
	}
	defer rows.Close()
	out := make([]model.ReaderTodo, 0, limit+1)
	for rows.Next() {
		item, err := scanReaderTodo(rows)
		if err != nil {
			return model.ReaderTodoPage{}, err
		}
		out = append(out, *item)
	}
	if err := rows.Err(); err != nil {
		return model.ReaderTodoPage{}, err
	}
	page := model.ReaderTodoPage{Items: out}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = readerTodoCursor(page.Items[len(page.Items)-1])
	}
	return page, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (r *PGXReaderVNextRepository) PatchTodo(ctx context.Context, patch model.ReaderTodoPatch) (*model.ReaderTodo, error) {
	var out *model.ReaderTodo
	err := r.withTx(ctx, func(db database.Querier) error {
		item, err := r.patchTodoOn(ctx, db, patch)
		if err != nil {
			return err
		}
		out = item
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r *PGXReaderVNextRepository) patchTodoOn(ctx context.Context, db database.Querier, patch model.ReaderTodoPatch) (*model.ReaderTodo, error) {
	var originKind string
	var hostRevision int64
	if err := db.QueryRow(ctx, `SELECT origin_kind,host_revision FROM reader_todos WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, patch.ID).Scan(&originKind, &hostRevision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read todo revision: %w", err)
	}

	if originKind == "standalone" {
		if patch.ExpectedHostRevisionSet || patch.ExpectedHostRevision != nil {
			return nil, ErrReaderTodoHostRevisionNotApplicable
		}
		return patchStandaloneTodo(ctx, db, patch)
	}
	if patch.ExpectedHostRevision == nil || *patch.ExpectedHostRevision != hostRevision {
		return nil, ErrRevisionConflict
	}
	if patch.Text != nil || patch.DueAtSet || patch.DueAt != nil || patch.Done == nil {
		return nil, ErrReaderTodoProjectionImmutable
	}
	return r.patchProjectedTodo(ctx, db, patch, hostRevision)
}

func patchStandaloneTodo(ctx context.Context, db database.Querier, patch model.ReaderTodoPatch) (*model.ReaderTodo, error) {
	dueAtSet := patch.DueAtSet || patch.DueAt != nil
	item, err := scanReaderTodo(db.QueryRow(ctx, `
		UPDATE reader_todos SET
			text=COALESCE($1,text), due_at=CASE WHEN $2 THEN $3 ELSE due_at END,
			done=COALESCE($4,done),
			completed_at=CASE WHEN COALESCE($4,done) THEN COALESCE(completed_at,NOW()) ELSE NULL END,
			updated_at=NOW()
			WHERE id=$5 AND deleted_at IS NULL
			RETURNING `+readerTodoColumns, patch.Text, dueAtSet, patch.DueAt, patch.Done, patch.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("patch todo: %w", err)
	}
	return item, nil
}

func (r *PGXReaderVNextRepository) patchProjectedTodo(ctx context.Context, db database.Querier, patch model.ReaderTodoPatch, hostRevision int64) (*model.ReaderTodo, error) {
	var originKind, originHostKind, originHostID string
	var originRef []byte
	if err := db.QueryRow(ctx, `
		SELECT origin_kind,origin_host_kind,origin_host_id,origin_ref
		FROM reader_todos
		WHERE id=$1 AND deleted_at IS NULL
		FOR UPDATE`, patch.ID).Scan(&originKind, &originHostKind, &originHostID, &originRef); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read projected todo host: %w", err)
	}
	if originKind == "standalone" || originHostKind == "" || originHostID == "" {
		return nil, ErrReaderTodoHostMissing
	}
	blockRef := originBlockRef(originRef)
	if blockRef == "" {
		return nil, ErrReaderTodoAnchorNotFound
	}
	occurrence, err := strconv.Atoi(originBlockOccurrence(originRef))
	if err != nil || occurrence <= 0 {
		occurrence = 1
	}
	done := *patch.Done

	switch originKind {
	case "thought":
		return r.patchThoughtTodo(ctx, db, patch.ID, originHostKind, originHostID, blockRef, occurrence, hostRevision, done)
	case "note":
		return r.patchNoteTodo(ctx, db, patch.ID, originHostID, blockRef, occurrence, hostRevision, done)
	default:
		return nil, ErrReaderTodoHostMissing
	}
}

func readerTodoThoughtWritebackOpID(todoID uuid.UUID, hostRevision int64, blockRef string, occurrence int, done bool) string {
	seed := fmt.Sprintf("reader-todo-writeback:%s:%d:%s:%d:%t", todoID, hostRevision, blockRef, occurrence, done)
	return "todo-" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(seed)).String()
}

func (r *PGXReaderVNextRepository) patchThoughtTodo(ctx context.Context, db database.Querier, todoID uuid.UUID, hostKind, hostID, blockRef string, occurrence int, hostRevision int64, done bool) (*model.ReaderTodo, error) {
	var body, source string
	var target, quote []byte
	var linkID *uuid.UUID
	var currentSequence int64
	if err := db.QueryRow(ctx, `
		SELECT body,target,quote,source,link_id,last_sequence
		FROM reader_thoughts
		WHERE id=$1 AND deleted=false
		FOR UPDATE`, hostID).Scan(&body, &target, &quote, &source, &linkID, &currentSequence); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrReaderTodoHostMissing
		}
		return nil, fmt.Errorf("read thought todo host: %w", err)
	}
	if currentSequence != hostRevision {
		return nil, ErrRevisionConflict
	}
	updated := readertext.Update(body, blockRef, occurrence, done)
	switch updated.Status {
	case readertext.NotFound:
		return nil, ErrReaderTodoAnchorNotFound
	case readertext.Ambiguous:
		return nil, ErrReaderTodoAnchorAmbiguous
	}
	if updated.Source == body {
		return r.updateProjectedTodo(ctx, db, todoID, updated.Block.Text, done, hostRevision)
	}
	linkIDValue := ""
	if linkID != nil {
		linkIDValue = linkID.String()
	}
	payload, err := json.Marshal(struct {
		Body   string          `json:"body"`
		Quote  json.RawMessage `json:"quote,omitempty"`
		Source string          `json:"source"`
		LinkID string          `json:"link_id,omitempty"`
	}{Body: updated.Source, Quote: quote, Source: source, LinkID: linkIDValue})
	if err != nil {
		return nil, fmt.Errorf("encode thought todo writeback: %w", err)
	}
	thoughtOp, sequence, duplicate, err := r.appendDerivedThoughtOp(ctx, db, model.ReaderThoughtOp{
		OpID:          readerTodoThoughtWritebackOpID(todoID, hostRevision, blockRef, occurrence, done),
		DeviceID:      "reader-todo",
		OperationKind: "update",
		AnnotationID:  hostID,
		HostKind:      hostKind,
		HostID:        hostID,
		Target:        target,
		Payload:       payload,
	})
	if err != nil {
		return nil, fmt.Errorf("append thought todo writeback: %w", err)
	}
	if !duplicate {
		if err := r.materializeThought(ctx, db, thoughtOp, sequence); err != nil {
			return nil, err
		}
	}
	return r.updateProjectedTodo(ctx, db, todoID, updated.Block.Text, done, sequence)
}

func (r *PGXReaderVNextRepository) patchNoteTodo(ctx context.Context, db database.Querier, todoID uuid.UUID, hostID, blockRef string, occurrence int, hostRevision int64, done bool) (*model.ReaderTodo, error) {
	noteID, err := uuid.Parse(hostID)
	if err != nil {
		return nil, ErrReaderTodoHostMissing
	}
	var title, content string
	var draftContent pgtype.Text
	var currentRevision int64
	if err := db.QueryRow(ctx, `
		SELECT title,published_content,published_revision,draft_content
		FROM reader_notes
		WHERE id=$1 AND deleted_at IS NULL
		FOR UPDATE`, noteID).Scan(&title, &content, &currentRevision, &draftContent); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrReaderTodoHostMissing
		}
		return nil, fmt.Errorf("read note todo host: %w", err)
	}
	if currentRevision != hostRevision {
		return nil, ErrRevisionConflict
	}
	if draftContent.Valid && draftContent.String != content {
		return nil, ErrRevisionConflict
	}
	updated := readertext.Update(content, blockRef, occurrence, done)
	switch updated.Status {
	case readertext.NotFound:
		return nil, ErrReaderTodoAnchorNotFound
	case readertext.Ambiguous:
		return nil, ErrReaderTodoAnchorAmbiguous
	}
	if updated.Source == content {
		return r.updateProjectedTodo(ctx, db, todoID, updated.Block.Text, done, hostRevision)
	}
	newRevision := currentRevision + 1
	result, err := db.Exec(ctx, `
		UPDATE reader_notes
		SET published_content=$1,published_revision=$2,draft_content=NULL,
			draft_revision=draft_revision+1,draft_updated_at=NULL,updated_at=NOW()
		WHERE id=$3 AND published_revision=$4 AND deleted_at IS NULL`, updated.Source, newRevision, noteID, currentRevision)
	if err != nil {
		return nil, fmt.Errorf("write note todo host: %w", err)
	}
	if result.RowsAffected() == 0 {
		return nil, ErrRevisionConflict
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO reader_note_history (note_id,revision,title,content,reanchor_ops)
		VALUES ($1,$2,$3,$4,'[]'::jsonb)
		ON CONFLICT (note_id,revision) DO NOTHING`, noteID, newRevision, title, updated.Source); err != nil {
		return nil, fmt.Errorf("record note todo history: %w", err)
	}
	if err := r.markThoughtHostTombstonesOn(ctx, db, "note", noteID.String(), "note_todo_updated"); err != nil {
		return nil, err
	}
	if err := r.replaceNoteTodoProjectionsOn(ctx, db, noteID); err != nil {
		return nil, err
	}
	return r.updateProjectedTodo(ctx, db, todoID, updated.Block.Text, done, newRevision)
}

func (r *PGXReaderVNextRepository) updateProjectedTodo(ctx context.Context, db database.Querier, todoID uuid.UUID, text string, done bool, hostRevision int64) (*model.ReaderTodo, error) {
	item, err := scanReaderTodo(db.QueryRow(ctx, `
		UPDATE reader_todos SET
			text=$1,done=$2,host_revision=$3,
			completed_at=CASE WHEN $2 THEN COALESCE(completed_at,NOW()) ELSE NULL END,
			updated_at=NOW()
		WHERE id=$4 AND deleted_at IS NULL
		RETURNING `+readerTodoColumns, text, done, hostRevision, todoID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update projected todo: %w", err)
	}
	return item, nil
}

func (r *PGXReaderVNextRepository) DeleteTodo(ctx context.Context, id uuid.UUID) error {
	return r.withTx(ctx, func(db database.Querier) error {
		var originKind string
		if err := db.QueryRow(ctx, `
			SELECT origin_kind FROM reader_todos
			WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&originKind); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read todo for delete: %w", err)
		}
		if originKind != "standalone" {
			return ErrReaderTodoProjectionImmutable
		}
		result, err := db.Exec(ctx, `
			UPDATE reader_todos SET deleted_at=NOW(),updated_at=NOW()
			WHERE id=$1 AND deleted_at IS NULL`, id)
		if err != nil {
			return fmt.Errorf("delete todo: %w", err)
		}
		if result.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}
