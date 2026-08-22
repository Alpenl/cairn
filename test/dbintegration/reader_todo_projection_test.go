package dbintegration

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
	readerservice "webtag/internal/service"
)

type readerTodoProjectionRow struct {
	ID        uuid.UUID
	Text      string
	DeletedAt *time.Time
}

// readReaderTodoProjections returns every projection row for one host,
// including the dismissed ones. A regression that resurrects a dismissed
// projection shows up as a second row for the same block, so the assertions
// below must never filter the tombstones out.
func readReaderTodoProjections(t *testing.T, pool *pgxpool.Pool, originKind, hostID string) []readerTodoProjectionRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT id,text,deleted_at
		FROM reader_todos
		WHERE origin_kind=$1 AND origin_host_id=$2
		ORDER BY created_at,id`, originKind, hostID)
	if err != nil {
		t.Fatalf("read %s/%s projections: %v", originKind, hostID, err)
	}
	defer rows.Close()
	out := make([]readerTodoProjectionRow, 0, 4)
	for rows.Next() {
		var item readerTodoProjectionRow
		if err := rows.Scan(&item.ID, &item.Text, &item.DeletedAt); err != nil {
			t.Fatalf("scan %s/%s projection: %v", originKind, hostID, err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s/%s projections: %v", originKind, hostID, err)
	}
	return out
}

// TestReaderDismissedTodoProjectionStaysDismissed is the PostgreSQL regression
// for the Home/Todos split brain: Home used to reconcile against the live
// projections only, so a dismissed row looked absent and was re-inserted the
// next time the user opened Home. Todos never resurrected it, so the same
// installation reported two different TODO lists depending on which screen was
// opened last.
func TestReaderDismissedTodoProjectionStaysDismissed(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	service := readerservice.NewReaderApplications(postgresReaderStores(reader), nil).Todos

	note, err := reader.CreateNote(ctx, model.ReaderNote{
		Title: "Dismissed projection host", PublishedContent: "- [ ] dismissed by the user",
	})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	projections := readReaderTodoProjections(t, pool, "note", note.ID.String())
	if len(projections) != 1 || projections[0].DeletedAt != nil {
		t.Fatalf("projections after creating the note = %#v, want one live projection", projections)
	}
	dismissed := projections[0]

	if _, err := pool.Exec(ctx, `UPDATE reader_todos SET deleted_at=NOW(),updated_at=NOW() WHERE id=$1`, dismissed.ID); err != nil {
		t.Fatalf("dismiss projection: %v", err)
	}
	afterDismissal := readReaderTodoProjections(t, pool, "note", note.ID.String())
	if len(afterDismissal) != 1 || afterDismissal[0].DeletedAt == nil {
		t.Fatalf("projections after dismissal = %#v, want one tombstone", afterDismissal)
	}
	tombstonedAt := *afterDismissal[0].DeletedAt

	for _, tt := range []struct {
		name string
		open func(t *testing.T)
	}{
		{name: "home", open: func(t *testing.T) {
			t.Helper()
			aggregate, err := reader.LoadHomeAggregate(ctx)
			if err != nil {
				t.Fatalf("LoadHomeAggregate: %v", err)
			}
			for _, todo := range aggregate.Todos {
				if todo.OriginHostID != nil && *todo.OriginHostID == note.ID.String() {
					t.Fatalf("Home listed the dismissed projection: %#v", todo)
				}
			}
			if aggregate.Counts["todos"] != 0 {
				t.Fatalf("Home todo count = %d, want 0", aggregate.Counts["todos"])
			}
		}},
		{name: "todos", open: func(t *testing.T) {
			t.Helper()
			page, err := service.ListTodos(ctx, "", 200)
			if err != nil {
				t.Fatalf("ListTodos: %v", err)
			}
			for _, todo := range page.Items {
				if todo.OriginHostID != nil && *todo.OriginHostID == note.ID.String() {
					t.Fatalf("Todos listed the dismissed projection: %#v", todo)
				}
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.open(t)
			rows := readReaderTodoProjections(t, pool, "note", note.ID.String())
			if len(rows) != 1 {
				t.Fatalf("opening %s produced %#v, want the single tombstone", tt.name, rows)
			}
			if rows[0].ID != dismissed.ID || rows[0].DeletedAt == nil || !rows[0].DeletedAt.Equal(tombstonedAt) {
				t.Fatalf("opening %s changed the tombstone: %#v, want %s dismissed at %s", tt.name, rows[0], dismissed.ID, tombstonedAt)
			}
		})
	}
}

// TestReaderDismissedTodoProjectionSurvivesSourceRewrite verifies that normal
// note publishes retire a removed checkbox without resurrecting its tombstone
// when the same checkbox returns later.
func TestReaderDismissedTodoProjectionSurvivesSourceRewrite(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	service := readerservice.NewReaderApplications(postgresReaderStores(reader), nil).Todos

	note, err := reader.CreateNote(ctx, model.ReaderNote{
		Title: "Rewritten projection host", PublishedContent: "- [ ] rewritten by the source",
	})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if len(readReaderTodoProjections(t, pool, "note", note.ID.String())) != 1 {
		t.Fatal("creating the note did not project its checkbox")
	}

	draft, err := reader.SaveNoteDraft(ctx, model.ReaderNoteDraftCommand{
		NoteID: note.ID, Content: "no checklist left", ExpectedDraftRevision: note.DraftRevision,
	})
	if err != nil {
		t.Fatalf("save draft without checkbox: %v", err)
	}
	published, err := reader.PublishNote(ctx, model.ReaderNotePublishCommand{
		NoteID: note.ID, ExpectedDraftRevision: draft.DraftRevision, ExpectedPublishedRevision: note.PublishedRevision,
	})
	if err != nil {
		t.Fatalf("publish without checkbox: %v", err)
	}
	dismissed := readReaderTodoProjections(t, pool, "note", note.ID.String())
	if len(dismissed) != 1 || dismissed[0].DeletedAt == nil {
		t.Fatalf("projections after dropping the checkbox = %#v, want one tombstone", dismissed)
	}

	restoredDraft, err := reader.SaveNoteDraft(ctx, model.ReaderNoteDraftCommand{
		NoteID: note.ID, Content: "- [ ] rewritten by the source", ExpectedDraftRevision: published.DraftRevision,
	})
	if err != nil {
		t.Fatalf("save restored checkbox: %v", err)
	}
	if _, err := reader.PublishNote(ctx, model.ReaderNotePublishCommand{
		NoteID: note.ID, ExpectedDraftRevision: restoredDraft.DraftRevision, ExpectedPublishedRevision: published.PublishedRevision,
	}); err != nil {
		t.Fatalf("publish restored checkbox: %v", err)
	}
	if _, err := reader.LoadHomeAggregate(ctx); err != nil {
		t.Fatalf("LoadHomeAggregate: %v", err)
	}
	if _, err := service.ListTodos(ctx, "", 200); err != nil {
		t.Fatalf("ListTodos after restoring the checkbox: %v", err)
	}
	after := readReaderTodoProjections(t, pool, "note", note.ID.String())
	if len(after) != 1 || after[0].DeletedAt == nil {
		t.Fatalf("projections after reopening Home and Todos = %#v, want the tombstone to hold", after)
	}
}
