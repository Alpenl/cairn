package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"webtag/internal/concept"
	"webtag/internal/observability"
)

func names(candidates []concept.CandidateConcept) []string {
	out := make([]string, len(candidates))
	for i, c := range candidates {
		out[i] = c.Name
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRecallUnionsBothLegs is the point of the whole feature: the
// co-occurrence leg must ADD candidates the direct content↔concept leg
// cannot reach, not replace or reorder them. Direct hits lead because the
// prompt tells the model to prefer earlier candidates and they are the
// higher-precision signal.
func TestRecallUnionsBothLegs(t *testing.T) {
	t.Parallel()

	lister := &fakeCandidateLister{
		count:     100,
		nearest:   cands("RAG", "向量检索"),
		neighbour: cands("科技周刊", "开发工具"),
	}
	g := newRetrievalGate(liveEmbedder(), lister, 40, 30, nil)

	got, reason, partialErr := g.recall(context.Background(), uuid.Nil, sampleContent())
	if reason != "" || partialErr != nil {
		t.Fatalf("recall reason=%q partialErr=%v, want clean success", reason, partialErr)
	}
	want := []string{"RAG", "向量检索", "科技周刊", "开发工具"}
	if !equalStrings(names(got), want) {
		t.Fatalf("candidates = %v, want %v (direct first, then neighbour)", names(got), want)
	}
	if lister.neighbourCalls != 1 {
		t.Fatalf("neighbour leg called %d times, want 1", lister.neighbourCalls)
	}
}

// TestRecallDedupesByConceptIDAcrossLegs: both legs legitimately return the
// same concept when a neighbour link carries a concept that also sits near
// the content. Emitting it twice would waste prompt budget and read as two
// separate suggestions to the model.
func TestRecallDedupesByConceptIDAcrossLegs(t *testing.T) {
	t.Parallel()

	shared := concept.CandidateConcept{ConceptID: uuid.New(), Name: "RAG"}
	extra := concept.CandidateConcept{ConceptID: uuid.New(), Name: "科技周刊"}
	lister := &fakeCandidateLister{
		count:     100,
		nearest:   []concept.CandidateConcept{shared},
		neighbour: []concept.CandidateConcept{shared, extra},
	}
	g := newRetrievalGate(liveEmbedder(), lister, 40, 30, nil)

	got, reason, _ := g.recall(context.Background(), uuid.Nil, sampleContent())
	if reason != "" {
		t.Fatalf("recall reason = %q, want none", reason)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %v, want the shared concept collapsed to one entry", names(got))
	}
	if got[0].ConceptID != shared.ConceptID || got[1].ConceptID != extra.ConceptID {
		t.Fatalf("candidates = %#v, want [shared extra]", got)
	}
}

// TestRecallDedupeKeepsDistinctIDsWithSameName guards the choice to dedupe
// by ConceptID rather than name. Two concepts can render the same display
// name while a merge proposal is still pending; collapsing them by name
// would drop an id the router still needs to map a chosen tag back.
func TestRecallDedupeKeepsDistinctIDsWithSameName(t *testing.T) {
	t.Parallel()

	a := concept.CandidateConcept{ConceptID: uuid.New(), Name: "接口设计"}
	b := concept.CandidateConcept{ConceptID: uuid.New(), Name: "接口设计"}
	lister := &fakeCandidateLister{
		count:     100,
		nearest:   []concept.CandidateConcept{a},
		neighbour: []concept.CandidateConcept{b},
	}
	g := newRetrievalGate(liveEmbedder(), lister, 40, 30, nil)

	got, _, _ := g.recall(context.Background(), uuid.Nil, sampleContent())
	if len(got) != 2 {
		t.Fatalf("candidates = %#v, want both ids kept despite the identical name", got)
	}
}

// TestRecallExcludesTheLinkBeingParsed: on re-parse the link already has an
// embedding and its own link_concept rows, so it would be its own nearest
// neighbour and vote for the exact tag set being recomputed.
func TestRecallExcludesTheLinkBeingParsed(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	lister := &fakeCandidateLister{count: 100, nearest: cands("RAG")}
	g := newRetrievalGate(liveEmbedder(), lister, 40, 30, nil)

	if _, _, err := g.recall(context.Background(), linkID, sampleContent()); err != nil {
		t.Fatalf("recall partialErr = %v", err)
	}
	if lister.neighbourExclude != linkID {
		t.Fatalf("neighbour leg excluded %v, want the link being parsed (%v)", lister.neighbourExclude, linkID)
	}
}

// TestRecallSurvivesDirectLegFailure: the legs are independent queries. One
// failing must not throw away the other's candidates and drop the run to
// free-generation tagging.
func TestRecallSurvivesDirectLegFailure(t *testing.T) {
	t.Parallel()

	lister := &fakeCandidateLister{
		count:      100,
		nearestErr: errors.New("pg down"),
		neighbour:  cands("科技周刊"),
	}
	g := newRetrievalGate(liveEmbedder(), lister, 40, 30, nil)

	got, reason, partialErr := g.recall(context.Background(), uuid.Nil, sampleContent())
	if reason != "" {
		t.Fatalf("reason = %q, want no fallback — the neighbour leg produced candidates", reason)
	}
	if !equalStrings(names(got), []string{"科技周刊"}) {
		t.Fatalf("candidates = %v, want the surviving leg's results", names(got))
	}
	// The surviving leg must not hide the broken one.
	if partialErr == nil {
		t.Fatal("partialErr = nil; a failing leg must stay visible even when the other succeeds")
	}
}

// TestRecallSurvivesNeighbourLegFailure is the mirror case and the one that
// matters for rollout: the new leg failing must never regress the existing
// direct-recall behaviour.
func TestRecallSurvivesNeighbourLegFailure(t *testing.T) {
	t.Parallel()

	lister := &fakeCandidateLister{
		count:        100,
		nearest:      cands("RAG"),
		neighbourErr: errors.New("pg down"),
	}
	g := newRetrievalGate(liveEmbedder(), lister, 40, 30, nil)

	got, reason, partialErr := g.recall(context.Background(), uuid.Nil, sampleContent())
	if reason != "" {
		t.Fatalf("reason = %q, want no fallback — the direct leg still worked", reason)
	}
	if !equalStrings(names(got), []string{"RAG"}) {
		t.Fatalf("candidates = %v, want the direct leg's results", names(got))
	}
	if partialErr == nil {
		t.Fatal("partialErr = nil; the broken neighbour leg must stay visible")
	}
}

// TestRecallBothLegsFailingIsRetrieveFailed keeps a total loss labelled as
// the operational degradation it is. Reporting "no_candidates" would file a
// database outage under an expected steady-state event.
func TestRecallBothLegsFailingIsRetrieveFailed(t *testing.T) {
	t.Parallel()

	lister := &fakeCandidateLister{
		count:        100,
		nearestErr:   errors.New("pg down"),
		neighbourErr: errors.New("pg down"),
	}
	g := newRetrievalGate(liveEmbedder(), lister, 40, 30, nil)

	got, reason, partialErr := g.recall(context.Background(), uuid.Nil, sampleContent())
	if got != nil {
		t.Errorf("candidates = %#v, want nil", got)
	}
	if reason != retrieveFallbackRetrieveFailed {
		t.Errorf("reason = %q, want %q", reason, retrieveFallbackRetrieveFailed)
	}
	if partialErr == nil {
		t.Error("partialErr = nil, want the underlying failures")
	}
}

// TestRecallEmptyBothLegsIsNoCandidates: a warm词表 that simply has nothing
// near this content is NOT a degradation and must keep its steady-state
// label so the metric stays usable for alerting.
func TestRecallEmptyBothLegsIsNoCandidates(t *testing.T) {
	t.Parallel()

	g := newRetrievalGate(liveEmbedder(), &fakeCandidateLister{count: 100}, 40, 30, nil)

	got, reason, partialErr := g.recall(context.Background(), uuid.Nil, sampleContent())
	if got != nil {
		t.Errorf("candidates = %#v, want nil", got)
	}
	if reason != retrieveFallbackNoCandidates {
		t.Errorf("reason = %q, want %q", reason, retrieveFallbackNoCandidates)
	}
	if partialErr != nil {
		t.Errorf("partialErr = %v, want nil — nothing actually failed", partialErr)
	}
}

// TestRecallRecordsPerLegContribution: the co-occurrence leg's whole premise
// is recalling concepts the direct leg cannot reach, which is unfalsifiable
// without this split. The counts must be POST-dedup — a neighbour concept the
// direct leg already returned was not "added" by the neighbour leg, and
// counting it would make the leg look productive while contributing nothing.
func TestRecallRecordsPerLegContribution(t *testing.T) {
	t.Parallel()

	shared := concept.CandidateConcept{ConceptID: uuid.New(), Name: "RAG"}
	onlyDirect := concept.CandidateConcept{ConceptID: uuid.New(), Name: "向量检索"}
	onlyNeighbour := concept.CandidateConcept{ConceptID: uuid.New(), Name: "科技周刊"}

	metrics := observability.NewMetrics()
	lister := &fakeCandidateLister{
		count:     100,
		nearest:   []concept.CandidateConcept{shared, onlyDirect},
		neighbour: []concept.CandidateConcept{shared, onlyNeighbour},
	}
	g := newRetrievalGate(liveEmbedder(), lister, 40, 30, metrics)

	if _, reason, _ := g.recall(context.Background(), uuid.Nil, sampleContent()); reason != "" {
		t.Fatalf("recall reason = %q, want none", reason)
	}

	direct := testutil.ToFloat64(metrics.TaggingRecallCandidatesTotal.WithLabelValues("direct"))
	neighbour := testutil.ToFloat64(metrics.TaggingRecallCandidatesTotal.WithLabelValues("neighbour"))
	if direct != 2 {
		t.Errorf("direct leg contribution = %v, want 2", direct)
	}
	if neighbour != 1 {
		t.Errorf("neighbour leg contribution = %v, want 1 (the shared concept was already counted as direct)", neighbour)
	}
}

// TestRecallCandidatesKeepsPartialResultsWithoutFallbackMetric checks the
// pipeline-level wiring: a degraded-but-usable recall must still hand the
// candidates to the analyzer and must NOT bump the fallback counter, or the
// metric stops meaning "we fell back to free-generation".
func TestRecallCandidatesKeepsPartialResultsWithoutFallbackMetric(t *testing.T) {
	t.Parallel()

	metrics := observability.NewMetrics()
	lister := &fakeCandidateLister{
		count:        100,
		nearest:      cands("RAG"),
		neighbourErr: errors.New("pg down"),
	}
	g := newRetrievalGate(liveEmbedder(), lister, 40, 30, nil)
	p := &ParsePipeline{retrieval: g, metrics: metrics}

	got := p.recallCandidates(context.Background(), uuid.New(), sampleContent())
	if !equalStrings(names(got), []string{"RAG"}) {
		t.Fatalf("candidates = %v, want the surviving leg's results", names(got))
	}
	if total := totalRetrieveFallback(metrics); total != 0 {
		t.Fatalf("fallback metric = %v, want 0 — candidates were produced, this is not a fallback", total)
	}
}
