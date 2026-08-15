package dbintegration

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	rf6bTranslationTerminalHistoryIndexMigrationID = "b671c9d2e411"
	rf6bTranslationTerminalHistoryIndexName        = "idx_river_job_translation_terminal_history"
)

const rf6bTranslationTerminalHistoryPlanSQL = `
EXPLAIN (FORMAT JSON, COSTS OFF)
SELECT id, kind, state::text, finalized_at, args
FROM public.river_job
WHERE kind IN ('translate_link_v2', 'translate_link_content')
  AND state IN ('cancelled', 'completed', 'discarded')
  AND finalized_at IS NOT NULL
  AND (finalized_at, id) > ($1::timestamptz, $2::bigint)
ORDER BY finalized_at ASC, id ASC
LIMIT $3`

const rf6bTranslationTerminalHistoryQuerySQL = `
SELECT id
FROM public.river_job
WHERE kind IN ('translate_link_v2', 'translate_link_content')
  AND state IN ('cancelled', 'completed', 'discarded')
  AND finalized_at IS NOT NULL
  AND (finalized_at, id) > ($1::timestamptz, $2::bigint)
ORDER BY finalized_at ASC, id ASC
LIMIT $3`

func TestTranslationTerminalHistoryIndexSupportsGenericKeysetPlan(t *testing.T) {
	pool := StartPostgres(t)
	assertMigrationRecorded(t, pool, rf6bTranslationTerminalHistoryIndexMigrationID, true)

	var valid, ready bool
	if err := pool.QueryRow(t.Context(), `SELECT indexes.indisvalid, indexes.indisready
		FROM pg_catalog.pg_index AS indexes
		WHERE indexes.indexrelid = 'public.idx_river_job_translation_terminal_history'::regclass`).
		Scan(&valid, &ready); err != nil {
		t.Fatalf("read RF6B terminal-history index state: %v", err)
	}
	if !valid || !ready {
		t.Fatalf("RF6B terminal-history index valid/ready = %v/%v, want true/true", valid, ready)
	}

	finalizedAt := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	const decoyCount = 20_000
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.river_job (
		args, kind, max_attempts, state, finalized_at
	)
	SELECT '{}'::jsonb, 'parse_link', 3, 'completed'::public.river_job_state, $1
	FROM generate_series(1, $2)`, finalizedAt, decoyCount); err != nil {
		t.Fatalf("seed RF6B non-translation terminal decoys: %v", err)
	}

	const matchingCount = 12
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.river_job (
		args, kind, max_attempts, state, finalized_at
	)
	SELECT jsonb_build_object('fixture', fixture),
	       CASE WHEN fixture % 2 = 0 THEN 'translate_link_v2' ELSE 'translate_link_content' END,
	       3,
	       CASE fixture % 3
	         WHEN 0 THEN 'cancelled'::public.river_job_state
	         WHEN 1 THEN 'completed'::public.river_job_state
	         ELSE 'discarded'::public.river_job_state
	       END,
	       $1
	FROM generate_series(1, $2) AS fixture`, finalizedAt, matchingCount); err != nil {
		t.Fatalf("seed RF6B translation terminal history: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `ANALYZE public.river_job`); err != nil {
		t.Fatalf("analyze RF6B terminal-history fixture: %v", err)
	}

	matchingIDs := rf6bTranslationTerminalIDs(t, pool, finalizedAt)
	if len(matchingIDs) != matchingCount {
		t.Fatalf("matching terminal jobs = %d, want %d (with %d decoys)",
			len(matchingIDs), matchingCount, decoyCount)
	}
	cursorID := matchingIDs[4]
	wantPage := matchingIDs[5:9]
	page := rf6bTranslationTerminalPage(t, pool, finalizedAt, cursorID, len(wantPage))
	if !slices.Equal(page, wantPage) {
		t.Fatalf("same-timestamp keyset page = %v, want %v", page, wantPage)
	}

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin RF6B terminal-history planner probe: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), `SET LOCAL plan_cache_mode = force_generic_plan`); err != nil {
		t.Fatalf("force generic RF6B planner mode: %v", err)
	}

	withIndex := rf6bExplainPlan(t, tx, finalizedAt, cursorID, len(wantPage))
	if !withIndex.usesIndex(rf6bTranslationTerminalHistoryIndexName) {
		t.Fatalf("generic terminal-history plan did not use %s: %+v",
			rf6bTranslationTerminalHistoryIndexName, withIndex)
	}
	if withIndex.containsNodeType("Sort") {
		t.Fatalf("generic terminal-history plan sorted despite ordered index: %+v", withIndex)
	}

	if _, err := tx.Exec(t.Context(), `DROP INDEX public.idx_river_job_translation_terminal_history`); err != nil {
		t.Fatalf("temporarily drop RF6B terminal-history index: %v", err)
	}
	withoutIndex := rf6bExplainPlan(t, tx, finalizedAt, cursorID, len(wantPage))
	if withoutIndex.usesIndex(rf6bTranslationTerminalHistoryIndexName) {
		t.Fatalf("planner mutation still reports dropped index %s: %+v",
			rf6bTranslationTerminalHistoryIndexName, withoutIndex)
	}
	// Without the index the plan has to sort. Asserting that here is what makes
	// the `!containsNodeType("Sort")` assertion above evidence rather than a
	// clause that would hold no matter what the planner did.
	if !withoutIndex.containsNodeType("Sort") {
		t.Fatalf("plan without the index did not sort, so the ordered-index assertion proves nothing: %+v",
			withoutIndex)
	}
}

// The planner probe runs against both the pool and an open transaction — the
// index-drop mutation has to stay inside a rolled-back transaction — so the
// helpers take the narrow read surface both types satisfy.
type rf6bPlanQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type rf6bExplainEnvelope struct {
	Plan rf6bExplainNode `json:"Plan"`
}

type rf6bExplainNode struct {
	NodeType  string            `json:"Node Type"`
	IndexName string            `json:"Index Name"`
	Plans     []rf6bExplainNode `json:"Plans"`
}

func (n rf6bExplainNode) usesIndex(name string) bool {
	if n.IndexName == name {
		return true
	}
	for _, child := range n.Plans {
		if child.usesIndex(name) {
			return true
		}
	}
	return false
}

func (n rf6bExplainNode) containsNodeType(nodeType string) bool {
	if n.NodeType == nodeType {
		return true
	}
	for _, child := range n.Plans {
		if child.containsNodeType(nodeType) {
			return true
		}
	}
	return false
}

func rf6bExplainPlan(
	t *testing.T,
	db rf6bPlanQuerier,
	finalizedAt time.Time,
	cursorID int64,
	limit int,
) rf6bExplainNode {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(
		t.Context(),
		rf6bTranslationTerminalHistoryPlanSQL,
		finalizedAt,
		cursorID,
		limit,
	).Scan(&raw); err != nil {
		t.Fatalf("explain RF6B terminal-history query: %v", err)
	}
	var envelopes []rf6bExplainEnvelope
	if err := json.Unmarshal(raw, &envelopes); err != nil {
		t.Fatalf("decode RF6B terminal-history plan %q: %v", raw, err)
	}
	if len(envelopes) != 1 {
		t.Fatalf("RF6B terminal-history plan envelopes = %d, want 1: %s", len(envelopes), raw)
	}
	return envelopes[0].Plan
}

func rf6bTranslationTerminalIDs(t *testing.T, db rf6bPlanQuerier, finalizedAt time.Time) []int64 {
	t.Helper()
	rows, err := db.Query(t.Context(), `SELECT id
		FROM public.river_job
		WHERE kind IN ('translate_link_v2', 'translate_link_content')
		  AND state IN ('cancelled', 'completed', 'discarded')
		  AND finalized_at = $1
		ORDER BY id`, finalizedAt)
	if err != nil {
		t.Fatalf("list RF6B translation terminal IDs: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan RF6B translation terminal ID: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate RF6B translation terminal IDs: %v", err)
	}
	return ids
}

func rf6bTranslationTerminalPage(
	t *testing.T,
	db rf6bPlanQuerier,
	finalizedAt time.Time,
	cursorID int64,
	limit int,
) []int64 {
	t.Helper()
	rows, err := db.Query(
		t.Context(),
		rf6bTranslationTerminalHistoryQuerySQL,
		finalizedAt,
		cursorID,
		limit,
	)
	if err != nil {
		t.Fatalf("query RF6B translation terminal page: %v", err)
	}
	defer rows.Close()
	page := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan RF6B translation terminal page: %v", err)
		}
		page = append(page, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate RF6B translation terminal page: %v", err)
	}
	return page
}
