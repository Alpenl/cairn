package dbintegration

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/readertext"
	"webtag/internal/repository"
)

func readReaderTodoBackfillLedger(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*)::int FROM reader_todo_projection_backfills`).Scan(&count); err != nil {
		t.Fatalf("read backfill ledger: %v", err)
	}
	return count
}

// TestReaderTodoProjectionBackfillIsIdempotent runs the backfill against
// pre-existing sources whose projections were never written, then runs it
// again. The second call must rebuild nothing and report the first run.
func TestReaderTodoProjectionBackfillIsIdempotent(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)

	// Seeding through SQL is the point: these are the rows a pre-upgrade
	// installation already has, with no projection behind them.
	seedReaderVNextNote(t, pool, "Backfill host", "- [ ] backfill me\ncontext\n- [x] already done")
	if facts := readReaderTodoProjectionFacts(t, pool); len(facts) != 0 {
		t.Fatalf("seeded installation already had projections: %#v", facts)
	}

	first, err := reader.BackfillTodoProjections(ctx)
	if err != nil {
		t.Fatalf("first BackfillTodoProjections: %v", err)
	}
	if first.AlreadyComplete || first.ProjectedCount != 2 {
		t.Fatalf("first BackfillTodoProjections() = %#v, want a fresh run over two blocks", first)
	}
	facts := readReaderTodoProjectionFacts(t, pool)
	assertLiveTodoTexts(t, facts, "first backfill", "backfill me", "already done")
	assertProjectedTodoDone(t, facts, "first backfill", "already done", true)
	if got := readReaderTodoBackfillLedger(t, pool); got != 1 {
		t.Fatalf("ledger rows = %d, want 1", got)
	}

	second, err := reader.BackfillTodoProjections(ctx)
	if err != nil {
		t.Fatalf("second BackfillTodoProjections: %v", err)
	}
	if !second.AlreadyComplete || !second.CompletedAt.Equal(first.CompletedAt) || second.ProjectedCount != first.ProjectedCount {
		t.Fatalf("second BackfillTodoProjections() = %#v, want the first run reported back", second)
	}
	after := readReaderTodoProjectionFacts(t, pool)
	if len(after) != len(facts) {
		t.Fatalf("second backfill changed the projection: %#v", after)
	}
	if got := readReaderTodoBackfillLedger(t, pool); got != 1 {
		t.Fatalf("ledger rows after replay = %d, want 1", got)
	}

	third, err := reader.BackfillTodoProjections(ctx)
	if err != nil {
		t.Fatalf("third BackfillTodoProjections: %v", err)
	}
	if !third.AlreadyComplete {
		t.Fatalf("third BackfillTodoProjections() = %#v, want the ledger to hold", third)
	}
}

// TestReaderTodoProjectionBackfillLeavesNoMarkerWhenItFails makes the ledger
// insert fail after the rebuild has already written rows. The projections and
// the completion marker must roll back together, so a later run still sees an
// un-backfilled installation and can finish the job.
func TestReaderTodoProjectionBackfillLeavesNoMarkerWhenItFails(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)

	seedReaderVNextNote(t, pool, "Failing backfill host", "- [ ] rebuild me")

	// A trigger that raises on the ledger insert is the cheapest way to fail
	// the transaction strictly after the projections were written.
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION reader_todo_backfill_test_block() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'backfill ledger unavailable';
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER reader_todo_backfill_test_block
		BEFORE INSERT ON reader_todo_projection_backfills
		FOR EACH ROW EXECUTE FUNCTION reader_todo_backfill_test_block()`); err != nil {
		t.Fatalf("install failing trigger: %v", err)
	}
	if _, err := reader.BackfillTodoProjections(ctx); err == nil {
		t.Fatal("BackfillTodoProjections() succeeded with a failing ledger insert")
	}
	if got := readReaderTodoBackfillLedger(t, pool); got != 0 {
		t.Fatalf("failed backfill recorded %d ledger rows, want 0", got)
	}
	if facts := readReaderTodoProjectionFacts(t, pool); len(facts) != 0 {
		t.Fatalf("failed backfill left projections behind: %#v", facts)
	}

	if _, err := pool.Exec(ctx, `
		DROP TRIGGER reader_todo_backfill_test_block ON reader_todo_projection_backfills;
		DROP FUNCTION reader_todo_backfill_test_block()`); err != nil {
		t.Fatalf("remove failing trigger: %v", err)
	}
	retried, err := reader.BackfillTodoProjections(ctx)
	if err != nil {
		t.Fatalf("retried BackfillTodoProjections: %v", err)
	}
	if retried.AlreadyComplete || retried.ProjectedCount != 1 {
		t.Fatalf("retried BackfillTodoProjections() = %#v, want a fresh run", retried)
	}
	assertLiveTodoTexts(t, readReaderTodoProjectionFacts(t, pool), "retried backfill", "rebuild me")
}

// TestReaderTodoProjectionBackfillHonoursTombstones proves the backfill uses
// the same tombstone rule as every other path: a dismissed projection is not
// resurrected just because the source still emits its block.
func TestReaderTodoProjectionBackfillHonoursTombstones(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)

	content := "- [ ] dismissed already"
	note := seedReaderVNextNote(t, pool, "Tombstoned backfill host", content)
	blocks := readertext.List(content)
	if len(blocks) != 1 {
		t.Fatalf("readertext.List(%q) = %#v, want one block", content, blocks)
	}
	hostKind, hostID := "note", note.ID.String()
	if _, err := pool.Exec(ctx, `
		INSERT INTO reader_todos (text,done,origin_kind,origin_host_kind,origin_host_id,origin_ref,host_revision,deleted_at)
		VALUES ($1,false,'note',$2,$3,$4::jsonb,1,NOW())`,
		blocks[0].Text, hostKind, hostID,
		readerVNextJSON(t, map[string]any{
			"block_ref":   blocks[0].BlockRef,
			"text":        blocks[0].Text,
			"occurrence":  blocks[0].Occurrence,
			"source_kind": "note",
			"source_id":   hostID,
		})); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}

	if _, err := reader.BackfillTodoProjections(ctx); err != nil {
		t.Fatalf("BackfillTodoProjections: %v", err)
	}
	facts := readReaderTodoProjectionFacts(t, pool)
	assertLiveTodoTexts(t, facts, "backfill with tombstone")
	if len(facts) != 1 {
		t.Fatalf("backfill with tombstone produced %#v, want only the tombstone", facts)
	}
}
