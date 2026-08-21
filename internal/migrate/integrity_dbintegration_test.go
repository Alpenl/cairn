//go:build dbintegration

package migrate

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestUserDeletedThoughtIsContentFreeAndTerminalDB(t *testing.T) {
	pool := migrationTestPool(t, 2)
	requireMigratedIntegritySchema(t, pool)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	annotationID := "terminal-" + uuid.NewString()
	deleteOpID := "delete-" + uuid.NewString()
	lateOpID := "late-" + uuid.NewString()
	defer cleanupThoughtFixture(t, pool, annotationID)

	if _, err := pool.Exec(ctx, `INSERT INTO reader_thoughts
		(id,host_kind,host_id,target,body,source,deleted,last_sequence,winner_logical_clock,winner_device_id,winner_op_id)
		VALUES ($1,'note','privacy-test',$2::jsonb,'private-body','private-source',false,1,1,'device','add')`,
		annotationID, `{"secret":"active"}`); err != nil {
		t.Fatalf("insert active thought: %v", err)
	}
	var deleteSequence int64
	if err := pool.QueryRow(ctx, `INSERT INTO reader_thought_ops
		(op_id,device_id,operation_kind,annotation_id,host_kind,host_id,target,payload,logical_clock)
		VALUES ($1,'device','delete',$2,'note','privacy-test',$3::jsonb,$4::jsonb,2) RETURNING sequence`,
		deleteOpID, annotationID, `{"secret":"delete-target"}`, `{"secret":"delete-payload"}`).Scan(&deleteSequence); err != nil {
		t.Fatalf("insert delete op: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE reader_thoughts
		SET deleted=true,last_sequence=$2,winner_logical_clock=2,winner_op_id=$3 WHERE id=$1`,
		annotationID, deleteSequence, deleteOpID); err != nil {
		t.Fatalf("materialize delete: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason,snapshot)
		VALUES ($1,'note','privacy-test','user_deleted',$2::jsonb)`, annotationID, `{"secret":"snapshot"}`); err != nil {
		t.Fatalf("insert terminal marker: %v", err)
	}

	var lateSequence int64
	if err := pool.QueryRow(ctx, `INSERT INTO reader_thought_ops
		(op_id,device_id,operation_kind,annotation_id,host_kind,host_id,target,payload,logical_clock)
		VALUES ($1,'device','update',$2,'note','privacy-test',$3::jsonb,$4::jsonb,3) RETURNING sequence`,
		lateOpID, annotationID, `{"secret":"late-target"}`, `{"secret":"late-payload"}`).Scan(&lateSequence); err != nil {
		t.Fatalf("insert late op: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE reader_thoughts SET
		deleted=false,body='resurrected',source='resurrected',target=$2::jsonb,
		last_sequence=$3,winner_logical_clock=3,winner_op_id=$4 WHERE id=$1`,
		annotationID, `{"secret":"late-materialized"}`, lateSequence, lateOpID); err != nil {
		t.Fatalf("apply late materialized update: %v", err)
	}
	deleteTag, err := pool.Exec(ctx, `DELETE FROM reader_thought_tombstones WHERE thought_id=$1 AND reason='user_deleted'`, annotationID)
	if err != nil {
		t.Fatalf("attempt terminal marker delete: %v", err)
	}
	if deleteTag.RowsAffected() != 0 {
		t.Fatalf("terminal marker delete affected %d rows, want 0", deleteTag.RowsAffected())
	}
	if _, err := pool.Exec(ctx, `INSERT INTO reader_thought_supersession_events
		(annotation_id,loser_sequence,winner_sequence,loser,winner_at_detection)
		VALUES ($1,$2,$3,$4::jsonb,$5::jsonb)`, annotationID, deleteSequence, lateSequence,
		`{"secret":"loser"}`, `{"secret":"winner"}`); err != nil {
		t.Fatalf("insert late supersession event: %v", err)
	}

	var terminal, contentFree bool
	if err := pool.QueryRow(ctx, `SELECT user_deleted AND deleted,
		body='' AND source='' AND target='{}'::jsonb AND quote IS NULL
		FROM reader_thoughts WHERE id=$1`, annotationID).Scan(&terminal, &contentFree); err != nil {
		t.Fatalf("read terminal thought: %v", err)
	}
	if !terminal || !contentFree {
		t.Fatalf("terminal thought = (%t, content-free %t), want true/true", terminal, contentFree)
	}
	var leaks int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM reader_thought_ops WHERE annotation_id=$1 AND (target::text LIKE '%secret%' OR payload::text LIKE '%secret%')) +
		(SELECT count(*) FROM reader_thought_supersession_events WHERE annotation_id=$1 AND (loser::text LIKE '%secret%' OR winner_at_detection::text LIKE '%secret%')) +
		(SELECT count(*) FROM reader_thought_tombstones WHERE thought_id=$1 AND (reason<>'user_deleted' OR snapshot::text LIKE '%secret%'))`,
		annotationID).Scan(&leaks); err != nil {
		t.Fatalf("count privacy leaks: %v", err)
	}
	if leaks != 0 {
		t.Fatalf("user-deleted thought retained %d content-bearing history rows", leaks)
	}
}

func requireMigratedIntegritySchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var ready bool
	if err := pool.QueryRow(t.Context(), `SELECT
		to_regclass('public.reader_thoughts') IS NOT NULL
		AND EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='reader_thoughts' AND column_name='user_deleted')`).Scan(&ready); err != nil {
		t.Fatalf("inspect migrated integrity schema: %v", err)
	}
	if !ready {
		t.Skip("WEBTAG_TEST_DATABASE_URL does not contain the migrated Cairn schema")
	}
}

func cleanupThoughtFixture(t *testing.T, pool *pgxpool.Pool, annotationID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	statements := []string{
		`DELETE FROM reader_thoughts WHERE id=$1`,
		`DELETE FROM reader_thought_supersession_events WHERE annotation_id=$1`,
		`DELETE FROM reader_thought_ops WHERE annotation_id=$1`,
		`DELETE FROM reader_thought_tombstones WHERE thought_id=$1`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement, annotationID); err != nil {
			t.Errorf("cleanup thought fixture: %v", err)
		}
	}
}
