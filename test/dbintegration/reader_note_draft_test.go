package dbintegration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
)

type readerNoteDraftState struct {
	content   *string
	revision  int64
	draftAt   *time.Time
	updatedAt time.Time
}

func TestReaderNoteDraftPostgresErrorPriorityAndAtomicity(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()

	t.Run("missing notes are not found without writes", func(t *testing.T) {
		missingID := uuid.New()
		if _, err := repo.SaveNoteDraft(ctx, model.ReaderNoteDraftCommand{NoteID: missingID, Content: "must not create", ExpectedDraftRevision: 0}); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("missing SaveNoteDraft error = %v, want ErrNotFound", err)
		}
		var missingRows int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_notes WHERE id=$1`, missingID).Scan(&missingRows); err != nil || missingRows != 0 {
			t.Fatalf("missing note rows = %d, %v; want 0", missingRows, err)
		}
	})

	t.Run("matching revision advances exactly once", func(t *testing.T) {
		note := seedReaderVNextNote(t, pool, "matching", "published")
		updated, err := repo.SaveNoteDraft(ctx, model.ReaderNoteDraftCommand{NoteID: note.ID, Content: "draft one", ExpectedDraftRevision: note.DraftRevision})
		if err != nil {
			t.Fatalf("matching SaveNoteDraft: %v", err)
		}
		if updated.DraftRevision != note.DraftRevision+1 || updated.DraftContent == nil || *updated.DraftContent != "draft one" {
			t.Fatalf("matching result = %#v, want one revision increment and draft", updated)
		}
		state := readerNoteDraftSnapshot(t, pool, note.ID)
		if state.revision != note.DraftRevision+1 || state.content == nil || *state.content != "draft one" {
			t.Fatalf("matching stored state = %#v", state)
		}
	})

	t.Run("stale revision is conflict without writes", func(t *testing.T) {
		note := seedReaderVNextNote(t, pool, "stale", "published")
		if _, err := repo.SaveNoteDraft(ctx, model.ReaderNoteDraftCommand{NoteID: note.ID, Content: "first draft", ExpectedDraftRevision: 0}); err != nil {
			t.Fatalf("prepare stale note: %v", err)
		}
		before := readerNoteDraftSnapshot(t, pool, note.ID)
		if _, err := repo.SaveNoteDraft(ctx, model.ReaderNoteDraftCommand{NoteID: note.ID, Content: "stale overwrite", ExpectedDraftRevision: 0}); !errors.Is(err, repository.ErrRevisionConflict) {
			t.Fatalf("stale SaveNoteDraft error = %v, want ErrRevisionConflict", err)
		}
		after := readerNoteDraftSnapshot(t, pool, note.ID)
		assertReaderNoteDraftState(t, after, before)
	})

	t.Run("concurrent delete keeps the statement-snapshot classification", func(t *testing.T) {
		note := seedReaderVNextNote(t, pool, "concurrent", "published")
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatalf("begin delete transaction: %v", err)
		}
		defer tx.Rollback(t.Context())
		if _, err := tx.Exec(t.Context(), `DELETE FROM reader_notes WHERE id=$1`, note.ID); err != nil {
			t.Fatalf("delete concurrent note: %v", err)
		}

		result := make(chan error, 1)
		go func() {
			_, saveErr := repo.SaveNoteDraft(context.Background(), model.ReaderNoteDraftCommand{NoteID: note.ID, Content: "racing draft", ExpectedDraftRevision: 0})
			result <- saveErr
		}()
		waitForReaderDraftLock(t, pool)
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatalf("commit concurrent delete: %v", err)
		}
		if err := <-result; !errors.Is(err, repository.ErrRevisionConflict) {
			t.Fatalf("save racing committed delete = %v, want ErrRevisionConflict from its statement snapshot", err)
		}
	})

}

func readerNoteDraftSnapshot(t *testing.T, pool *pgxpool.Pool, noteID uuid.UUID) readerNoteDraftState {
	t.Helper()
	var state readerNoteDraftState
	if err := pool.QueryRow(t.Context(), `SELECT draft_content, draft_revision, draft_updated_at, updated_at FROM reader_notes WHERE id=$1`, noteID).
		Scan(&state.content, &state.revision, &state.draftAt, &state.updatedAt); err != nil {
		t.Fatalf("read note draft state: %v", err)
	}
	return state
}

func assertReaderNoteDraftState(t *testing.T, got, want readerNoteDraftState) {
	t.Helper()
	if got.revision != want.revision || got.updatedAt != want.updatedAt || !sameOptionalString(got.content, want.content) || !sameOptionalTime(got.draftAt, want.draftAt) {
		t.Fatalf("draft state = %#v, want %#v", got, want)
	}
}

func sameOptionalString(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func sameOptionalTime(left, right *time.Time) bool {
	return left == nil && right == nil || left != nil && right != nil && left.Equal(*right)
}

func waitForReaderDraftLock(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE query LIKE '%WITH updated AS%' AND wait_event_type = 'Lock')`).Scan(&waiting)
		if err != nil {
			t.Fatalf("observe draft lock: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("SaveNoteDraft did not wait on the concurrent delete lock")
}
