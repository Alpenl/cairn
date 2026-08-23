package dbintegration

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/dto"
	"webtag/internal/handler"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type readerTodoRevisionState struct {
	Text         string
	DueAt        *time.Time
	Done         bool
	HostRevision int64
	CompletedAt  *time.Time
	UpdatedAt    time.Time
}

func readReaderTodoRevisionState(t *testing.T, pool *pgxpool.Pool, todoID uuid.UUID) readerTodoRevisionState {
	t.Helper()
	var state readerTodoRevisionState
	if err := pool.QueryRow(t.Context(), `
		SELECT text,due_at,done,host_revision,completed_at,updated_at
		FROM reader_todos
		WHERE id=$1`, todoID).Scan(
		&state.Text,
		&state.DueAt,
		&state.Done,
		&state.HostRevision,
		&state.CompletedAt,
		&state.UpdatedAt,
	); err != nil {
		t.Fatalf("read TODO %s state: %v", todoID, err)
	}
	return state
}

type readerNoteRevisionState struct {
	Content   string
	Revision  int64
	UpdatedAt time.Time
}

func readReaderNoteRevisionState(t *testing.T, pool *pgxpool.Pool, noteID uuid.UUID) readerNoteRevisionState {
	t.Helper()
	var state readerNoteRevisionState
	if err := pool.QueryRow(t.Context(), `
		SELECT published_content,published_revision,updated_at
		FROM reader_notes
		WHERE id=$1`, noteID).Scan(
		&state.Content,
		&state.Revision,
		&state.UpdatedAt,
	); err != nil {
		t.Fatalf("read note %s state: %v", noteID, err)
	}
	return state
}

// readerTodoPatchState is deliberately wider than the immediate TODO
// row. A projected TODO write can change its source association, note host,
// revision history, and timestamps in one transaction; every rejected PATCH
// path must leave all of those installation-owned records untouched.
type readerTodoPatchState struct {
	Todos       []byte
	Notes       []byte
	NoteHistory []byte
}

func readReaderTodoPatchState(t *testing.T, pool *pgxpool.Pool) readerTodoPatchState {
	t.Helper()
	var state readerTodoPatchState
	if err := pool.QueryRow(t.Context(), `
		SELECT
			COALESCE((
				SELECT jsonb_agg(jsonb_build_object(
					'id',id,'text',text,'due_at',due_at,'done',done,
					'origin_kind',origin_kind,'origin_host_kind',origin_host_kind,
					'origin_host_id',origin_host_id,'origin_ref',origin_ref,
					'host_revision',host_revision,'completed_at',completed_at,
					'deleted_at',deleted_at,'updated_at',updated_at
				) ORDER BY id)
				FROM reader_todos
			), '[]'::jsonb),
			COALESCE((
				SELECT jsonb_agg(jsonb_build_object(
					'id',id,'title',title,'published_content',published_content,
					'published_revision',published_revision,'draft_content',draft_content,
					'draft_revision',draft_revision,'draft_updated_at',draft_updated_at,
					'deleted_at',deleted_at,'updated_at',updated_at
				) ORDER BY id)
				FROM reader_notes
			), '[]'::jsonb),
			COALESCE((
				SELECT jsonb_agg(jsonb_build_object(
					'id',id,'note_id',note_id,'revision',revision,'title',title,
					'content',content,'reanchor_ops',reanchor_ops,'created_at',created_at
				) ORDER BY id)
				FROM reader_note_history
			), '[]'::jsonb)`).Scan(&state.Todos, &state.Notes, &state.NoteHistory); err != nil {
		t.Fatalf("read TODO patch state: %v", err)
	}
	return state
}

func assertReaderTodoPatchStateUnchanged(t *testing.T, label string, before, after readerTodoPatchState) {
	t.Helper()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("%s wrote installation-owned TODO patch state\n before=%#v\n  after=%#v", label, before, after)
	}
}

func TestReaderTodoHostRevisionPostgresContract(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	dueAt := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	standalone, err := reader.CreateTodo(ctx, model.ReaderTodo{Text: "standalone before CAS", DueAt: &dueAt, OriginKind: "standalone"})
	if err != nil {
		t.Fatalf("CreateTodo: %v", err)
	}

	changedText := "this must never be written by a rejected PATCH"
	changedDueAt := dueAt.Add(24 * time.Hour)
	done := true
	negative, zero, positive := int64(-1), int64(0), int64(99)
	standaloneRejects := []struct {
		name                    string
		expectedHostRevision    *int64
		expectedHostRevisionSet bool
	}{
		{name: "explicit negative", expectedHostRevision: &negative},
		{name: "explicit zero", expectedHostRevision: &zero},
		{name: "explicit positive", expectedHostRevision: &positive},
		{name: "explicit null", expectedHostRevisionSet: true},
	}
	for _, tt := range standaloneRejects {
		t.Run("standalone/"+tt.name, func(t *testing.T) {
			before := readReaderTodoRevisionState(t, pool, standalone.ID)
			_, err := reader.PatchTodo(ctx, model.ReaderTodoPatch{
				ID:                      standalone.ID,
				Text:                    &changedText,
				DueAt:                   &changedDueAt,
				DueAtSet:                true,
				Done:                    &done,
				ExpectedHostRevision:    tt.expectedHostRevision,
				ExpectedHostRevisionSet: tt.expectedHostRevisionSet,
			})
			if !errors.Is(err, repository.ErrReaderTodoHostRevisionNotApplicable) {
				t.Fatalf("PatchTodo() error = %v, want ErrReaderTodoHostRevisionNotApplicable", err)
			}
			after := readReaderTodoRevisionState(t, pool, standalone.ID)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected standalone PATCH wrote state\n before=%#v\n  after=%#v", before, after)
			}
		})
	}

	updatedStandalone, err := reader.PatchTodo(ctx, model.ReaderTodoPatch{
		ID:       standalone.ID,
		Text:     &changedText,
		DueAt:    &changedDueAt,
		DueAtSet: true,
		Done:     &done,
	})
	if err != nil {
		t.Fatalf("omitted standalone revision PatchTodo: %v", err)
	}
	if updatedStandalone.Text != changedText || updatedStandalone.DueAt == nil || !updatedStandalone.DueAt.Equal(changedDueAt) || !updatedStandalone.Done || updatedStandalone.CompletedAt == nil {
		t.Fatalf("omitted standalone revision PatchTodo = %#v, want mutable update", updatedStandalone)
	}

	note, err := reader.CreateNote(ctx, model.ReaderNote{Title: "Projected TODO host", PublishedContent: "- [ ] projected task"})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	todos, err := reader.ListTodos(ctx, "", 200)
	if err != nil {
		t.Fatalf("ListTodos: %v", err)
	}
	var projected *model.ReaderTodo
	for index := range todos.Items {
		if todos.Items[index].OriginKind == "note" && todos.Items[index].OriginHostID != nil && *todos.Items[index].OriginHostID == note.ID.String() {
			projected = &todos.Items[index]
			break
		}
	}
	if projected == nil {
		t.Fatalf("ListTodos = %#v, want note projection", todos)
	}

	t.Run("missing TODO lookup leaves installation state unchanged", func(t *testing.T) {
		before := readReaderTodoPatchState(t, pool)
		_, err := reader.PatchTodo(ctx, model.ReaderTodoPatch{
			ID:                   uuid.New(),
			Text:                 &changedText,
			DueAt:                &changedDueAt,
			DueAtSet:             true,
			Done:                 &done,
			ExpectedHostRevision: &negative,
		})
		if !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("missing PatchTodo() error = %v, want ErrNotFound", err)
		}
		assertReaderTodoPatchStateUnchanged(t, "missing TODO PATCH", before, readReaderTodoPatchState(t, pool))
	})

	positiveStale := projected.HostRevision + 1
	projectedRejects := []struct {
		name                 string
		expectedHostRevision *int64
	}{
		{name: "omitted"},
		{name: "negative", expectedHostRevision: &negative},
		{name: "zero", expectedHostRevision: &zero},
		{name: "positive stale", expectedHostRevision: &positiveStale},
	}
	for _, tt := range projectedRejects {
		t.Run("projected/"+tt.name, func(t *testing.T) {
			before := readReaderTodoPatchState(t, pool)
			_, err := reader.PatchTodo(ctx, model.ReaderTodoPatch{
				ID:                   projected.ID,
				Done:                 &done,
				ExpectedHostRevision: tt.expectedHostRevision,
			})
			if !errors.Is(err, repository.ErrRevisionConflict) {
				t.Fatalf("PatchTodo() error = %v, want ErrRevisionConflict", err)
			}
			assertReaderTodoPatchStateUnchanged(t, "rejected projected PATCH", before, readReaderTodoPatchState(t, pool))
		})
	}

	t.Run("projected/explicit null from JSON transport", func(t *testing.T) {
		before := readReaderTodoPatchState(t, pool)
		gin.SetMode(gin.TestMode)
		router := gin.New()
		readerService := postgresReaderApplications(reader).Todos
		handler.RegisterReaderRoutes(router, handler.ReaderRoutes{Todos: handler.NewReaderTodoRoutes(readerService)})
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPatch, "/api/todos/"+projected.ID.String(), strings.NewReader(`{"done":true,"expected_host_revision":null}`)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, request)
		if response.Code != http.StatusConflict {
			t.Fatalf("JSON null PATCH status = %d, want 409; body=%s", response.Code, response.Body.String())
		}
		var envelope dto.ErrorResponse
		if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
			t.Fatalf("decode JSON null PATCH error: %v; body=%s", err, response.Body.String())
		}
		if envelope.Error.ErrorCode != httperr.CodeRevisionConflict {
			t.Fatalf("JSON null PATCH error code = %q, want %q", envelope.Error.ErrorCode, httperr.CodeRevisionConflict)
		}
		assertReaderTodoPatchStateUnchanged(t, "JSON null projected PATCH", before, readReaderTodoPatchState(t, pool))
	})

	matching := projected.HostRevision
	completed, err := reader.PatchTodo(ctx, model.ReaderTodoPatch{
		ID:                   projected.ID,
		Done:                 &done,
		ExpectedHostRevision: &matching,
	})
	if err != nil {
		t.Fatalf("matching projected PatchTodo: %v", err)
	}
	if !completed.Done || completed.HostRevision != matching+1 {
		t.Fatalf("matching projected PatchTodo = %#v, want done at host revision %d", completed, matching+1)
	}
	afterFirstWrite := readReaderNoteRevisionState(t, pool, note.ID)
	if afterFirstWrite.Revision != matching+1 || !strings.Contains(afterFirstWrite.Content, "[x] projected task") {
		t.Fatalf("note after matching projected PatchTodo = %#v, want checked revision %d", afterFirstWrite, matching+1)
	}

	idempotentRevision := completed.HostRevision
	replayed, err := reader.PatchTodo(ctx, model.ReaderTodoPatch{
		ID:                   projected.ID,
		Done:                 &done,
		ExpectedHostRevision: &idempotentRevision,
	})
	if err != nil {
		t.Fatalf("idempotent projected PatchTodo: %v", err)
	}
	if !replayed.Done || replayed.HostRevision != idempotentRevision {
		t.Fatalf("idempotent projected PatchTodo = %#v, want unchanged host revision %d", replayed, idempotentRevision)
	}
	afterReplay := readReaderNoteRevisionState(t, pool, note.ID)
	if !reflect.DeepEqual(afterReplay, afterFirstWrite) {
		t.Fatalf("idempotent projected PATCH rewrote host\n before=%#v\n  after=%#v", afterFirstWrite, afterReplay)
	}
}
