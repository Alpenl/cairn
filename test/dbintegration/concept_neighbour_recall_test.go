// concept_neighbour_recall_test.go exercises the co-occurrence leg of
// candidate recall (ListConceptsOfNearestLinks) against a real pgvector
// Postgres.
//
// The pgxmock test in internal/repository asserts the bound arguments, but
// pgxmock never parses or executes SQL — it string-matches the query prefix.
// A CTE joining three tables with a GROUP BY, a vector operator and seven
// parameters therefore has no unit-level coverage of whether it is valid SQL,
// let alone whether it returns the right rows. This file is that coverage.
package dbintegration

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"webtag/internal/repository"
)

const (
	neighbourModel = "test-embed-model"
	neighbourDims  = 1536
)

// neighbourVec returns a 1536-d unit vector at cosine c to e0=[1,0,…], so
// each seeded link sits at an exact known distance from the probe.
func neighbourVec(c float64) []float32 {
	v := make([]float32, neighbourDims)
	v[0] = float32(c)
	v[1] = float32(math.Sqrt(1 - c*c))
	return v
}

func seedNeighbourConcept(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO concept (primary_name, display_name)
		 VALUES ($1, $1) RETURNING id`,
		name,
	).Scan(&id); err != nil {
		t.Fatalf("seed concept %q: %v", name, err)
	}
	return id
}

// seedNeighbourLink inserts a link at the given cosine to the probe and
// attaches the supplied concepts. status / embeddingModel are parameters so
// the filter branches can be exercised.
func seedNeighbourLink(
	t *testing.T,
	pool *pgxpool.Pool,
	sourceKey string,
	cos float64,
	status, embeddingModel string,
	conceptIDs ...uuid.UUID,
) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var linkID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO links (url, source_kind, source_key, status, embedding, embedding_model, first_collected_at)
		 VALUES ($1, 'url', $2, $3, $4, $5, NOW()) RETURNING id`,
		"https://example.com/"+sourceKey, sourceKey, status,
		pgvector.NewVector(neighbourVec(cos)), embeddingModel,
	).Scan(&linkID); err != nil {
		t.Fatalf("seed link %q: %v", sourceKey, err)
	}
	for _, conceptID := range conceptIDs {
		if _, err := pool.Exec(ctx,
			`INSERT INTO link_concept (link_id, concept_id, surface_tag)
			 VALUES ($1, $2, 'x')`,
			linkID, conceptID,
		); err != nil {
			t.Fatalf("attach concept to %q: %v", sourceKey, err)
		}
	}
	return linkID
}

// TestNeighbourRecallRanksByCooccurrenceAndAppliesFilters runs the real SQL
// and checks every clause that has a way to be silently wrong:
//
//   - ORDER BY freq DESC — the whole premise is that a concept shared by more
//     neighbours is a stronger suggestion
//   - the excludeLinkID guard — on re-parse the link is its own nearest
//     neighbour and would vote for the tag set being recomputed
//   - status='done' and embedding_model filters
//   - LIMIT on the nearest-links CTE, not on the concept rows
func TestNeighbourRecallRanksByCooccurrenceAndAppliesFilters(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	repo := repository.NewPGXConceptRepository(pool)

	weekly := seedNeighbourConcept(t, pool, "科技周刊")
	rag := seedNeighbourConcept(t, pool, "RAG")
	selfOnly := seedNeighbourConcept(t, pool, "只有自己")
	pendingOnly := seedNeighbourConcept(t, pool, "未完成")
	wrongModelOnly := seedNeighbourConcept(t, pool, "模型不匹配")

	// Three close neighbours all carry 科技周刊; only one also carries RAG.
	seedNeighbourLink(t, pool, "n1", 0.95, "done", neighbourModel, weekly, rag)
	seedNeighbourLink(t, pool, "n2", 0.94, "done", neighbourModel, weekly)
	seedNeighbourLink(t, pool, "n3", 0.93, "done", neighbourModel, weekly)

	// Excluded for three different reasons, each with a concept no other
	// link carries — so if a filter breaks, the offending name shows up.
	self := seedNeighbourLink(t, pool, "self", 1.0, "done", neighbourModel, selfOnly)
	seedNeighbourLink(t, pool, "pending", 0.99, "pending", neighbourModel, pendingOnly)
	seedNeighbourLink(t, pool, "wrongmodel", 0.98, "done", "other-model", wrongModelOnly)

	got, err := repo.ListConceptsOfNearestLinks(ctx, neighbourVec(1.0), neighbourModel, 10, 15, self)
	if err != nil {
		t.Fatalf("ListConceptsOfNearestLinks: %v", err)
	}

	names := make([]string, len(got))
	for i, c := range got {
		names[i] = c.Name
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %v, want exactly [科技周刊 RAG]", names)
	}
	if names[0] != "科技周刊" {
		t.Errorf("top candidate = %q, want 科技周刊 (3 neighbours vs RAG's 1) — freq ordering is broken", names[0])
	}
	if names[1] != "RAG" {
		t.Errorf("second candidate = %q, want RAG", names[1])
	}
	if got[0].ConceptID != weekly || got[1].ConceptID != rag {
		t.Errorf("returned concept ids %v/%v, want %v/%v", got[0].ConceptID, got[1].ConceptID, weekly, rag)
	}
}

// TestNeighbourRecallLimitsNeighbourLinks pins that linkTopK bounds the
// voting links. Passing 1 must leave only the single nearest link's concepts.
func TestNeighbourRecallLimitsNeighbourLinks(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	repo := repository.NewPGXConceptRepository(pool)

	near := seedNeighbourConcept(t, pool, "最近的")
	far := seedNeighbourConcept(t, pool, "较远的")
	seedNeighbourLink(t, pool, "near", 0.99, "done", neighbourModel, near)
	seedNeighbourLink(t, pool, "far", 0.50, "done", neighbourModel, far)

	got, err := repo.ListConceptsOfNearestLinks(ctx, neighbourVec(1.0), neighbourModel, 1, 15, uuid.Nil)
	if err != nil {
		t.Fatalf("ListConceptsOfNearestLinks: %v", err)
	}
	if len(got) != 1 || got[0].ConceptID != near {
		t.Fatalf("candidates = %#v, want only the nearest link's concept (%v)", got, near)
	}
}

// TestNeighbourRecallAppliesSimilarityFloor: without a floor, ORDER BY +
// LIMIT always returns linkTopK links no matter how far away they are. On a
// thin corpus that means "the ten nearest" is really "ten arbitrary links",
// and their concepts get injected as candidates the model is told to prefer
// verbatim — worse than injecting nothing.
func TestNeighbourRecallAppliesSimilarityFloor(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	repo := repository.NewPGXConceptRepository(pool)

	close := seedNeighbourConcept(t, pool, "够近的")
	distant := seedNeighbourConcept(t, pool, "太远的")

	// cosine 0.90 → distance 0.10, inside the 0.25 floor.
	seedNeighbourLink(t, pool, "close", 0.90, "done", neighbourModel, close)
	// cosine 0.20 → distance 0.80, well outside. It is the ONLY other link,
	// so an unfloored query would happily return it.
	seedNeighbourLink(t, pool, "distant", 0.20, "done", neighbourModel, distant)

	got, err := repo.ListConceptsOfNearestLinks(ctx, neighbourVec(1.0), neighbourModel, 10, 15, uuid.Nil)
	if err != nil {
		t.Fatalf("ListConceptsOfNearestLinks: %v", err)
	}
	if len(got) != 1 || got[0].ConceptID != close {
		t.Fatalf("candidates = %#v, want only the near link's concept %v — the similarity floor is not applied", got, close)
	}
}
