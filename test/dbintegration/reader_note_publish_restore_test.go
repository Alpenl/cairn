package dbintegration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// These tests intentionally use a real database: the Note transaction relies
// on PostgreSQL row locks, sequences, transactions, and rollback rather than mock call
// order. Keep this fixture separate from the broad Reader cross-surface chain.
func TestIssue83PublishEmptyAndNoOpLeaveAllStateUntouched(t *testing.T) {
	t.Run("canonical ASCII empty", func(t *testing.T) {
		pool, repo, ctx := issue83Repository(t)
		note := issue83SeedNote(t, pool, "Existing", "Published", " \t\r\n", 1)
		before := issue83SnapshotState(t, pool, note.ID)

		_, err := repo.PublishNote(ctx, model.ReaderNotePublishCommand{
			NoteID: note.ID, ExpectedDraftRevision: 1, ExpectedPublishedRevision: 1,
		})
		if !errors.Is(err, repository.ErrReaderNoteContentEmpty) {
			t.Fatalf("PublishNote() error = %v, want ErrReaderNoteContentEmpty", err)
		}
		issue83AssertSnapshotEqual(t, issue83SnapshotState(t, pool, note.ID), before)
	})

	t.Run("Unicode whitespace remains Markdown content", func(t *testing.T) {
		pool, repo, ctx := issue83Repository(t)
		note := issue83SeedNote(t, pool, "Existing", "Published", "\u00a0", 1)

		published, err := repo.PublishNote(ctx, model.ReaderNotePublishCommand{
			NoteID: note.ID, ExpectedDraftRevision: 1, ExpectedPublishedRevision: 1,
		})
		if err != nil {
			t.Fatalf("PublishNote Unicode whitespace: %v", err)
		}
		if published.PublishedContent != "\u00a0" || published.PublishedRevision != 2 {
			t.Fatalf("published = %#v, want Unicode whitespace content at revision 2", published)
		}
	})

	t.Run("identical published snapshot", func(t *testing.T) {
		pool, repo, ctx := issue83Repository(t)
		note := issue83SeedNote(t, pool, "Existing", "Published", "", 0)
		before := issue83SnapshotState(t, pool, note.ID)

		published, err := repo.PublishNote(ctx, model.ReaderNotePublishCommand{
			NoteID: note.ID, ExpectedDraftRevision: 0, ExpectedPublishedRevision: 1,
		})
		if err != nil {
			t.Fatalf("PublishNote no-op: %v", err)
		}
		if published.PublishedRevision != 1 || published.PublishedContent != "Published" {
			t.Fatalf("no-op response = %#v", published)
		}
		issue83AssertSnapshotEqual(t, issue83SnapshotState(t, pool, note.ID), before)
	})
}

func TestIssue83PublishRequiresExactActiveThoughtSet(t *testing.T) {
	for _, test := range []struct {
		name    string
		ops     func(t *testing.T, noteID uuid.UUID, thoughtID string) []json.RawMessage
		wantErr error
	}{
		{name: "exact", ops: func(t *testing.T, noteID uuid.UUID, thoughtID string) []json.RawMessage {
			return []json.RawMessage{issue83Reanchor(t, thoughtID, noteID, 1, 2, "# New heading\n\nNew quote", "New quote")}
		}},
		{name: "missing", ops: func(*testing.T, uuid.UUID, string) []json.RawMessage { return nil }, wantErr: repository.ErrReaderNoteReanchorIncomplete},
		{name: "extra", ops: func(t *testing.T, noteID uuid.UUID, thoughtID string) []json.RawMessage {
			return []json.RawMessage{
				issue83Reanchor(t, thoughtID, noteID, 1, 2, "# New heading\n\nNew quote", "New quote"),
				issue83Historical(t, "extra-thought"),
			}
		}, wantErr: repository.ErrReaderNoteReanchorIncomplete},
		{name: "duplicate", ops: func(t *testing.T, noteID uuid.UUID, thoughtID string) []json.RawMessage {
			op := issue83Reanchor(t, thoughtID, noteID, 1, 2, "# New heading\n\nNew quote", "New quote")
			return []json.RawMessage{op, op}
		}, wantErr: repository.ErrReaderNoteReanchorIncomplete},
		{name: "empty set with active thought", ops: func(*testing.T, uuid.UUID, string) []json.RawMessage { return []json.RawMessage{} }, wantErr: repository.ErrReaderNoteReanchorIncomplete},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool, repo, ctx := issue83Repository(t)
			note, thoughtID := issue83ReadyPublish(t, pool, repo, ctx)
			before := issue83SnapshotState(t, pool, note.ID)

			published, err := repo.PublishNote(ctx, model.ReaderNotePublishCommand{
				NoteID: note.ID, ExpectedDraftRevision: 1, ExpectedPublishedRevision: 1,
				ReanchorOps: test.ops(t, note.ID, thoughtID),
			})
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("PublishNote() error = %v, want %v", err, test.wantErr)
				}
				issue83AssertSnapshotEqual(t, issue83SnapshotState(t, pool, note.ID), before)
				return
			}
			if err != nil {
				t.Fatalf("PublishNote exact set: %v", err)
			}
			if published.PublishedRevision != 2 || published.Title != "New heading" {
				t.Fatalf("published = %#v, want title/New revision", published)
			}
			thought, err := repo.GetThought(ctx, thoughtID)
			if err != nil {
				t.Fatalf("GetThought: %v", err)
			}
			if issue83TargetRevision(t, thought.Target) != 2 {
				t.Fatalf("thought target = %s, want revision 2", thought.Target)
			}
		})
	}
}

func TestIssue83PublishExcludesHistoricalThoughtsFromTheActiveSet(t *testing.T) {
	pool, repo, ctx := issue83Repository(t)
	note, thoughtID := issue83ReadyPublish(t, pool, repo, ctx)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason)
		VALUES ($1,'note',$2,'note_reanchor_missing-quote')`, thoughtID, note.ID.String()); err != nil {
		t.Fatalf("seed historical Thought: %v", err)
	}

	published, err := repo.PublishNote(ctx, model.ReaderNotePublishCommand{
		NoteID: note.ID, ExpectedDraftRevision: 1, ExpectedPublishedRevision: 1,
		ReanchorOps: []json.RawMessage{},
	})
	if err != nil {
		t.Fatalf("PublishNote with only a historical Thought: %v", err)
	}
	if published.PublishedRevision != 2 {
		t.Fatalf("published revision = %d, want 2", published.PublishedRevision)
	}
	var tombstones int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_tombstones WHERE thought_id=$1`, thoughtID).Scan(&tombstones); err != nil {
		t.Fatalf("count historical Thought tombstones: %v", err)
	}
	if tombstones != 1 {
		t.Fatalf("historical Thought tombstones = %d, want 1", tombstones)
	}
}

func TestIssue83ConcurrentThoughtAndDualCASAreSerialized(t *testing.T) {
	pool, repo, ctx := issue83Repository(t)
	note := issue83SeedNote(t, pool, "Old", "Old quote", "New quote", 1)

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin note-share blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	var locked uuid.UUID
	if err := blocker.QueryRow(ctx, `SELECT id FROM reader_notes WHERE id=$1 FOR SHARE`, note.ID).Scan(&locked); err != nil {
		t.Fatalf("hold Note FOR SHARE: %v", err)
	}

	publishDone := make(chan error, 1)
	go func() {
		_, publishErr := repo.PublishNote(ctx, model.ReaderNotePublishCommand{
			NoteID: note.ID, ExpectedDraftRevision: 1, ExpectedPublishedRevision: 1,
		})
		publishDone <- publishErr
	}()
	issue83WaitForNoteLock(t, pool, note.ID)

	appendDone := make(chan error, 1)
	go func() {
		_, appendErr := repo.AppendThoughtOps(ctx, []model.ReaderThoughtOp{issue83ThoughtAdd(note.ID, "concurrent-thought", "Old quote")})
		appendDone <- appendErr
	}()
	select {
	case err := <-appendDone:
		if err != nil {
			t.Fatalf("concurrent AppendThoughtOps: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent thought did not acquire compatible Note FOR SHARE")
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("release Note blocker: %v", err)
	}
	if err := <-publishDone; !errors.Is(err, repository.ErrReaderNoteReanchorIncomplete) {
		t.Fatalf("PublishNote after concurrent thought = %v, want incomplete set", err)
	}

	// Once a transition wins, a competing request with the same pair of CAS
	// values must fail and leave the winner's revision available for retry.
	note = issue83SeedNote(t, pool, "Race", "Old", "New", 1)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, publishErr := repo.PublishNote(ctx, model.ReaderNotePublishCommand{
				NoteID: note.ID, ExpectedDraftRevision: 1, ExpectedPublishedRevision: 1,
			})
			errs <- publishErr
		}()
	}
	wg.Wait()
	close(errs)
	var successes, conflicts int
	for err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, repository.ErrRevisionConflict) {
			conflicts++
		} else {
			t.Fatalf("concurrent PublishNote error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("dual CAS results successes=%d conflicts=%d, want 1/1", successes, conflicts)
	}

	current, err := repo.GetNote(ctx, note.ID)
	if err != nil {
		t.Fatalf("read publish winner for restore race: %v", err)
	}
	errs = make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, restoreErr := repo.RestoreNoteRevision(ctx, model.ReaderNoteRestoreCommand{
				NoteID:                    note.ID,
				Revision:                  1,
				ExpectedDraftRevision:     current.DraftRevision,
				ExpectedPublishedRevision: current.PublishedRevision,
				ReanchorOps:               []json.RawMessage{},
			})
			errs <- restoreErr
		}()
	}
	wg.Wait()
	close(errs)
	successes, conflicts = 0, 0
	for err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, repository.ErrRevisionConflict) {
			conflicts++
		} else {
			t.Fatalf("concurrent RestoreNoteRevision error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("restore dual CAS results successes=%d conflicts=%d, want 1/1", successes, conflicts)
	}
}

func TestIssue83RestorePreservesDirtyDraftAndConvergesCleanThoughts(t *testing.T) {
	pool, repo, ctx := issue83Repository(t)
	note, thoughtID := issue83ReadyPublish(t, pool, repo, ctx)
	published, err := repo.PublishNote(ctx, model.ReaderNotePublishCommand{
		NoteID: note.ID, ExpectedDraftRevision: 1, ExpectedPublishedRevision: 1,
		ReanchorOps: []json.RawMessage{issue83Reanchor(t, thoughtID, note.ID, 1, 2, "# New heading\n\nNew quote", "New quote")},
	})
	if err != nil {
		t.Fatalf("publish restore fixture: %v", err)
	}

	dirty, err := repo.SaveNoteDraft(ctx, model.ReaderNoteDraftCommand{NoteID: note.ID, Content: "Unsaved draft", ExpectedDraftRevision: published.DraftRevision})
	if err != nil {
		t.Fatalf("SaveNoteDraft dirty fixture: %v", err)
	}
	beforeDirty := issue83SnapshotState(t, pool, note.ID)
	_, err = repo.RestoreNoteRevision(ctx, model.ReaderNoteRestoreCommand{
		NoteID: note.ID, Revision: 1, ExpectedDraftRevision: dirty.DraftRevision, ExpectedPublishedRevision: published.PublishedRevision,
	})
	if !errors.Is(err, repository.ErrReaderNoteDraftDirty) {
		t.Fatalf("dirty RestoreNoteRevision error = %v, want ErrReaderNoteDraftDirty", err)
	}
	issue83AssertSnapshotEqual(t, issue83SnapshotState(t, pool, note.ID), beforeDirty)

	if err := repo.DiscardNoteDraft(ctx, model.ReaderNoteDiscardDraftCommand{NoteID: note.ID, ExpectedDraftRevision: dirty.DraftRevision}); err != nil {
		t.Fatalf("discard dirty fixture: %v", err)
	}
	clean, err := repo.GetNote(ctx, note.ID)
	if err != nil {
		t.Fatalf("GetNote clean fixture: %v", err)
	}
	var oldContent, oldTitle string
	if err := pool.QueryRow(ctx, `SELECT content,title FROM reader_note_history WHERE note_id=$1 AND revision=1`, note.ID).Scan(&oldContent, &oldTitle); err != nil {
		t.Fatalf("read immutable source history: %v", err)
	}
	restored, err := repo.RestoreNoteRevision(ctx, model.ReaderNoteRestoreCommand{
		NoteID: note.ID, Revision: 1, ExpectedDraftRevision: clean.DraftRevision, ExpectedPublishedRevision: clean.PublishedRevision,
		ReanchorOps: []json.RawMessage{issue83Reanchor(t, thoughtID, note.ID, 2, 3, "# Old heading\n\nOld quote", "Old quote")},
	})
	if err != nil {
		t.Fatalf("clean RestoreNoteRevision: %v", err)
	}
	if restored.PublishedRevision != 3 || restored.PublishedContent != oldContent || restored.Title != oldTitle || restored.DraftContent != nil {
		t.Fatalf("restored = %#v, want clean aligned revision 3", restored)
	}
	var sourceContent, sourceTitle string
	if err := pool.QueryRow(ctx, `SELECT content,title FROM reader_note_history WHERE note_id=$1 AND revision=1`, note.ID).Scan(&sourceContent, &sourceTitle); err != nil {
		t.Fatalf("re-read immutable history: %v", err)
	}
	if sourceContent != oldContent || sourceTitle != oldTitle {
		t.Fatalf("old history mutated: content=%q title=%q", sourceContent, sourceTitle)
	}
	thought, err := repo.GetThought(ctx, thoughtID)
	if err != nil {
		t.Fatalf("GetThought restored: %v", err)
	}
	if issue83TargetRevision(t, thought.Target) != 3 {
		t.Fatalf("restored thought target = %s, want revision 3", thought.Target)
	}
}

func TestIssue83PublishRollsBackDistinctWriteStages(t *testing.T) {
	for _, test := range []struct {
		stage      string
		historical bool
	}{
		{stage: "reader_notes"},
		{stage: "reader_note_history"},
		{stage: "reader_thought_ops"},
		{stage: "reader_thoughts"},
		{stage: "reader_thought_tombstones", historical: true},
	} {
		t.Run(test.stage, func(t *testing.T) {
			pool, repo, ctx := issue83Repository(t)
			note, thoughtID := issue83ReadyPublish(t, pool, repo, ctx)
			before := issue83SnapshotState(t, pool, note.ID)
			issue83InstallFailTrigger(t, pool, test.stage)
			ops := []json.RawMessage{issue83Reanchor(t, thoughtID, note.ID, 1, 2, "# New heading\n\nNew quote", "New quote")}
			if test.historical {
				ops = []json.RawMessage{issue83Historical(t, thoughtID)}
			}

			_, err := repo.PublishNote(ctx, model.ReaderNotePublishCommand{
				NoteID: note.ID, ExpectedDraftRevision: 1, ExpectedPublishedRevision: 1,
				ReanchorOps: ops,
			})
			if err == nil || !strings.Contains(err.Error(), "issue83 injected write failure") {
				t.Fatalf("PublishNote with %s failure = %v", test.stage, err)
			}
			issue83AssertRollbackState(t, issue83SnapshotState(t, pool, note.ID), before)
		})
	}
}

func TestIssue83RestoreRollsBackDistinctWriteStages(t *testing.T) {
	for _, test := range []struct {
		stage      string
		historical bool
	}{
		{stage: "reader_notes"},
		{stage: "reader_note_history"},
		{stage: "reader_thought_ops"},
		{stage: "reader_thoughts"},
		{stage: "reader_thought_tombstones", historical: true},
	} {
		t.Run(test.stage, func(t *testing.T) {
			pool, repo, ctx := issue83Repository(t)
			note, thoughtID := issue83ReadyPublish(t, pool, repo, ctx)
			published, err := repo.PublishNote(ctx, model.ReaderNotePublishCommand{
				NoteID: note.ID, ExpectedDraftRevision: 1, ExpectedPublishedRevision: 1,
				ReanchorOps: []json.RawMessage{issue83Reanchor(t, thoughtID, note.ID, 1, 2, "# New heading\n\nNew quote", "New quote")},
			})
			if err != nil {
				t.Fatalf("publish restore fixture: %v", err)
			}
			before := issue83SnapshotState(t, pool, note.ID)
			issue83InstallFailTrigger(t, pool, test.stage)
			ops := []json.RawMessage{issue83Reanchor(t, thoughtID, note.ID, 2, 3, "# Old heading\n\nOld quote", "Old quote")}
			if test.historical {
				ops = []json.RawMessage{issue83Historical(t, thoughtID)}
			}

			_, err = repo.RestoreNoteRevision(ctx, model.ReaderNoteRestoreCommand{
				NoteID:                    note.ID,
				Revision:                  1,
				ExpectedDraftRevision:     published.DraftRevision,
				ExpectedPublishedRevision: published.PublishedRevision,
				ReanchorOps:               ops,
			})
			if err == nil || !strings.Contains(err.Error(), "issue83 injected write failure") {
				t.Fatalf("RestoreNoteRevision with %s failure = %v", test.stage, err)
			}
			issue83AssertRollbackState(t, issue83SnapshotState(t, pool, note.ID), before)
		})
	}
}

func issue83Repository(t *testing.T) (*pgxpool.Pool, *repository.PGXReaderVNextRepository, context.Context) {
	t.Helper()
	pool := StartPostgres(t)
	return pool, repository.NewPGXReaderVNextRepository(pool), t.Context()
}

func issue83SeedNote(t *testing.T, pool *pgxpool.Pool, title, published, draft string, draftRevision int64) model.ReaderNote {
	t.Helper()
	var draftValue any
	if draft != "" {
		draftValue = draft
	}
	var note model.ReaderNote
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO reader_notes (title,published_content,published_revision,draft_content,draft_revision)
		VALUES ($1,$2,1,$3,$4)
		RETURNING id,title,published_content,published_revision,draft_content,draft_revision`,
		title, published, draftValue, draftRevision,
	).Scan(&note.ID, &note.Title, &note.PublishedContent, &note.PublishedRevision, &note.DraftContent, &note.DraftRevision); err != nil {
		t.Fatalf("seed note: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO reader_note_history (note_id,revision,title,content,reanchor_ops) VALUES ($1,1,$2,$3,'[]'::jsonb)`, note.ID, title, published); err != nil {
		t.Fatalf("seed immutable revision 1: %v", err)
	}
	return note
}

func issue83ReadyPublish(t *testing.T, pool *pgxpool.Pool, repo *repository.PGXReaderVNextRepository, ctx context.Context) (model.ReaderNote, string) {
	t.Helper()
	note := issue83SeedNote(t, pool, "Old heading", "# Old heading\n\nOld quote", "", 0)
	thoughtID := "thought-" + uuid.NewString()
	if _, err := repo.AppendThoughtOps(ctx, []model.ReaderThoughtOp{issue83ThoughtAdd(note.ID, thoughtID, "Old quote")}); err != nil {
		t.Fatalf("seed active Thought: %v", err)
	}
	updated, err := repo.SaveNoteDraft(ctx, model.ReaderNoteDraftCommand{
		NoteID: note.ID, Content: "# New heading\n\nNew quote", ExpectedDraftRevision: 0,
	})
	if err != nil {
		t.Fatalf("seed draft: %v", err)
	}
	return *updated, thoughtID
}

func issue83ThoughtAdd(noteID uuid.UUID, thoughtID, exact string) model.ReaderThoughtOp {
	target, _ := json.Marshal(map[string]any{"kind": "note", "host_id": noteID.String(), "version": map[string]any{"note_revision": 1}})
	payload, _ := json.Marshal(map[string]any{"body": "Thought", "quote": map[string]any{"exact": exact, "start": 0, "end": utf16Len(exact)}, "source": "user"})
	return model.ReaderThoughtOp{OpID: "issue83-add-" + thoughtID, DeviceID: "issue83", OperationKind: "add", AnnotationID: thoughtID, HostKind: "note", HostID: noteID.String(), Target: target, Payload: payload, LogicalClock: 1}
}

func issue83Reanchor(t *testing.T, thoughtID string, noteID uuid.UUID, previous, next int64, content, exact string) json.RawMessage {
	t.Helper()
	start := strings.Index(content, exact)
	if start < 0 {
		t.Fatalf("fixture quote %q absent from %q", exact, content)
	}
	start = utf16Len(content[:start])
	encoded, err := json.Marshal(map[string]any{
		"thought_id": thoughtID, "status": "reanchored", "reason": "unique-quote",
		"target": map[string]any{"kind": "note", "host_id": noteID.String(), "version": map[string]any{"note_revision": next}},
		"quote":  map[string]any{"exact": exact, "prefix": "", "suffix": ""},
		"range":  map[string]any{"start": start, "end": start + utf16Len(exact)},
	})
	if err != nil {
		t.Fatalf("marshal reanchor: %v", err)
	}
	_ = previous
	return encoded
}

func issue83Historical(t *testing.T, thoughtID string) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"thought_id": thoughtID, "status": "historical", "reason": "missing-quote"})
	if err != nil {
		t.Fatalf("marshal historical: %v", err)
	}
	return encoded
}

func issue83TargetRevision(t *testing.T, raw json.RawMessage) int64 {
	t.Helper()
	var target struct {
		Version struct {
			NoteRevision int64 `json:"note_revision"`
		} `json:"version"`
	}
	if err := json.Unmarshal(raw, &target); err != nil {
		t.Fatalf("decode thought target %s: %v", raw, err)
	}
	return target.Version.NoteRevision
}

type issue83Snapshot struct {
	Title, Published                          string
	PublishedRevision                         int64
	Draft                                     *string
	DraftRevision                             int64
	DraftUpdatedAt, UpdatedAt                 time.Time
	History, Operations, Thoughts, Tombstones int64
	Sequence                                  int64
}

func issue83SnapshotState(t *testing.T, pool *pgxpool.Pool, noteID uuid.UUID) issue83Snapshot {
	t.Helper()
	var state issue83Snapshot
	if err := pool.QueryRow(t.Context(), `SELECT title,published_content,published_revision,draft_content,draft_revision,COALESCE(draft_updated_at,'epoch'::timestamptz),updated_at FROM reader_notes WHERE id=$1`, noteID).Scan(&state.Title, &state.Published, &state.PublishedRevision, &state.Draft, &state.DraftRevision, &state.DraftUpdatedAt, &state.UpdatedAt); err != nil {
		t.Fatalf("snapshot note: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_note_history WHERE note_id=$1`, noteID).Scan(&state.History); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_ops`).Scan(&state.Operations); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thoughts`).Scan(&state.Thoughts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_tombstones`).Scan(&state.Tombstones); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT last_value FROM reader_thought_ops_sequence_seq`).Scan(&state.Sequence); err != nil {
		t.Fatalf("snapshot thought sequence: %v", err)
	}
	return state
}

func issue83AssertSnapshotEqual(t *testing.T, got, want issue83Snapshot) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("transaction changed state\n got: %#v\nwant: %#v", got, want)
	}
}

// PostgreSQL sequences are non-transactional: a failed INSERT can consume a
// value even when the row and every domain write are rolled back. The empty
// publish test above still asserts sequence equality because it reaches no
// INSERT at all; fault injection asserts the durable transaction state.
func issue83AssertRollbackState(t *testing.T, got, want issue83Snapshot) {
	t.Helper()
	got.Sequence = 0
	want.Sequence = 0
	issue83AssertSnapshotEqual(t, got, want)
}

func issue83InstallFailTrigger(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	name := "issue83_fail_" + table
	if _, err := pool.Exec(t.Context(), `CREATE OR REPLACE FUNCTION issue83_fail_writer() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'issue83 injected write failure'; END $$`); err != nil {
		t.Fatalf("create failure function: %v", err)
	}
	if _, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT OR UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION issue83_fail_writer()`, name, table)); err != nil {
		t.Fatalf("create failure trigger for %s: %v", table, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, name, table))
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS issue83_fail_writer()`)
	})
}

func issue83WaitForNoteLock(t *testing.T, pool *pgxpool.Pool, noteID uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiters int
		if err := pool.QueryRow(t.Context(), `
			SELECT count(*) FROM pg_locks l
			JOIN pg_stat_activity a ON a.pid=l.pid
			WHERE NOT l.granted AND a.query LIKE '%reader_notes%'`).Scan(&waiters); err != nil {
			t.Fatalf("observe Note lock wait: %v", err)
		}
		if waiters > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("publish for Note %s never blocked", noteID)
}

func utf16Len(value string) int { return len(utf16.Encode([]rune(value))) }
