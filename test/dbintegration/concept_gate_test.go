// concept_gate_test.go exercises the v3 vector gate (Phase 6) against a
// real pgvector-enabled Postgres. The pure-Go unit tests in
// internal/concept cover the branch logic with a stubbed nearest-neighbour
// store; this file is the only place the *actual* cosine-distance query
// (`ORDER BY embedding <=> $1`, model-scoped) runs against pgvector and the
// `1 - distance → similarity` conversion is validated end to end. It catches
// what the mock cannot: that the SQL operator returns the distance we think
// it does, that the model filter works, and that the three thresholds
// partition real cosine values the way the resolver assumes.
//
// The seeded concept and every embedder output are unit vectors so cosine
// similarity equals the plain dot product; we place each probe at an exact
// known cosine to the seed by constructing [cos, sin, 0, …]. With a single
// seeded neighbour the top-1 is unambiguous regardless of HNSW recall.
package dbintegration

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"webtag/internal/concept"
	"webtag/internal/repository"
)

const (
	gateTestModel = "test-embed-model"
	gateTestDims  = 1536
)

// unitVecAtCosine returns a 1536-d unit vector whose cosine similarity to
// the basis vector e0 = [1,0,0,…] is exactly c: [c, sqrt(1-c^2), 0, …].
func unitVecAtCosine(c float64) []float32 {
	v := make([]float32, gateTestDims)
	v[0] = float32(c)
	v[1] = float32(math.Sqrt(1 - c*c))
	return v
}

// e0 is the seed concept's vector — the basis vector all probes are
// measured against.
func e0() []float32 {
	v := make([]float32, gateTestDims)
	v[0] = 1
	return v
}

// scriptedEmbedder returns a fixed vector for every Embed call so a test
// can place the probe surface at an exact cosine to the seeded concept.
type scriptedEmbedder struct {
	vec []float32
}

func (e scriptedEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = e.vec
	}
	return out, nil
}
func (e scriptedEmbedder) Model() string { return gateTestModel }
func (e scriptedEmbedder) Enabled() bool { return true }

// TestVectorGateAutoMerge: probe at cosine 0.97 (>= 0.92) must reuse the
// seeded concept and write an embedding-merge alias, creating NO new
// concept.
func TestVectorGateAutoMerge(t *testing.T) {
	pool := StartPostgres(t)
	store := repository.NewPGXConceptRepository(pool)
	proposals := repository.NewPGXConceptProposalRepository(pool)
	seedID := seedConcept(t, pool, "RAG", gateTestModel)

	r := concept.NewResolver(concept.ResolverOptions{
		Store:     store,
		Embedder:  scriptedEmbedder{vec: unitVecAtCosine(0.97)},
		Proposals: proposals,
		Gate:      concept.GateConfig{AutoMergeThreshold: 0.92, NewThreshold: 0.80},
	})

	id, err := r.Resolve(context.Background(), "Retrieval Augmented Generation")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id != seedID {
		t.Fatalf("auto-merge: id = %v, want seed %v", id, seedID)
	}
	if n := countConcepts(t, pool); n != 1 {
		t.Fatalf("auto-merge created a concept: count = %d, want 1 (seed only)", n)
	}
	aliasID, ok := aliasConceptID(t, pool, "retrieval augmented generation")
	if !ok || aliasID != seedID {
		t.Fatalf("embedding-merge alias missing or wrong: id=%v ok=%v want %v", aliasID, ok, seedID)
	}
	if src := aliasSource(t, pool, "retrieval augmented generation"); src != "embedding-merge" {
		t.Fatalf("alias source = %q, want embedding-merge", src)
	}
	if n := countPendingProposals(t, pool); n != 0 {
		t.Fatalf("auto-merge wrote %d proposals, want 0", n)
	}
}

// TestVectorGateNewConcept: probe at cosine 0.50 (<= 0.80) must create a
// brand-new concept carrying its vector + model, and NO proposal.
func TestVectorGateNewConcept(t *testing.T) {
	pool := StartPostgres(t)
	store := repository.NewPGXConceptRepository(pool)
	proposals := repository.NewPGXConceptProposalRepository(pool)
	seedID := seedConcept(t, pool, "RAG", gateTestModel)

	r := concept.NewResolver(concept.ResolverOptions{
		Store:     store,
		Embedder:  scriptedEmbedder{vec: unitVecAtCosine(0.50)},
		Proposals: proposals,
		Gate:      concept.GateConfig{AutoMergeThreshold: 0.92, NewThreshold: 0.80},
	})

	id, err := r.Resolve(context.Background(), "WeKnora")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id == seedID || id == uuid.Nil {
		t.Fatalf("new-concept: id = %v, want a fresh concept (not seed %v)", id, seedID)
	}
	if n := countConcepts(t, pool); n != 2 {
		t.Fatalf("new-concept: count = %d, want 2 (seed + new)", n)
	}
	if model, hasVec := conceptVectorModel(t, pool, id); !hasVec || model != gateTestModel {
		t.Fatalf("new concept should carry vector + model %q; hasVec=%v model=%q", gateTestModel, hasVec, model)
	}
	if n := countPendingProposals(t, pool); n != 0 {
		t.Fatalf("new-concept wrote %d proposals, want 0", n)
	}
}

// TestVectorGateAmbiguousBand: probe at cosine 0.86 (0.80 < c < 0.92) must
// create a new concept AND enqueue a merge proposal (winner=seed,
// loser=new, reason="embedding-ambiguous: cos=…").
func TestVectorGateAmbiguousBand(t *testing.T) {
	pool := StartPostgres(t)
	store := repository.NewPGXConceptRepository(pool)
	proposals := repository.NewPGXConceptProposalRepository(pool)
	seedID := seedConcept(t, pool, "RAG", gateTestModel)

	r := concept.NewResolver(concept.ResolverOptions{
		Store:     store,
		Embedder:  scriptedEmbedder{vec: unitVecAtCosine(0.86)},
		Proposals: proposals,
		Gate:      concept.GateConfig{AutoMergeThreshold: 0.92, NewThreshold: 0.80},
	})

	id, err := r.Resolve(context.Background(), "RAG-ish")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id == seedID || id == uuid.Nil {
		t.Fatalf("ambiguous: id = %v, want a fresh concept (not seed %v)", id, seedID)
	}
	if n := countConcepts(t, pool); n != 2 {
		t.Fatalf("ambiguous: count = %d, want 2 (seed + new)", n)
	}
	pending, err := proposals.ListPendingProposals(context.Background(), 50, 0)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("ambiguous: pending proposals = %d, want 1", len(pending))
	}
	p := pending[0]
	if p.WinnerID != seedID {
		t.Errorf("proposal winner = %v, want seed %v", p.WinnerID, seedID)
	}
	if p.LoserID != id {
		t.Errorf("proposal loser = %v, want new concept %v", p.LoserID, id)
	}
	if p.Score < 0.84 || p.Score > 0.88 {
		t.Errorf("proposal score = %f, want ~0.86", p.Score)
	}
	if !strings.HasPrefix(p.LLMReason, "embedding-ambiguous: cos=") {
		t.Errorf("proposal reason = %q, want prefix %q", p.LLMReason, "embedding-ambiguous: cos=")
	}
	// The gauge-backing count must agree with the list and the repo method.
	if n := countPendingProposals(t, pool); n != 1 {
		t.Errorf("CountPendingProposals(SQL) = %d, want 1", n)
	}
	if n, err := proposals.CountPendingProposals(context.Background()); err != nil || n != 1 {
		t.Errorf("repo.CountPendingProposals = (%d, %v), want (1, nil)", n, err)
	}
}

// TestVectorGateModelFilterIgnoresOtherModelVectors: a neighbour embedded
// under a DIFFERENT model must be invisible to the gate (incompatible
// vector space), so a probe that would auto-merge against it instead
// creates a new concept (cold table for the current model).
func TestVectorGateModelFilterIgnoresOtherModelVectors(t *testing.T) {
	pool := StartPostgres(t)
	store := repository.NewPGXConceptRepository(pool)
	proposals := repository.NewPGXConceptProposalRepository(pool)
	seedConcept(t, pool, "StaleModelConcept", "some-other-model")

	r := concept.NewResolver(concept.ResolverOptions{
		Store:     store,
		Embedder:  scriptedEmbedder{vec: unitVecAtCosine(0.99)}, // would auto-merge if visible
		Proposals: proposals,
		Gate:      concept.GateConfig{AutoMergeThreshold: 0.92, NewThreshold: 0.80},
	})

	id, err := r.Resolve(context.Background(), "Probe")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("expected a new concept")
	}
	if n := countConcepts(t, pool); n != 2 {
		t.Fatalf("model filter: count = %d, want 2 (stale seed + new under current model)", n)
	}
	if model, hasVec := conceptVectorModel(t, pool, id); !hasVec || model != gateTestModel {
		t.Fatalf("new concept should carry the current model %q; hasVec=%v model=%q", gateTestModel, hasVec, model)
	}
	if n := countPendingProposals(t, pool); n != 0 {
		t.Fatalf("model filter created %d proposals, want 0 (cold table → plain new concept)", n)
	}
}

// --- raw SQL helpers (independent of the code under test) -----------

func seedConcept(t *testing.T, pool *pgxpool.Pool, primaryName, model string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO concept (primary_name, embedding, embedding_model)
		 VALUES ($1, $2, $3) RETURNING id`,
		primaryName, pgvector.NewVector(e0()), model,
	).Scan(&id); err != nil {
		t.Fatalf("seed concept %q: %v", primaryName, err)
	}
	return id
}

func countConcepts(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM concept`).Scan(&n); err != nil {
		t.Fatalf("count concepts: %v", err)
	}
	return n
}

func countPendingProposals(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM concept_merge_proposal WHERE status = 'pending'`).Scan(&n); err != nil {
		t.Fatalf("count pending proposals: %v", err)
	}
	return n
}

func aliasConceptID(t *testing.T, pool *pgxpool.Pool, alias string) (uuid.UUID, bool) {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT concept_id FROM concept_alias WHERE alias = $1`, alias).Scan(&id); err != nil {
		return uuid.Nil, false
	}
	return id, true
}

func aliasSource(t *testing.T, pool *pgxpool.Pool, alias string) string {
	t.Helper()
	var src string
	if err := pool.QueryRow(context.Background(),
		`SELECT source FROM concept_alias WHERE alias = $1`, alias).Scan(&src); err != nil {
		t.Fatalf("alias source for %q: %v", alias, err)
	}
	return src
}

func conceptVectorModel(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (string, bool) {
	t.Helper()
	var model *string
	var hasVec bool
	if err := pool.QueryRow(context.Background(),
		`SELECT embedding_model, embedding IS NOT NULL FROM concept WHERE id = $1`, id).
		Scan(&model, &hasVec); err != nil {
		t.Fatalf("concept vector/model for %v: %v", id, err)
	}
	if model == nil {
		return "", hasVec
	}
	return *model, hasVec
}
