package dbintegration

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
	readerservice "webtag/internal/service"
)

type readerTodoProjectionFact struct {
	Text         string
	Done         bool
	OriginKind   string
	OriginHostID string
	OriginRef    string
	HostRevision int64
	Live         bool
}

// readReaderTodoProjectionFacts reads every projection row, tombstones
// included, reduced to the fields a reconcile would recompute. Volatile
// columns are excluded on purpose: a repair rewrites updated_at even when it
// changes nothing, and that is not drift.
func readReaderTodoProjectionFacts(t *testing.T, pool *pgxpool.Pool) []readerTodoProjectionFact {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT text,done,origin_kind,COALESCE(origin_host_id,''),origin_ref::text,host_revision,deleted_at IS NULL
		FROM reader_todos
		WHERE origin_kind <> 'standalone'`)
	if err != nil {
		t.Fatalf("read projection facts: %v", err)
	}
	defer rows.Close()
	out := make([]readerTodoProjectionFact, 0, 8)
	for rows.Next() {
		var fact readerTodoProjectionFact
		if err := rows.Scan(&fact.Text, &fact.Done, &fact.OriginKind, &fact.OriginHostID, &fact.OriginRef, &fact.HostRevision, &fact.Live); err != nil {
			t.Fatalf("scan projection fact: %v", err)
		}
		out = append(out, fact)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate projection facts: %v", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OriginHostID != out[j].OriginHostID {
			return out[i].OriginHostID < out[j].OriginHostID
		}
		return out[i].OriginRef < out[j].OriginRef
	})
	return out
}

func liveReaderTodoTexts(facts []readerTodoProjectionFact) []string {
	out := make([]string, 0, len(facts))
	for _, fact := range facts {
		if fact.Live {
			out = append(out, fact.Text)
		}
	}
	sort.Strings(out)
	return out
}

// assertProjectionsNeedNoRepair is the write-time maintenance contract stated
// as an executable claim: once a command commits, running the whole-installation
// repair must find nothing to change. A write path that forgot to maintain its
// host shows up here as a diff, which is why every step of the lifecycle below
// calls it instead of asserting only the rows it expects.
func assertProjectionsNeedNoRepair(t *testing.T, pool *pgxpool.Pool, service *readerservice.ReaderVNextService, label string) []readerTodoProjectionFact {
	t.Helper()
	before := readReaderTodoProjectionFacts(t, pool)
	if err := service.RepairProjectedTodos(t.Context()); err != nil {
		t.Fatalf("%s: repair projections: %v", label, err)
	}
	after := readReaderTodoProjectionFacts(t, pool)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("%s left the projection out of date\n before=%#v\n  after=%#v", label, before, after)
	}
	return after
}

func assertLiveTodoTexts(t *testing.T, facts []readerTodoProjectionFact, label string, want ...string) {
	t.Helper()
	got := liveReaderTodoTexts(facts)
	if want == nil {
		want = []string{}
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: live projections = %v, want %v", label, got, want)
	}
}

// TestReaderNoteTodoProjectionIsMaintainedOnWrite walks a Note through create,
// draft, publish, checkbox writeback, revision restore, delete and restore and
// requires the projection to be correct at every commit — with no read-path
// reconcile in between.
func TestReaderNoteTodoProjectionIsMaintainedOnWrite(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	service := readerservice.NewReaderVNextService(reader, nil)

	note, err := reader.CreateNote(ctx, model.ReaderNote{Title: "Write-path note", PublishedContent: "- [ ] alpha"})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	facts := assertProjectionsNeedNoRepair(t, pool, service, "create note")
	assertLiveTodoTexts(t, facts, "create note", "alpha")

	if _, err := reader.SaveNoteDraft(ctx, model.ReaderNoteDraftCommand{
		NoteID: note.ID, Content: "- [ ] alpha\n- [ ] beta", ExpectedDraftRevision: note.DraftRevision,
	}); err != nil {
		t.Fatalf("SaveNoteDraft: %v", err)
	}
	facts = assertProjectionsNeedNoRepair(t, pool, service, "save draft")
	assertLiveTodoTexts(t, facts, "save draft", "alpha")

	published, err := reader.PublishNote(ctx, model.ReaderNotePublishCommand{
		NoteID: note.ID, ExpectedDraftRevision: note.DraftRevision + 1, ExpectedPublishedRevision: note.PublishedRevision,
	})
	if err != nil {
		t.Fatalf("PublishNote: %v", err)
	}
	facts = assertProjectionsNeedNoRepair(t, pool, service, "publish note")
	assertLiveTodoTexts(t, facts, "publish note", "alpha", "beta")

	todo := findLiveProjectedTodo(t, pool, "note", note.ID.String(), "beta")
	ticked, err := reader.PatchTodo(ctx, model.ReaderTodoPatch{
		ID: todo, Done: boolPointerForTodoWritePath(true), ExpectedHostRevision: &published.PublishedRevision,
	})
	if err != nil {
		t.Fatalf("PatchTodo: %v", err)
	}
	if !ticked.Done {
		t.Fatalf("PatchTodo() = %#v, want done", ticked)
	}
	facts = assertProjectionsNeedNoRepair(t, pool, service, "tick checkbox")
	assertLiveTodoTexts(t, facts, "tick checkbox", "alpha", "beta")
	assertProjectedTodoDone(t, facts, "tick checkbox", "beta", true)

	current, err := reader.GetNote(ctx, note.ID)
	if err != nil {
		t.Fatalf("GetNote: %v", err)
	}
	if _, err := reader.RestoreNoteRevision(ctx, model.ReaderNoteRestoreCommand{
		NoteID: note.ID, Revision: published.PublishedRevision,
		ExpectedDraftRevision: current.DraftRevision, ExpectedPublishedRevision: current.PublishedRevision,
	}); err != nil {
		t.Fatalf("RestoreNoteRevision: %v", err)
	}
	facts = assertProjectionsNeedNoRepair(t, pool, service, "restore revision")
	assertLiveTodoTexts(t, facts, "restore revision", "alpha", "beta")
	assertProjectedTodoDone(t, facts, "restore revision", "beta", false)

	if err := reader.DeleteNote(ctx, note.ID); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}
	facts = assertProjectionsNeedNoRepair(t, pool, service, "delete note")
	assertLiveTodoTexts(t, facts, "delete note")

	// Restoring the note restores the source, not the dismissed projections:
	// a soft-deleted projection is a tombstone for every path, and the repair
	// agrees, which is exactly what assertProjectionsNeedNoRepair checks.
	if err := reader.RestoreNote(ctx, note.ID); err != nil {
		t.Fatalf("RestoreNote: %v", err)
	}
	facts = assertProjectionsNeedNoRepair(t, pool, service, "restore note")
	assertLiveTodoTexts(t, facts, "restore note")
}

// TestReaderThoughtTodoProjectionIsMaintainedOnWrite does the same for a
// Thought hosted on a saved Link, including the Link delete path whose Thought
// tombstones are written by a database trigger rather than by Go.
func TestReaderThoughtTodoProjectionIsMaintainedOnWrite(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	service := readerservice.NewReaderVNextService(reader, nil)
	links := repository.NewPGXLinkRepository(pool)

	linkID := seedReaderVNextSavedLink(t, pool, "https://todo.example/"+uuid.NewString(), "Write-path link", "body", "summary")
	thoughtID := "thought-todo-" + uuid.NewString()
	appendThoughtWithBody(t, ctx, reader, thoughtID, linkID, 1, "- [ ] read the paper")
	facts := assertProjectionsNeedNoRepair(t, pool, service, "create thought")
	assertLiveTodoTexts(t, facts, "create thought", "read the paper")

	appendThoughtWithBody(t, ctx, reader, thoughtID, linkID, 2, "- [ ] read the paper\n- [ ] write the notes")
	facts = assertProjectionsNeedNoRepair(t, pool, service, "update thought")
	assertLiveTodoTexts(t, facts, "update thought", "read the paper", "write the notes")

	if err := links.DeleteLifecycle(ctx, linkID); err != nil {
		t.Fatalf("DeleteLifecycle: %v", err)
	}
	facts = assertProjectionsNeedNoRepair(t, pool, service, "delete link host")
	assertLiveTodoTexts(t, facts, "delete link host")
}

// TestReaderThoughtDeleteRetiresTodoProjectionOnWrite keeps the plain Thought
// delete path separate from the Link lifecycle so a regression in either is
// attributable.
func TestReaderThoughtDeleteRetiresTodoProjectionOnWrite(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	service := readerservice.NewReaderVNextService(reader, nil)

	linkID := seedReaderVNextSavedLink(t, pool, "https://todo.example/"+uuid.NewString(), "Deleted thought link", "body", "summary")
	thoughtID := "thought-todo-" + uuid.NewString()
	appendThoughtWithBody(t, ctx, reader, thoughtID, linkID, 1, "- [ ] disappear with me")
	facts := assertProjectionsNeedNoRepair(t, pool, service, "create thought")
	assertLiveTodoTexts(t, facts, "create thought", "disappear with me")

	if _, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
		OpID:          "delete-" + uuid.NewString(),
		DeviceID:      "todo-write-path",
		LogicalClock:  2,
		OperationKind: "delete",
		AnnotationID:  thoughtID,
		HostKind:      "link",
		HostID:        linkID.String(),
		Target:        readerVNextJSON(t, map[string]any{"kind": "saved-content", "host_id": linkID.String(), "version": map[string]any{"content_revision": 1}}),
		Payload:       readerVNextJSON(t, map[string]any{}),
	}}); err != nil {
		t.Fatalf("delete thought: %v", err)
	}
	facts = assertProjectionsNeedNoRepair(t, pool, service, "delete thought")
	assertLiveTodoTexts(t, facts, "delete thought")
}

func appendThoughtWithBody(
	t *testing.T,
	ctx context.Context,
	reader *repository.PGXReaderVNextRepository,
	thoughtID string,
	linkID uuid.UUID,
	clock int64,
	body string,
) {
	t.Helper()
	kind := "add"
	if clock > 1 {
		kind = "update"
	}
	if _, err := reader.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
		OpID:          "op-" + uuid.NewString(),
		DeviceID:      "todo-write-path",
		LogicalClock:  clock,
		OperationKind: kind,
		AnnotationID:  thoughtID,
		HostKind:      "link",
		HostID:        linkID.String(),
		Target:        readerVNextJSON(t, map[string]any{"kind": "saved-content", "host_id": linkID.String(), "version": map[string]any{"content_revision": 1}}),
		Payload:       readerVNextJSON(t, map[string]any{"body": body, "source": "user", "link_id": linkID.String()}),
	}}); err != nil {
		t.Fatalf("append thought %s: %v", thoughtID, err)
	}
}

func findLiveProjectedTodo(t *testing.T, pool *pgxpool.Pool, originKind, hostID, text string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(), `
		SELECT id FROM reader_todos
		WHERE origin_kind=$1 AND origin_host_id=$2 AND text=$3 AND deleted_at IS NULL`, originKind, hostID, text).Scan(&id); err != nil {
		t.Fatalf("find projected TODO %s/%s %q: %v", originKind, hostID, text, err)
	}
	return id
}

func boolPointerForTodoWritePath(value bool) *bool { return &value }

func assertProjectedTodoDone(t *testing.T, facts []readerTodoProjectionFact, label, text string, want bool) {
	t.Helper()
	for _, fact := range facts {
		if fact.Live && fact.Text == text {
			if fact.Done != want {
				t.Fatalf("%s: projection %q done = %t, want %t", label, text, fact.Done, want)
			}
			return
		}
	}
	t.Fatalf("%s: no live projection for %q", label, text)
}
