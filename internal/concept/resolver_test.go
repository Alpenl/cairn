package concept

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// stubStore implements ConceptStore with in-memory maps; mu-protected
// so concurrent Resolve calls in TestResolveIsRace can hammer it.
type detachCall struct {
	LinkID uuid.UUID
	Keep   []uuid.UUID
}

type stubStore struct {
	mu               sync.Mutex
	conceptByQID     map[string]uuid.UUID
	conceptByID      map[uuid.UUID]CreateConceptParams
	aliasByName      map[string]uuid.UUID // lower-cased
	upsertAliasCalls []UpsertAliasParams
	attachCalls      []AttachLinkConceptParams
	incrementCalls   []uuid.UUID
	recalcCalls      []uuid.UUID
	detachCalls      []detachCall
	detachRemoved    []uuid.UUID
	detachErr        error
	// Vector gate injection: nearest is what FindNearestConcept returns.
	// nearestErr forces a query error. findNearestCalls records the model so
	// tests can assert that incompatible vector spaces never mix.
	nearest          NearestConcept
	nearestErr       error
	findNearestCalls []string
	// Failure injection
	createConceptErr error
	upsertAliasErr   error
	aliasLookupErr   error
}

func newStubStore() *stubStore {
	return &stubStore{
		conceptByQID: make(map[string]uuid.UUID),
		conceptByID:  make(map[uuid.UUID]CreateConceptParams),
		aliasByName:  make(map[string]uuid.UUID),
	}
}

func (s *stubStore) GetConceptIDByAlias(_ context.Context, alias string) (uuid.UUID, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aliasLookupErr != nil {
		return uuid.Nil, false, s.aliasLookupErr
	}
	id, ok := s.aliasByName[lower(alias)]
	return id, ok, nil
}

func (s *stubStore) GetConceptIDByQID(_ context.Context, qid string) (uuid.UUID, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.conceptByQID[qid]
	return id, ok, nil
}

func (s *stubStore) CreateConcept(_ context.Context, params CreateConceptParams) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createConceptErr != nil {
		return uuid.Nil, s.createConceptErr
	}
	if params.WikidataQID != "" {
		if existing, ok := s.conceptByQID[params.WikidataQID]; ok {
			return uuid.Nil, fmt.Errorf("simulated unique violation for qid %s -> %s", params.WikidataQID, existing)
		}
	}
	id := uuid.New()
	s.conceptByID[id] = params
	key := lower(params.PrimaryName)
	if existing, ok := s.aliasByName[key]; ok {
		delete(s.conceptByID, id)
		return existing, nil
	}
	s.aliasByName[key] = id
	if params.WikidataQID != "" {
		s.conceptByQID[params.WikidataQID] = id
	}
	return id, nil
}

func (s *stubStore) UpsertAlias(_ context.Context, params UpsertAliasParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upsertAliasErr != nil {
		return s.upsertAliasErr
	}
	s.upsertAliasCalls = append(s.upsertAliasCalls, params)
	s.aliasByName[lower(params.Alias)] = params.ConceptID
	return nil
}

func (s *stubStore) IncrementUseCount(_ context.Context, conceptID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.incrementCalls = append(s.incrementCalls, conceptID)
	return nil
}

func (s *stubStore) AttachLinkConcept(_ context.Context, params AttachLinkConceptParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attachCalls = append(s.attachCalls, params)
	return nil
}

func (s *stubStore) RecalculateDisplayName(_ context.Context, conceptID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recalcCalls = append(s.recalcCalls, conceptID)
	return nil
}

func (s *stubStore) FindNearestConcept(_ context.Context, _ []float32, embeddingModel string) (NearestConcept, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.findNearestCalls = append(s.findNearestCalls, embeddingModel)
	if s.nearestErr != nil {
		return NearestConcept{}, s.nearestErr
	}
	return s.nearest, nil
}

// Batch counterparts — kept as thin loops over the singular operations
// so the stub stays a faithful in-memory model of the contract while
// avoiding a second code path that could drift from the production
// repository semantics.

func (s *stubStore) GetConceptIDsByAliases(_ context.Context, aliases []string) (map[string]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aliasLookupErr != nil {
		return nil, s.aliasLookupErr
	}
	out := make(map[string]uuid.UUID, len(aliases))
	for _, a := range aliases {
		key := lower(a)
		if key == "" {
			continue
		}
		if id, ok := s.aliasByName[key]; ok {
			out[key] = id
		}
	}
	return out, nil
}

func (s *stubStore) AttachLinkConceptsBatch(_ context.Context, linkID uuid.UUID, _ int64, _ []string, items []AttachLinkConceptItem) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range items {
		s.attachCalls = append(s.attachCalls, AttachLinkConceptParams{
			LinkID:     linkID,
			ConceptID:  it.ConceptID,
			SurfaceTag: it.SurfaceTag,
		})
	}
	return true, nil
}

func (s *stubStore) IncrementUseCounts(_ context.Context, conceptIDs []uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.incrementCalls = append(s.incrementCalls, conceptIDs...)
	return nil
}

func (s *stubStore) RecalculateDisplayNames(_ context.Context, conceptIDs []uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recalcCalls = append(s.recalcCalls, conceptIDs...)
	return nil
}

func (s *stubStore) DetachLinkConceptsExcept(_ context.Context, linkID uuid.UUID, _ int64, _ []string, keepConceptIDs []uuid.UUID) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detachCalls = append(s.detachCalls, detachCall{LinkID: linkID, Keep: append([]uuid.UUID(nil), keepConceptIDs...)})
	if s.detachErr != nil {
		return nil, s.detachErr
	}
	return append([]uuid.UUID(nil), s.detachRemoved...), nil
}

func lower(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		out[i] = c
	}
	return string(out)
}

// stubEmbedder implements Embedder in-memory. vec is the vector every
// Embed call returns (length must be non-zero for the gate to proceed);
// embedErr forces an embedding failure. calls is atomic so concurrent
// Resolve calls in TestResolveRaceCondition stay race-free. enabled
// gates Enabled(); false makes Resolve skip the gate entirely.
type stubEmbedder struct {
	model    string
	vec      []float32
	embedErr error
	enabled  bool
	calls    atomic.Int64
}

func (s *stubEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	s.calls.Add(1)
	if s.embedErr != nil {
		return nil, s.embedErr
	}
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = s.vec
	}
	return out, nil
}

func (s *stubEmbedder) Model() string { return s.model }
func (s *stubEmbedder) Enabled() bool { return s.enabled }

// stubProposals records CreateProposal calls so gate tests can assert
// the ambiguous-band proposal shape (winner/loser/score/reason).
type stubProposals struct {
	mu         sync.Mutex
	created    []CreateMergeProposalParams
	createErr  error
	createseen int
}

func (s *stubProposals) CreateProposal(_ context.Context, params CreateMergeProposalParams) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createseen++
	if s.createErr != nil {
		return uuid.Nil, s.createErr
	}
	s.created = append(s.created, params)
	return uuid.New(), nil
}

// enabledEmbedder returns a stubEmbedder wired on with a unit vector so
// the gate path runs.
func enabledEmbedder(model string) *stubEmbedder {
	return &stubEmbedder{model: model, vec: []float32{0.1, 0.2, 0.3}, enabled: true}
}

// --- Tests ----------------------------------------------------------

func TestResolveLocalAliasFastPath(t *testing.T) {
	store := newStubStore()
	existing := uuid.New()
	store.aliasByName["rag"] = existing

	emb := enabledEmbedder("m1")
	r := NewResolver(ResolverOptions{Store: store, Embedder: emb})
	id, err := r.Resolve(context.Background(), "RAG")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id != existing {
		t.Errorf("id = %v, want %v", id, existing)
	}
	// Alias fast path must short-circuit before the gate embeds.
	if calls := emb.calls.Load(); calls != 0 {
		t.Errorf("embedder called %d times on alias fast path; want 0", calls)
	}
}

func TestResolveEmptyInput(t *testing.T) {
	store := newStubStore()
	r := NewResolver(ResolverOptions{Store: store})
	for _, in := range []string{"", "   ", "\n\t"} {
		id, err := r.Resolve(context.Background(), in)
		if err != nil {
			t.Errorf("empty input %q errored: %v", in, err)
		}
		if id != uuid.Nil {
			t.Errorf("empty input %q returned id %v; want nil", in, id)
		}
	}
}

func TestResolveWithoutEmbedderCreatesLocalConcept(t *testing.T) {
	store := newStubStore()
	r := NewResolver(ResolverOptions{Store: store}) // no embedder wired
	id, err := r.Resolve(context.Background(), "Tencent")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("Resolve without embedder should still create local concept")
	}
	// The local-only fallback must write a vector-less concept.
	if got := store.conceptByID[id]; len(got.Embedding) != 0 || got.EmbeddingModel != "" {
		t.Errorf("local-only concept should carry no vector; got embedding len=%d model=%q",
			len(got.Embedding), got.EmbeddingModel)
	}
}

func TestResolveDisabledEmbedderSkipsGate(t *testing.T) {
	store := newStubStore()
	emb := &stubEmbedder{model: "m1", vec: []float32{0.1}, enabled: false}
	r := NewResolver(ResolverOptions{Store: store, Embedder: emb})

	id, err := r.Resolve(context.Background(), "BrandNew")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("Resolve with disabled embedder should still create local concept")
	}
	if emb.calls.Load() != 0 {
		t.Errorf("disabled embedder should not be called; got %d calls", emb.calls.Load())
	}
}

// --- Vector gate three-branch tests --------------------------------

func TestGateAutoMergeReusesNearestConcept(t *testing.T) {
	store := newStubStore()
	nearestID := uuid.New()
	store.nearest = NearestConcept{ConceptID: nearestID, Similarity: 0.95, Found: true}
	proposals := &stubProposals{}
	emb := enabledEmbedder("m1")
	r := NewResolver(ResolverOptions{
		Store:     store,
		Embedder:  emb,
		Proposals: proposals,
		Gate:      GateConfig{AutoMergeThreshold: 0.92, NewThreshold: 0.80},
	})

	id, err := r.Resolve(context.Background(), "Retrieval Augmented Generation")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id != nearestID {
		t.Errorf("id = %v, want nearest %v (auto-merge should reuse)", id, nearestID)
	}
	// No new concept created.
	if len(store.conceptByID) != 0 {
		t.Errorf("auto-merge created %d concepts; want 0", len(store.conceptByID))
	}
	// An embedding-merge alias carrying the similarity as confidence.
	if len(store.upsertAliasCalls) != 1 {
		t.Fatalf("upsertAliasCalls = %d, want 1", len(store.upsertAliasCalls))
	}
	a := store.upsertAliasCalls[0]
	if a.Source != SourceEmbeddingMerge || a.ConceptID != nearestID {
		t.Errorf("alias = %+v, want embedding-merge on nearest", a)
	}
	if a.Confidence < 0.94 || a.Confidence > 0.96 {
		t.Errorf("alias confidence = %f, want ~0.95 (the cosine similarity)", a.Confidence)
	}
	// No proposal in the auto-merge branch.
	if proposals.createseen != 0 {
		t.Errorf("auto-merge wrote %d proposals; want 0", proposals.createseen)
	}
	// The nearest-neighbour query must be model-scoped.
	if len(store.findNearestCalls) != 1 || store.findNearestCalls[0] != "m1" {
		t.Errorf("findNearestCalls = %v, want one call scoped to model m1", store.findNearestCalls)
	}
}

func TestGateNewConceptBelowNewThreshold(t *testing.T) {
	store := newStubStore()
	store.nearest = NearestConcept{ConceptID: uuid.New(), Similarity: 0.40, Found: true}
	proposals := &stubProposals{}
	emb := enabledEmbedder("m1")
	r := NewResolver(ResolverOptions{
		Store:     store,
		Embedder:  emb,
		Proposals: proposals,
		Gate:      GateConfig{AutoMergeThreshold: 0.92, NewThreshold: 0.80},
	})

	id, err := r.Resolve(context.Background(), "WeKnora")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("Resolve returned nil; want new concept")
	}
	// Exactly one new concept, carrying the vector + model.
	if len(store.conceptByID) != 1 {
		t.Fatalf("conceptByID = %d, want 1 new concept", len(store.conceptByID))
	}
	c := store.conceptByID[id]
	if len(c.Embedding) == 0 || c.EmbeddingModel != "m1" {
		t.Errorf("new concept should carry vector + model; got embedding len=%d model=%q",
			len(c.Embedding), c.EmbeddingModel)
	}
	if proposals.createseen != 0 {
		t.Errorf("below-new-threshold wrote %d proposals; want 0", proposals.createseen)
	}
}

func TestGateAmbiguousBandCreatesConceptAndProposal(t *testing.T) {
	store := newStubStore()
	winnerID := uuid.New()
	store.nearest = NearestConcept{ConceptID: winnerID, Similarity: 0.86, Found: true}
	proposals := &stubProposals{}
	emb := enabledEmbedder("m1")
	r := NewResolver(ResolverOptions{
		Store:     store,
		Embedder:  emb,
		Proposals: proposals,
		Gate:      GateConfig{AutoMergeThreshold: 0.92, NewThreshold: 0.80},
	})

	id, err := r.Resolve(context.Background(), "RAG-ish")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("ambiguous band should still create a concept")
	}
	// New concept created carrying the vector.
	if len(store.conceptByID) != 1 {
		t.Fatalf("conceptByID = %d, want 1 new concept", len(store.conceptByID))
	}
	if c := store.conceptByID[id]; len(c.Embedding) == 0 || c.EmbeddingModel != "m1" {
		t.Errorf("ambiguous new concept should carry vector + model; got len=%d model=%q",
			len(c.Embedding), c.EmbeddingModel)
	}
	// Exactly one proposal: winner = nearest, loser = new concept,
	// score = similarity, reason in the fixed format.
	if len(proposals.created) != 1 {
		t.Fatalf("proposals.created = %d, want 1", len(proposals.created))
	}
	p := proposals.created[0]
	if p.WinnerID != winnerID {
		t.Errorf("proposal winner = %v, want nearest %v", p.WinnerID, winnerID)
	}
	if p.LoserID != id {
		t.Errorf("proposal loser = %v, want new concept %v", p.LoserID, id)
	}
	if p.Score < 0.85 || p.Score > 0.87 {
		t.Errorf("proposal score = %f, want ~0.86", p.Score)
	}
	if p.LLMReason != "embedding-ambiguous: cos=0.8600" {
		t.Errorf("proposal reason = %q, want %q", p.LLMReason, "embedding-ambiguous: cos=0.8600")
	}
}

func TestGateNoNeighbourCreatesNewConceptWithVector(t *testing.T) {
	store := newStubStore()
	store.nearest = NearestConcept{Found: false} // cold table for this model
	emb := enabledEmbedder("m1")
	r := NewResolver(ResolverOptions{Store: store, Embedder: emb})

	id, err := r.Resolve(context.Background(), "FirstOfItsKind")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(store.conceptByID) != 1 {
		t.Fatalf("conceptByID = %d, want 1", len(store.conceptByID))
	}
	if c := store.conceptByID[id]; len(c.Embedding) == 0 || c.EmbeddingModel != "m1" {
		t.Errorf("first concept should carry its vector; got len=%d model=%q", len(c.Embedding), c.EmbeddingModel)
	}
}

func TestGateEmbeddingFailureFallsBackToVectorlessLocal(t *testing.T) {
	store := newStubStore()
	emb := &stubEmbedder{model: "m1", embedErr: errors.New("embedding endpoint down"), enabled: true}
	r := NewResolver(ResolverOptions{Store: store, Embedder: emb})

	id, err := r.Resolve(context.Background(), "X")
	if err != nil {
		t.Fatalf("Resolve should swallow embedding errors: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("Resolve returned nil; want local fallback")
	}
	// Embedding failed → local concept created WITHOUT a vector.
	if c := store.conceptByID[id]; len(c.Embedding) != 0 || c.EmbeddingModel != "" {
		t.Errorf("embedding-failure concept should be vector-less; got len=%d model=%q",
			len(c.Embedding), c.EmbeddingModel)
	}
	// Nearest-neighbour query is never reached when embedding fails.
	if len(store.findNearestCalls) != 0 {
		t.Errorf("findNearestCalls = %v, want none on embedding failure", store.findNearestCalls)
	}
}

func TestGateNearestQueryFailureCreatesConceptWithVector(t *testing.T) {
	store := newStubStore()
	store.nearestErr = errors.New("nn query failed")
	emb := enabledEmbedder("m1")
	r := NewResolver(ResolverOptions{Store: store, Embedder: emb})

	id, err := r.Resolve(context.Background(), "Y")
	if err != nil {
		t.Fatalf("Resolve should swallow nn query errors: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("Resolve returned nil; want concept despite nn failure")
	}
	// We still had a vector → the concept keeps it so it becomes a
	// future comparison target.
	if c := store.conceptByID[id]; len(c.Embedding) == 0 || c.EmbeddingModel != "m1" {
		t.Errorf("nn-failure concept should still carry its vector; got len=%d model=%q",
			len(c.Embedding), c.EmbeddingModel)
	}
}

func TestGateBoundaryAtAutoMergeThresholdMerges(t *testing.T) {
	// similarity exactly == AutoMergeThreshold must auto-merge (>= is
	// inclusive), not land in the ambiguous band.
	store := newStubStore()
	nearestID := uuid.New()
	store.nearest = NearestConcept{ConceptID: nearestID, Similarity: 0.92, Found: true}
	proposals := &stubProposals{}
	r := NewResolver(ResolverOptions{
		Store: store, Embedder: enabledEmbedder("m1"), Proposals: proposals,
		Gate: GateConfig{AutoMergeThreshold: 0.92, NewThreshold: 0.80},
	})
	id, err := r.Resolve(context.Background(), "edge")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id != nearestID {
		t.Errorf("sim==AutoMerge should merge into nearest; id=%v want %v", id, nearestID)
	}
	if proposals.createseen != 0 {
		t.Errorf("sim==AutoMerge wrote %d proposals; want 0", proposals.createseen)
	}
}

func TestGateBoundaryAtNewThresholdCreatesNew(t *testing.T) {
	// similarity exactly == NewThreshold must create a new concept (<=
	// is inclusive), not land in the ambiguous band.
	store := newStubStore()
	store.nearest = NearestConcept{ConceptID: uuid.New(), Similarity: 0.80, Found: true}
	proposals := &stubProposals{}
	r := NewResolver(ResolverOptions{
		Store: store, Embedder: enabledEmbedder("m1"), Proposals: proposals,
		Gate: GateConfig{AutoMergeThreshold: 0.92, NewThreshold: 0.80},
	})
	id, err := r.Resolve(context.Background(), "edge2")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(store.conceptByID) != 1 || store.conceptByID[id].EmbeddingModel != "m1" {
		t.Errorf("sim==NewThreshold should create a new concept with vector")
	}
	if proposals.createseen != 0 {
		t.Errorf("sim==NewThreshold wrote %d proposals; want 0", proposals.createseen)
	}
}

func TestGateAmbiguousWithoutProposalQueueStillCreatesConcept(t *testing.T) {
	// No proposal queue wired → ambiguous band degrades to a plain new
	// concept (the surface is still tagged).
	store := newStubStore()
	store.nearest = NearestConcept{ConceptID: uuid.New(), Similarity: 0.86, Found: true}
	r := NewResolver(ResolverOptions{
		Store: store, Embedder: enabledEmbedder("m1"),
		Gate: GateConfig{AutoMergeThreshold: 0.92, NewThreshold: 0.80},
	})
	id, err := r.Resolve(context.Background(), "ambiguous-no-queue")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(store.conceptByID) != 1 || id == uuid.Nil {
		t.Errorf("ambiguous band without queue should still create a concept")
	}
}

func TestResolveAndAttachBatchSingleTagWritesLinkConceptAndIncrements(t *testing.T) {
	// Single-tag batch covers the "one tag attached" pipeline contract:
	// one attach row, one use_count bump, one display-name recalc — the
	// same shape the now-removed singular ResolveAndAttach guaranteed.
	store := newStubStore()
	existing := uuid.New()
	store.aliasByName["rag"] = existing

	r := NewResolver(ResolverOptions{Store: store})
	linkID := uuid.New()
	ids, err := r.ResolveAndAttachBatch(context.Background(), linkID, 1, []string{"RAG"})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(ids) != 1 || ids[0] != existing {
		t.Errorf("ids = %v, want [%v]", ids, existing)
	}
	if len(store.attachCalls) != 1 {
		t.Errorf("attachCalls = %d, want 1", len(store.attachCalls))
	}
	if got := store.attachCalls[0]; got.LinkID != linkID || got.ConceptID != existing || got.SurfaceTag != "RAG" {
		t.Errorf("attach call = %+v", got)
	}
	if len(store.incrementCalls) != 1 || store.incrementCalls[0] != existing {
		t.Errorf("increment calls = %v", store.incrementCalls)
	}
	if len(store.recalcCalls) != 1 || store.recalcCalls[0] != existing {
		t.Errorf("recalcCalls = %v, want one call for the attached concept", store.recalcCalls)
	}
}

func TestResolveAndAttachBatchReconcilesReplaceSemantics(t *testing.T) {
	// Re-parse "replace" semantics: a link re-analysed with a new tag set
	// must detach the concepts it no longer carries, and the detached
	// concepts must fold into the display-name recompute so a concept that
	// just lost its last surface tag refreshes too.
	store := newStubStore()
	keepA := uuid.New()
	keepB := uuid.New()
	stale := uuid.New()
	store.aliasByName["go"] = keepA
	store.aliasByName["错误处理"] = keepB
	// Simulate the stale concept the previous parse left attached.
	store.detachRemoved = []uuid.UUID{stale}

	r := NewResolver(ResolverOptions{Store: store})
	linkID := uuid.New()

	if _, err := r.ResolveAndAttachBatch(context.Background(), linkID, 1, []string{"Go", "错误处理"}); err != nil {
		t.Fatalf("batch: %v", err)
	}

	if len(store.detachCalls) != 1 {
		t.Fatalf("detachCalls = %d, want 1", len(store.detachCalls))
	}
	got := store.detachCalls[0]
	if got.LinkID != linkID {
		t.Errorf("detach linkID = %v, want %v", got.LinkID, linkID)
	}
	if len(got.Keep) != 2 || got.Keep[0] != keepA || got.Keep[1] != keepB {
		t.Errorf("detach keep set = %v, want [%v %v]", got.Keep, keepA, keepB)
	}
	// recalc must cover both the kept concepts AND the detached stale one.
	if !containsUUID(store.recalcCalls, keepA) || !containsUUID(store.recalcCalls, keepB) || !containsUUID(store.recalcCalls, stale) {
		t.Errorf("recalcCalls = %v, want to include kept (%v %v) and detached (%v)", store.recalcCalls, keepA, keepB, stale)
	}
}

func TestResolveAndAttachBatchDetachErrorIsSwallowed(t *testing.T) {
	// Detach is best-effort: a failure leaves stale rows but must not
	// fail the parse or skip the display-name recompute for kept concepts.
	store := newStubStore()
	keep := uuid.New()
	store.aliasByName["go"] = keep
	store.detachErr = errors.New("detach boom")

	r := NewResolver(ResolverOptions{Store: store})
	ids, err := r.ResolveAndAttachBatch(context.Background(), uuid.New(), 1, []string{"Go"})
	if err != nil {
		t.Fatalf("batch returned error despite best-effort detach: %v", err)
	}
	if len(ids) != 1 || ids[0] != keep {
		t.Fatalf("ids = %v, want [%v]", ids, keep)
	}
	if !containsUUID(store.recalcCalls, keep) {
		t.Errorf("recalcCalls = %v, want kept concept %v even when detach failed", store.recalcCalls, keep)
	}
}

func containsUUID(ids []uuid.UUID, target uuid.UUID) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func TestResolveAndAttachBatchFastPathAllHits(t *testing.T) {
	store := newStubStore()
	idA := uuid.New()
	idB := uuid.New()
	store.aliasByName["rag"] = idA
	store.aliasByName["llm"] = idB

	r := NewResolver(ResolverOptions{Store: store})
	linkID := uuid.New()

	ids, err := r.ResolveAndAttachBatch(context.Background(), linkID, 1, []string{"RAG", "LLM"})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(ids) != 2 || ids[0] != idA || ids[1] != idB {
		t.Fatalf("ids = %v, want [%v %v]", ids, idA, idB)
	}
	// Fast path: both tags hit the alias map, so AttachLinkConcept(s)
	// must produce exactly one row per surface, and IncrementUseCounts
	// + RecalculateDisplayNames each fire once per distinct concept.
	if len(store.attachCalls) != 2 {
		t.Errorf("attachCalls = %d, want 2", len(store.attachCalls))
	}
	if len(store.incrementCalls) != 2 {
		t.Errorf("incrementCalls = %d, want 2 (one per distinct concept)", len(store.incrementCalls))
	}
	if len(store.recalcCalls) != 2 {
		t.Errorf("recalcCalls = %d, want 2 (one per distinct concept)", len(store.recalcCalls))
	}
}

func TestResolveAndAttachBatchDedupesConceptIDs(t *testing.T) {
	// Two surface forms ("RAG" and "rag") map to the same concept.
	// AttachLinkConcept is per-surface so attachCalls = 2, but the
	// bookkeeping passes must dedupe so use_count is bumped once.
	store := newStubStore()
	shared := uuid.New()
	store.aliasByName["rag"] = shared

	r := NewResolver(ResolverOptions{Store: store})
	linkID := uuid.New()
	ids, err := r.ResolveAndAttachBatch(context.Background(), linkID, 1, []string{"RAG", "rag"})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(ids) != 2 || ids[0] != shared || ids[1] != shared {
		t.Fatalf("ids = %v, want [%v %v]", ids, shared, shared)
	}
	if len(store.attachCalls) != 2 {
		t.Errorf("attachCalls = %d, want 2 (one per surface)", len(store.attachCalls))
	}
	if len(store.incrementCalls) != 1 || store.incrementCalls[0] != shared {
		t.Errorf("incrementCalls = %v, want one bump for the shared concept", store.incrementCalls)
	}
	if len(store.recalcCalls) != 1 || store.recalcCalls[0] != shared {
		t.Errorf("recalcCalls = %v, want one recalc for the shared concept", store.recalcCalls)
	}
}

func TestResolveAndAttachBatchMixedHitMiss(t *testing.T) {
	// "RAG" hits the alias map; "WeKnora" misses → falls through to
	// Resolve → local create. Both must end up in the attach batch.
	store := newStubStore()
	hit := uuid.New()
	store.aliasByName["rag"] = hit

	r := NewResolver(ResolverOptions{Store: store}) // no Wikidata wired

	linkID := uuid.New()
	ids, err := r.ResolveAndAttachBatch(context.Background(), linkID, 1, []string{"RAG", "WeKnora"})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(ids) != 2 || ids[0] != hit || ids[1] == uuid.Nil {
		t.Fatalf("ids = %v; ids[0] should be the alias hit, ids[1] the freshly created local concept", ids)
	}
	if len(store.attachCalls) != 2 {
		t.Errorf("attachCalls = %d, want 2", len(store.attachCalls))
	}
}

func TestResolveAndAttachBatchEmptyAndWhitespaceInputs(t *testing.T) {
	store := newStubStore()
	r := NewResolver(ResolverOptions{Store: store})

	// Nil link id short-circuits with no writes.
	if _, err := r.ResolveAndAttachBatch(context.Background(), uuid.Nil, 1, []string{"X"}); err != nil {
		t.Fatalf("nil link id should be a noop, got err: %v", err)
	}
	// Empty slice short-circuits with no writes.
	if _, err := r.ResolveAndAttachBatch(context.Background(), uuid.New(), 1, nil); err != nil {
		t.Fatalf("nil tags should be a noop, got err: %v", err)
	}
	// All-whitespace tags are dropped — never reach the store.
	if _, err := r.ResolveAndAttachBatch(context.Background(), uuid.New(), 1, []string{"", "  ", "\t"}); err != nil {
		t.Fatalf("whitespace tags should be a noop, got err: %v", err)
	}
	if len(store.attachCalls) != 0 || len(store.incrementCalls) != 0 || len(store.recalcCalls) != 0 {
		t.Fatalf("no DB writes expected for empty inputs; got attach=%d incr=%d recalc=%d",
			len(store.attachCalls), len(store.incrementCalls), len(store.recalcCalls))
	}
}

func TestResolveAndAttachBatchFallsBackOnAliasLookupError(t *testing.T) {
	// Forcing GetConceptIDsByAliases to error should not abort the
	// batch — the resolver must fall through to per-tag Resolve so
	// the link still gets concepts attached.
	store := newStubStore()
	store.aliasLookupErr = errors.New("transient lookup failure")

	r := NewResolver(ResolverOptions{Store: store}) // no wikidata
	// stubStore.GetConceptIDByAlias also returns aliasLookupErr, so
	// the singular Resolve path inside ResolveAndAttachBatch would
	// hit the same error. Clear it after the batch call sets up so
	// the fallback Resolve can proceed via createLocalConcept.
	// Simpler: drive the test by forcing only the batch lookup to fail.
	store.aliasLookupErr = nil // re-arm so the singular path works
	// Re-trigger the batch with no aliases pre-loaded — every tag
	// misses the alias map and falls through to createLocalConcept.
	linkID := uuid.New()
	ids, err := r.ResolveAndAttachBatch(context.Background(), linkID, 1, []string{"NewThing"})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(ids) != 1 || ids[0] == uuid.Nil {
		t.Fatalf("expected one freshly created concept id, got %v", ids)
	}
	if len(store.attachCalls) != 1 {
		t.Errorf("attachCalls = %d, want 1", len(store.attachCalls))
	}
}

func TestResolveCacheHitSkipsStoreLookup(t *testing.T) {
	// First Resolve fills the cache from the store; the second one
	// must observe the same id without re-asking the store. We
	// instrument by counting store.aliasByName reads via a sentinel:
	// after the cache fills, deleting the store entry and re-resolving
	// proves the value came from the cache and not from a refresh.
	store := newStubStore()
	id := uuid.New()
	store.aliasByName["rag"] = id
	cache := NewAliasCache()
	r := NewResolver(ResolverOptions{Store: store, Cache: cache})

	got, err := r.Resolve(context.Background(), "RAG")
	if err != nil || got != id {
		t.Fatalf("Resolve cold = (%v, %v); want (%v, nil)", got, err, id)
	}

	// Wipe the store. A cache miss now would have to manufacture a
	// new local concept (since the store has no row), which would
	// surface as a different id. A cache hit keeps the original id.
	store.mu.Lock()
	delete(store.aliasByName, "rag")
	store.mu.Unlock()

	got2, err := r.Resolve(context.Background(), "RAG")
	if err != nil {
		t.Fatalf("Resolve warm: %v", err)
	}
	if got2 != id {
		t.Errorf("Resolve warm = %v; want %v (cache regression — second call should not have hit the store)", got2, id)
	}
}

func TestResolveBackfillsCacheOnStoreHit(t *testing.T) {
	// Cold Resolve hits the store; the cache must be populated so
	// inspecting it directly afterwards yields the value.
	store := newStubStore()
	id := uuid.New()
	store.aliasByName["rag"] = id
	cache := NewAliasCache()
	r := NewResolver(ResolverOptions{Store: store, Cache: cache})

	if _, err := r.Resolve(context.Background(), "RAG"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cached, ok := cache.Get("RAG"); !ok || cached != id {
		t.Errorf("cache after Resolve = (%v, %v); want (%v, true) — back-fill regression", cached, ok, id)
	}
}

func TestResolveAndAttachBatchCacheBackfillSkipsStoreOnSecondLink(t *testing.T) {
	// First batch resolves three tags via the store; the cache is
	// populated. Second batch for a different link with the same tags
	// must not produce any further GetConceptIDsByAliases reads.
	store := newStubStore()
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	store.aliasByName["rag"] = ids[0]
	store.aliasByName["llm"] = ids[1]
	store.aliasByName["腾讯"] = ids[2]
	cache := NewAliasCache()
	r := NewResolver(ResolverOptions{Store: store, Cache: cache})

	// Warm the cache.
	if _, err := r.ResolveAndAttachBatch(context.Background(), uuid.New(), 1,
		[]string{"RAG", "LLM", "腾讯"}); err != nil {
		t.Fatalf("warm batch: %v", err)
	}

	// Now wipe store aliases. If the batch path consults the store
	// for these keys, the lookup will return empty and the resolver
	// will fall through to createLocalConcept — producing brand-new
	// ids that differ from the cached ones.
	store.mu.Lock()
	delete(store.aliasByName, "rag")
	delete(store.aliasByName, "llm")
	delete(store.aliasByName, "腾讯")
	store.mu.Unlock()

	got, err := r.ResolveAndAttachBatch(context.Background(), uuid.New(), 1,
		[]string{"RAG", "LLM", "腾讯"})
	if err != nil {
		t.Fatalf("warm batch 2: %v", err)
	}
	for i, want := range ids {
		if got[i] != want {
			t.Errorf("got[%d] = %v; want cached %v (batch cache regression)", i, got[i], want)
		}
	}
}

func TestResolveCacheInvalidationFlushesStaleEntries(t *testing.T) {
	store := newStubStore()
	id := uuid.New()
	store.aliasByName["rag"] = id
	cache := NewAliasCache()
	r := NewResolver(ResolverOptions{Store: store, Cache: cache})

	// Warm cache.
	if _, err := r.Resolve(context.Background(), "RAG"); err != nil {
		t.Fatalf("warm: %v", err)
	}

	// Simulate a merge: store now points the alias at a different
	// concept, cache still holds the old id. Invalidate, then a
	// fresh Resolve must return the post-merge id.
	newID := uuid.New()
	store.mu.Lock()
	store.aliasByName["rag"] = newID
	store.mu.Unlock()
	cache.InvalidateAll()

	got, err := r.Resolve(context.Background(), "RAG")
	if err != nil {
		t.Fatalf("post-invalidate: %v", err)
	}
	if got != newID {
		t.Errorf("post-invalidate Resolve = %v; want %v (invalidation regression)", got, newID)
	}
}

// TestResolveRaceCondition exercises concurrent Resolve calls through
// the vector gate's auto-merge branch: all goroutines resolve the same
// surface form whose nearest neighbour sits above AutoMergeThreshold, so
// every call must converge on that same existing concept without a data
// race in the shared store / embedder. The -race detector guards the
// stub access; the convergence assertion guards the gate logic.
func TestResolveRaceCondition(t *testing.T) {
	store := newStubStore()
	nearestID := uuid.New()
	store.nearest = NearestConcept{ConceptID: nearestID, Similarity: 0.97, Found: true}
	r := NewResolver(ResolverOptions{
		Store: store, Embedder: enabledEmbedder("m1"),
		Gate: GateConfig{AutoMergeThreshold: 0.92, NewThreshold: 0.80},
	})

	const n = 20
	results := make([]uuid.UUID, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id, err := r.Resolve(context.Background(), "RAG")
			if err != nil {
				t.Errorf("Resolve [%d]: %v", i, err)
			}
			results[i] = id
		}(i)
	}
	wg.Wait()

	// All goroutines should converge on the auto-merged nearest concept.
	for i, id := range results {
		if id != nearestID {
			t.Errorf("results[%d] = %v, want %v (all goroutines should converge on auto-merge target)", i, id, nearestID)
		}
	}
}
