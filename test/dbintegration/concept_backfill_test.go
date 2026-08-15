// concept_backfill_test.go exercises the Phase 8 startup backfill against a
// real pgvector-enabled Postgres. The pure-Go unit tests in
// internal/concept/backfill_test.go cover the batch loop / fail-soft / cursor
// logic against an in-memory store; this file is the only place the actual
// scan query (`embedding IS NULL OR embedding_model IS DISTINCT FROM $current`,
// id-cursored) and the UPDATE … SET embedding/embedding_model run against
// pgvector. It validates the end-to-end contract the DEV-PLAN Phase 8 acceptance
// names: predefine 3 vector-less concepts + 1 stale-model concept → run the
// backfill → all 4 carry the current-model vector.
package dbintegration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/concept"
	"webtag/internal/repository"
)

// seedConceptNoVector inserts a concept row with NO embedding (the vector-less
// state legacy rows and gate fallbacks leave behind). Returns its id.
func seedConceptNoVector(t *testing.T, pool *pgxpool.Pool, primaryName string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO concept (primary_name) VALUES ($1) RETURNING id`,
		primaryName,
	).Scan(&id); err != nil {
		t.Fatalf("seed vector-less concept %q: %v", primaryName, err)
	}
	return id
}

// TestBackfillFillsMissingAndStaleVectors is the Phase 8 acceptance scenario:
// 3 concepts with no vector + 1 concept embedded under a stale model → the
// backfill re-embeds all 4 under the current model. A 5th concept already
// carrying the current-model vector must be left untouched (it never matches
// the scan predicate).
func TestBackfillFillsMissingAndStaleVectors(t *testing.T) {
	pool := StartPostgres(t)
	store := repository.NewPGXConceptRepository(pool)

	missing1 := seedConceptNoVector(t, pool, "alpha")
	missing2 := seedConceptNoVector(t, pool, "beta")
	missing3 := seedConceptNoVector(t, pool, "gamma")
	stale := seedConcept(t, pool, "delta", "stale-model")     // embedded under another model
	current := seedConcept(t, pool, "epsilon", gateTestModel) // already current → skip

	// A scripted embedder returns the unit basis vector for every input under
	// the current test model, so every backfilled row lands with a valid
	// current-model vector. Batch size 2 forces the id cursor across multiple
	// batches; the runner unit-tests cover the loop, here we only care that
	// every needy row ends up filled.
	b := concept.NewBackfiller(concept.BackfillOptions{
		Store:     store,
		Embedder:  scriptedEmbedder{vec: e0()},
		BatchSize: 2,
	})

	filled, failed, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("backfill Run: %v", err)
	}
	if filled != 4 || failed != 0 {
		t.Fatalf("backfill filled=%d failed=%d, want 4/0", filled, failed)
	}

	// All four needy rows now carry the current-model vector.
	for _, tc := range []struct {
		name string
		id   uuid.UUID
	}{
		{"alpha", missing1}, {"beta", missing2}, {"gamma", missing3}, {"delta", stale},
	} {
		model, hasVec := conceptVectorModel(t, pool, tc.id)
		if !hasVec || model != gateTestModel {
			t.Fatalf("%s (%v) after backfill: hasVec=%v model=%q, want true/%q",
				tc.name, tc.id, hasVec, model, gateTestModel)
		}
	}

	// The pre-existing current-model row is untouched (still current, still has
	// its vector). It was never a candidate, so the count of current-model
	// vectors is exactly 5 (4 backfilled + 1 pre-existing).
	if model, hasVec := conceptVectorModel(t, pool, current); !hasVec || model != gateTestModel {
		t.Fatalf("pre-existing current row changed: hasVec=%v model=%q", hasVec, model)
	}
	if n := countConceptsWithModel(t, pool, gateTestModel); n != 5 {
		t.Fatalf("current-model vectors = %d, want 5", n)
	}
}

// TestBackfillResumesViaCursorIdempotent verifies the backfill is idempotent
// across runs: a second Run after the first completes finds nothing to do.
func TestBackfillResumesViaCursorIdempotent(t *testing.T) {
	pool := StartPostgres(t)
	store := repository.NewPGXConceptRepository(pool)

	seedConceptNoVector(t, pool, "one")
	seedConceptNoVector(t, pool, "two")

	b := concept.NewBackfiller(concept.BackfillOptions{
		Store:     store,
		Embedder:  scriptedEmbedder{vec: e0()},
		BatchSize: 8,
	})

	if filled, failed, err := b.Run(context.Background()); err != nil || filled != 2 || failed != 0 {
		t.Fatalf("first run: filled=%d failed=%d err=%v, want 2/0/nil", filled, failed, err)
	}
	// Second run: everything already carries the current-model vector, so the
	// scan returns empty immediately — nothing filled, nothing failed.
	if filled, failed, err := b.Run(context.Background()); err != nil || filled != 0 || failed != 0 {
		t.Fatalf("second run: filled=%d failed=%d err=%v, want 0/0/nil (idempotent)", filled, failed, err)
	}
}

func countConceptsWithModel(t *testing.T, pool *pgxpool.Pool, model string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM concept WHERE embedding IS NOT NULL AND embedding_model = $1`,
		model,
	).Scan(&n); err != nil {
		t.Fatalf("count concepts with model %q: %v", model, err)
	}
	return n
}
