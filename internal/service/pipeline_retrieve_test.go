package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"webtag/internal/concept"
	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/observability"
	analyzerpkg "webtag/internal/service/analyzer"
)

// fakeEmbedder is a configurable RetrievalEmbedder stub. enabled / model /
// err / vec control each axis a gate test wants to exercise.
type fakeEmbedder struct {
	enabled bool
	model   string
	vec     []float32
	err     error
	calls   int
}

func (f *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = f.vec
	}
	return out, nil
}
func (f *fakeEmbedder) Model() string { return f.model }
func (f *fakeEmbedder) Enabled() bool { return f.enabled }

// fakeCandidateLister is a configurable concept.CandidateLister stub.
type fakeCandidateLister struct {
	count          int
	countErr       error
	nearest        []concept.CandidateConcept
	nearestErr     error
	nearestCalls   int
	neighbour      []concept.CandidateConcept
	neighbourErr   error
	neighbourCalls int
	// neighbourExclude records the link id the co-occurrence leg was asked
	// to skip, so tests can assert re-parse does not let a link vote for
	// its own existing tags.
	neighbourExclude uuid.UUID
}

func (f *fakeCandidateLister) ListNearestConcepts(_ context.Context, _ []float32, _ string, _ int) ([]concept.CandidateConcept, error) {
	f.nearestCalls++
	if f.nearestErr != nil {
		return nil, f.nearestErr
	}
	return f.nearest, nil
}
func (f *fakeCandidateLister) ListConceptsOfNearestLinks(_ context.Context, _ []float32, _ string, _, _ int, excludeLinkID uuid.UUID) ([]concept.CandidateConcept, error) {
	f.neighbourCalls++
	f.neighbourExclude = excludeLinkID
	if f.neighbourErr != nil {
		return nil, f.neighbourErr
	}
	return f.neighbour, nil
}
func (f *fakeCandidateLister) CountConcepts(_ context.Context) (int, error) {
	if f.countErr != nil {
		return 0, f.countErr
	}
	return f.count, nil
}

func sampleContent() fetcher.Content {
	return fetcher.Content{URL: "https://example.com/x", Title: "标题", Body: "正文内容"}
}

func warmLister(nearest []concept.CandidateConcept) *fakeCandidateLister {
	return &fakeCandidateLister{count: 100, nearest: nearest}
}

func liveEmbedder() *fakeEmbedder {
	return &fakeEmbedder{enabled: true, model: "m1", vec: []float32{0.1, 0.2}}
}

// TestRetrievalGateRecallsCandidatesWhenWarm: warm词表 + working embedder +
// non-empty recall → returns the candidate set, no fallback.
func TestRetrievalGateRecallsCandidatesWhenWarm(t *testing.T) {
	t.Parallel()

	want := cands("RAG", "WeKnora")
	g := newRetrievalGate(liveEmbedder(), warmLister(want), 40, 30, nil)

	got, reason, _ := g.recall(context.Background(), uuid.Nil, sampleContent())
	if reason != "" {
		t.Fatalf("recall fallback reason = %q, want none", reason)
	}
	if len(got) != 2 || got[0].Name != "RAG" || got[1].Name != "WeKnora" {
		t.Fatalf("candidates = %#v, want [RAG WeKnora]", got)
	}
}

// TestRetrievalGateColdStartFallback: 词表 below ColdStartMin must skip
// embedding entirely and fall back with reason "cold_start".
func TestRetrievalGateColdStartFallback(t *testing.T) {
	t.Parallel()

	emb := liveEmbedder()
	lister := &fakeCandidateLister{count: 5, nearest: cands("RAG")}
	g := newRetrievalGate(emb, lister, 40, 30, nil)

	got, reason, _ := g.recall(context.Background(), uuid.Nil, sampleContent())
	if got != nil {
		t.Errorf("cold start should return nil candidates, got %#v", got)
	}
	if reason != retrieveFallbackColdStart {
		t.Errorf("reason = %q, want %q", reason, retrieveFallbackColdStart)
	}
	if emb.calls != 0 {
		t.Errorf("cold start must not call the embedder; calls = %d", emb.calls)
	}
	if lister.nearestCalls != 0 {
		t.Errorf("cold start must not run recall; nearestCalls = %d", lister.nearestCalls)
	}
}

// TestRetrievalGateEmbeddingFailureFallback: embedding error → reason
// "embedding_failed", recall never attempted.
func TestRetrievalGateEmbeddingFailureFallback(t *testing.T) {
	t.Parallel()

	emb := &fakeEmbedder{enabled: true, model: "m1", err: errors.New("upstream down")}
	lister := warmLister(cands("RAG"))
	g := newRetrievalGate(emb, lister, 40, 30, nil)

	got, reason, _ := g.recall(context.Background(), uuid.Nil, sampleContent())
	if got != nil {
		t.Errorf("embedding failure should return nil candidates, got %#v", got)
	}
	if reason != retrieveFallbackEmbeddingFailed {
		t.Errorf("reason = %q, want %q", reason, retrieveFallbackEmbeddingFailed)
	}
	if lister.nearestCalls != 0 {
		t.Errorf("embedding failure must not run recall; nearestCalls = %d", lister.nearestCalls)
	}
}

func TestRecallFallbackLogDoesNotExposePrivateURL(t *testing.T) {
	t.Parallel()

	const privateURL = "https://reader:secret@example.com/private/account/path?token=review-secret#fragment"
	var output bytes.Buffer
	p := &ParsePipeline{
		retrieval: newRetrievalGate(liveEmbedder(), &fakeCandidateLister{count: 1}, 40, 30, nil),
		logger:    slog.New(slog.NewJSONHandler(&output, nil)),
	}
	linkID := uuid.New()
	p.recallCandidates(context.Background(), linkID, fetcher.Content{URL: privateURL, Title: "private", Body: "body"})

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if record["url_host"] != "example.com" || record["link_id"] != linkID.String() {
		t.Fatalf("safe correlation fields = %#v", record)
	}
	encoded := output.String()
	for _, secret := range []string{"reader:secret", "/private/account/path", "review-secret", "#fragment", privateURL} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("log leaked %q: %s", secret, encoded)
		}
	}
}

func TestPartialRecallLogDoesNotExposePrivateURL(t *testing.T) {
	t.Parallel()

	const privateURL = "https://reader:secret@example.com/private/account/path?token=review-secret#fragment"
	var output bytes.Buffer
	p := &ParsePipeline{
		retrieval: newRetrievalGate(liveEmbedder(), &fakeCandidateLister{
			count:        100,
			nearest:      cands("RAG"),
			neighbourErr: errors.New("neighbour lookup failed"),
		}, 40, 30, nil),
		logger: slog.New(slog.NewJSONHandler(&output, nil)),
	}
	linkID := uuid.New()
	candidates := p.recallCandidates(context.Background(), linkID, fetcher.Content{URL: privateURL, Title: "private", Body: "body"})
	if len(candidates) != 1 || candidates[0].Name != "RAG" {
		t.Fatalf("partial candidates = %#v, want surviving direct candidate", candidates)
	}

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if record["url_host"] != "example.com" || record["link_id"] != linkID.String() {
		t.Fatalf("safe correlation fields = %#v", record)
	}
	encoded := output.String()
	for _, secret := range []string{"reader:secret", "/private/account/path", "review-secret", "#fragment", privateURL} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("log leaked %q: %s", secret, encoded)
		}
	}
}

// TestRetrievalGateRecallErrorFallback: nearest-neighbour query error →
// reason "retrieve_failed".
func TestRetrievalGateRecallErrorFallback(t *testing.T) {
	t.Parallel()

	lister := &fakeCandidateLister{count: 100, nearestErr: errors.New("pg down")}
	g := newRetrievalGate(liveEmbedder(), lister, 40, 30, nil)

	got, reason, _ := g.recall(context.Background(), uuid.Nil, sampleContent())
	if got != nil {
		t.Errorf("recall error should return nil candidates, got %#v", got)
	}
	if reason != retrieveFallbackRetrieveFailed {
		t.Errorf("reason = %q, want %q", reason, retrieveFallbackRetrieveFailed)
	}
}

// TestRetrievalGateEmptyRecallFallback: warm词表 but recall returns nothing
// (no vectors for this model yet) → reason "no_candidates".
func TestRetrievalGateEmptyRecallFallback(t *testing.T) {
	t.Parallel()

	g := newRetrievalGate(liveEmbedder(), warmLister(nil), 40, 30, nil)

	got, reason, _ := g.recall(context.Background(), uuid.Nil, sampleContent())
	if got != nil {
		t.Errorf("empty recall should return nil candidates, got %#v", got)
	}
	if reason != retrieveFallbackNoCandidates {
		t.Errorf("reason = %q, want %q", reason, retrieveFallbackNoCandidates)
	}
}

// TestRetrievalGateDisabledWhenEmbedderOff: an embedder with Enabled()==false
// (EMBEDDING_MODEL unset) turns the whole gate off — no recall, no metric.
func TestRetrievalGateDisabledWhenEmbedderOff(t *testing.T) {
	t.Parallel()

	emb := &fakeEmbedder{enabled: false}
	g := newRetrievalGate(emb, warmLister(cands("RAG")), 40, 30, nil)
	if g.enabled() {
		t.Fatal("gate must report disabled when embedder is off")
	}

	// recallCandidates on a pipeline with a disabled gate returns nil and
	// emits NO fallback metric (feature off is not a degradation).
	metrics := observability.NewMetrics()
	p := &ParsePipeline{retrieval: g, metrics: metrics}
	if got := p.recallCandidates(context.Background(), uuid.Nil, sampleContent()); got != nil {
		t.Errorf("disabled gate should yield nil candidates, got %#v", got)
	}
	if total := totalRetrieveFallback(metrics); total != 0 {
		t.Errorf("disabled gate must not emit fallback metric; total = %v", total)
	}
}

// TestRecallCandidatesEmitsFallbackMetric: a fallback (cold start here)
// bumps tagging_retrieve_fallback_total{reason}.
func TestRecallCandidatesEmitsFallbackMetric(t *testing.T) {
	t.Parallel()

	metrics := observability.NewMetrics()
	g := newRetrievalGate(liveEmbedder(), &fakeCandidateLister{count: 1}, 40, 30, nil)
	p := &ParsePipeline{retrieval: g, metrics: metrics}

	if got := p.recallCandidates(context.Background(), uuid.Nil, sampleContent()); got != nil {
		t.Errorf("cold start should yield nil candidates, got %#v", got)
	}
	if c := testutil.ToFloat64(metrics.TaggingRetrieveFallbackTotal.WithLabelValues(retrieveFallbackColdStart)); c != 1 {
		t.Errorf("cold_start fallback counter = %v, want 1", c)
	}
}

// TestRetrievalGateConceptCountCached: the cold-start count is cached so a
// burst of recalls within the TTL queries CountConcepts only once.
func TestRetrievalGateConceptCountCached(t *testing.T) {
	t.Parallel()

	lister := &countingLister{count: 50}
	g := newRetrievalGate(liveEmbedder(), lister, 40, 30, nil)

	for i := 0; i < 5; i++ {
		if _, ok := g.conceptCount(context.Background()); !ok {
			t.Fatalf("conceptCount not ok on call %d", i)
		}
	}
	if lister.countCalls != 1 {
		t.Errorf("CountConcepts calls = %d, want 1 (cached)", lister.countCalls)
	}
}

// TestRetrievalGateConceptCountSkipsOnError: a count error with no primed
// value tells the caller to skip the cold-start gate (fails open).
func TestRetrievalGateConceptCountSkipsOnError(t *testing.T) {
	t.Parallel()

	lister := &fakeCandidateLister{countErr: errors.New("pg down")}
	g := newRetrievalGate(liveEmbedder(), lister, 40, 30, nil)

	if _, ok := g.conceptCount(context.Background()); ok {
		t.Error("conceptCount should report not-ok on an un-primed count error")
	}

	// Because the cold-start check is skipped, recall proceeds to recall
	// candidates rather than blocking on the count error.
	g2 := newRetrievalGate(liveEmbedder(), &errCountWarmLister{nearest: cands("RAG")}, 40, 30, nil)
	got, reason, _ := g2.recall(context.Background(), uuid.Nil, sampleContent())
	if reason != "" {
		t.Errorf("count error should fail open to recall, got fallback %q", reason)
	}
	if len(got) != 1 {
		t.Errorf("candidates = %#v, want 1", got)
	}
}

// TestPipelineRunInjectsCandidatesAndRoutesNewWords is the Phase 7
// end-to-end (in-memory) test: with a live embedder + warm词表, Run must
//  1. inject the recalled candidate names into the analyzer request, and
//  2. route the analyzer's output so candidate hits survive and excess
//     new words are truncated to one before persistence + concept attach.
func TestPipelineRunInjectsCandidatesAndRoutesNewWords(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("aaaa1111-aaaa-1111-aaaa-111111111111")
	jobID := uuid.MustParse("bbbb2222-bbbb-2222-bbbb-222222222222")
	now := time.Now().UTC()

	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {ID: linkID, URL: "https://example.com/a/b", Status: model.LinkStatusPending, CreatedAt: now, UpdatedAt: now},
	})
	jobStore := newPipelineJobStore(map[uuid.UUID]*model.ParseJob{
		linkID: {ID: jobID, LinkID: linkID, Status: model.JobStatusPending, ExpectedMetadataRevision: 1, CreatedAt: now, UpdatedAt: now},
	})
	treeStore := newPipelineTreeStore(nil)

	// Candidate set recalled from the warm词表.
	candidates := cands("RAG", "WeKnora", "腾讯")

	var gotCandidates []string
	analyzer := pipelineAnalyzerFunc(func(_ context.Context, req analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
		gotCandidates = append([]string(nil), req.Candidates...)
		// Model picks two candidates + emits THREE new words; routing must
		// keep both selections + only the first new word.
		return analyzerpkg.AnalysisResult{
			Summary: "摘要",
			Tags:    []string{"RAG", "WeKnora", "新词甲", "新词乙", "新词丙"},
		}, nil
	})
	attacher := &recordingConceptAttacher{}

	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Jobs:             jobStore,
		Tags:             &pipelineFakeTagStore{tags: []string{"Go"}},
		Tree:             treeStore,
		Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
			return fetcher.Content{URL: "https://example.com/a/b", Title: "标题", Body: "正文", FetcherType: "basic"}, nil
		}),
		Analyzer:          analyzer,
		Metrics:           observability.NewMetrics(),
		Embedder:          liveEmbedder(),
		ConceptCandidates: warmLister(candidates),
		ConceptAttacher:   attacher,
		RetrieveTopK:      40,
		ColdStartMin:      30,
	})

	if err := pipeline.Run(context.Background(), linkID, jobID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// 1. Candidate injection: analyzer saw the recalled names.
	wantCands := []string{"RAG", "WeKnora", "腾讯"}
	if len(gotCandidates) != len(wantCands) {
		t.Fatalf("analyzer candidates = %#v, want %#v", gotCandidates, wantCands)
	}
	for i := range wantCands {
		if gotCandidates[i] != wantCands[i] {
			t.Errorf("candidate[%d] = %q, want %q", i, gotCandidates[i], wantCands[i])
		}
	}

	// 2. Routing: persisted tags = 2 selections + 1 new word.
	if len(linkStore.UpdateAnalysisCalls) != 1 {
		t.Fatalf("analysis updates = %d, want 1", len(linkStore.UpdateAnalysisCalls))
	}
	gotTags := linkStore.UpdateAnalysisCalls[0].Tags
	wantTags := []string{"RAG", "WeKnora", "新词甲"}
	if len(gotTags) != len(wantTags) {
		t.Fatalf("persisted tags = %#v, want %#v", gotTags, wantTags)
	}
	for i := range wantTags {
		if gotTags[i] != wantTags[i] {
			t.Errorf("tag[%d] = %q, want %q", i, gotTags[i], wantTags[i])
		}
	}

	// 3. Concept attach got the same routed set (the new word goes through
	//    the vector gate; selections hit the alias fast-table).
	if len(attacher.calls) != 1 {
		t.Fatalf("concept attach calls = %d, want 1", len(attacher.calls))
	}
	if got := attacher.calls[0].tags; len(got) != 3 || got[2] != "新词甲" {
		t.Errorf("attached tags = %#v, want [RAG WeKnora 新词甲]", got)
	}
}

// TestPipelineRunColdStartUsesFreeGeneration: a cold词表 must NOT inject
// candidates — the analyzer sees an empty Candidates slice (free-generation
// path) and all output tags pass through unrouted.
func TestPipelineRunColdStartUsesFreeGeneration(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("cccc3333-cccc-3333-cccc-333333333333")
	jobID := uuid.MustParse("dddd4444-dddd-4444-dddd-444444444444")
	now := time.Now().UTC()
	metrics := observability.NewMetrics()

	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {ID: linkID, URL: "https://example.com/c", Status: model.LinkStatusPending, CreatedAt: now, UpdatedAt: now},
	})
	jobStore := newPipelineJobStore(map[uuid.UUID]*model.ParseJob{
		linkID: {ID: jobID, LinkID: linkID, Status: model.JobStatusPending, CreatedAt: now, UpdatedAt: now},
	})
	treeStore := newPipelineTreeStore(nil)

	var sawCandidates bool
	analyzer := pipelineAnalyzerFunc(func(_ context.Context, req analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
		sawCandidates = len(req.Candidates) > 0
		return analyzerpkg.AnalysisResult{Summary: "摘要", Tags: []string{"x", "y", "z"}}, nil
	})

	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:            linkStore,
		ReadingCompleter: linkStore,
		SiteCompleter:    linkStore,
		Jobs:             jobStore,
		Tags:             &pipelineFakeTagStore{tags: []string{"Go"}},
		Tree:             treeStore,
		Fetcher: pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
			return fetcher.Content{URL: "https://example.com/c", Title: "标题", Body: "正文", FetcherType: "basic"}, nil
		}),
		Analyzer:          analyzer,
		Metrics:           metrics,
		Embedder:          liveEmbedder(),
		ConceptCandidates: &fakeCandidateLister{count: 3}, // below ColdStartMin
		RetrieveTopK:      40,
		ColdStartMin:      30,
	})

	if err := pipeline.Run(context.Background(), linkID, jobID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if sawCandidates {
		t.Error("cold start must not inject candidates into the analyzer")
	}
	// All three output tags pass through (free-generation, no routing).
	if got := linkStore.UpdateAnalysisCalls[0].Tags; len(got) != 3 {
		t.Errorf("persisted tags = %#v, want 3 (unrouted)", got)
	}
	if c := testutil.ToFloat64(metrics.TaggingRetrieveFallbackTotal.WithLabelValues(retrieveFallbackColdStart)); c != 1 {
		t.Errorf("cold_start fallback counter = %v, want 1", c)
	}
}

// --- extra stubs for caching / error-skip tests ----------------------

type countingLister struct {
	count      int
	countCalls int
}

func (c *countingLister) ListNearestConcepts(_ context.Context, _ []float32, _ string, _ int) ([]concept.CandidateConcept, error) {
	return nil, nil
}
func (c *countingLister) ListConceptsOfNearestLinks(_ context.Context, _ []float32, _ string, _, _ int, _ uuid.UUID) ([]concept.CandidateConcept, error) {
	return nil, nil
}
func (c *countingLister) CountConcepts(_ context.Context) (int, error) {
	c.countCalls++
	return c.count, nil
}

// errCountWarmLister errors on CountConcepts (so cold-start is skipped) but
// returns a warm recall, proving recall proceeds past a count error.
type errCountWarmLister struct {
	nearest []concept.CandidateConcept
}

func (e *errCountWarmLister) ListNearestConcepts(_ context.Context, _ []float32, _ string, _ int) ([]concept.CandidateConcept, error) {
	return e.nearest, nil
}
func (e *errCountWarmLister) ListConceptsOfNearestLinks(_ context.Context, _ []float32, _ string, _, _ int, _ uuid.UUID) ([]concept.CandidateConcept, error) {
	return nil, nil
}
func (e *errCountWarmLister) CountConcepts(_ context.Context) (int, error) {
	return 0, errors.New("count unavailable")
}

func totalRetrieveFallback(m *observability.Metrics) float64 {
	var total float64
	for _, reason := range []string{
		retrieveFallbackEmbeddingFailed,
		retrieveFallbackRetrieveFailed,
		retrieveFallbackColdStart,
		retrieveFallbackNoCandidates,
	} {
		total += testutil.ToFloat64(m.TaggingRetrieveFallbackTotal.WithLabelValues(reason))
	}
	return total
}

// guard: the fakes satisfy the production interfaces.
var (
	_ RetrievalEmbedder       = (*fakeEmbedder)(nil)
	_ concept.CandidateLister = (*fakeCandidateLister)(nil)
	_ concept.CandidateLister = (*countingLister)(nil)
	_ concept.CandidateLister = (*errCountWarmLister)(nil)
)
