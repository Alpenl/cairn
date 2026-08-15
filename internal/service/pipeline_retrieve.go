package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"webtag/internal/concept"
	"webtag/internal/fetcher"
	"webtag/internal/observability"
)

// retrieveTextRunes is the rune budget of the content text embedded for
// candidate recall: the title plus the first 2000 runes of the body. The
// retrieval embedding only needs enough signal to land near the right
// concepts — feeding the whole body would inflate the embedding call cost
// without improving top-K recall, and the analyzer still sees the full
// (separately truncated) body in its own prompt.
const retrieveTextRunes = 2000

// coldStartCountTTL bounds how often the cold-start switch hits the DB for
// the concept-table size. Mirrors the 60s pendingProposalsGaugeTTL: the词表
// grows at human-curation cadence, so a per-parse COUNT(*) would be pure
// waste. 60s means a freshly-warmed词表 starts feeding candidates within a
// minute of crossing CONCEPT_COLD_START_MIN.
const coldStartCountTTL = 60 * time.Second

// Budgets for the co-occurrence recall leg (see
// concept.CandidateLister.ListConceptsOfNearestLinks).
//
// neighbourLinkTopK is how many nearest links vote. Ten is enough for a
// stable frequency signal — below that a single oddly-tagged neighbour
// swings the ranking, above it the tail is dominated by links that are only
// loosely related.
//
// neighbourConceptLimit caps what those votes contribute to the prompt. The
// direct leg already supplies up to topK (40 by default) candidates, and the
// prompt asks the model to pick 2-4; padding the list past that point buys
// nothing and dilutes the instruction to reuse candidates verbatim.
const (
	neighbourLinkTopK     = 10
	neighbourConceptLimit = 15
)

// recallCandidates runs the retrieval gate for a content item and records
// the fallback metric/log when retrieval is skipped. Returns nil when the
// gate is disabled (feature off — no metric) OR when retrieval fell back
// (metric + WARN recorded here). A non-nil result is the candidate set to
// inject into the analyzer prompt.
func (p *ParsePipeline) recallCandidates(ctx context.Context, linkID uuid.UUID, content fetcher.Content) []concept.CandidateConcept {
	if !p.retrieval.enabled() {
		return nil
	}
	candidates, fallbackReason, partialErr := p.retrieval.recall(ctx, linkID, content)
	if fallbackReason != "" {
		p.recordRetrieveFallback(fallbackReason)
		// cold_start / no_candidates are expected steady-state events on a
		// thin or model-mismatched词表; embedding_failed / retrieve_failed
		// are operational degradations. Log both at WARN so the fallback is
		// always visible, with the reason for triage.
		p.logWarn(ctx, "retrieve-then-select fallback to free-generation tagging",
			"reason", fallbackReason,
			"link_id", linkID.String(),
			"url_host", observability.SafeURLHost(content.URL),
			"err", observability.SafeError(partialErr),
		)
		return nil
	}
	if partialErr != nil {
		// One recall leg failed but the other carried the run. No fallback
		// metric — candidates were produced, so this is not a fallback — but
		// it must still be visible: a leg that is broken every single time
		// would otherwise never surface as long as the other one answers.
		p.logWarn(ctx, "retrieve-then-select recall leg degraded; continuing with partial candidates",
			"link_id", linkID.String(),
			"url_host", observability.SafeURLHost(content.URL),
			"err", observability.SafeError(partialErr),
		)
	}
	return candidates
}

// recordRetrieveFallback bumps tagging_retrieve_fallback_total{reason}.
// nil-metrics safe so test pipelines without an observability registry do
// not panic.
func (p *ParsePipeline) recordRetrieveFallback(reason string) {
	if p.metrics == nil {
		return
	}
	p.metrics.TaggingRetrieveFallbackTotal.WithLabelValues(reason).Inc()
}

// candidateConceptNames projects a candidate set to the bare name list the
// analyzer prompt consumes. Returns nil for an empty/nil input so the
// analyzer's len(Candidates)==0 check keeps the free-generation prompt.
func candidateConceptNames(candidates []concept.CandidateConcept) []string {
	if len(candidates) == 0 {
		return nil
	}
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if name := strings.TrimSpace(c.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// RetrievalEmbedder is the subset of the embedding client the retrieval
// gate uses. Identical in shape to concept.Embedder; defined here so the
// service package depends on the interface, not internal/embedding, and so
// tests can stub the network. embedding.Client satisfies it.
type RetrievalEmbedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	Model() string
	Enabled() bool
}

// retrievalFallbackReason labels are the values of the
// tagging_retrieve_fallback_total counter; see observability.Metrics.
const (
	retrieveFallbackEmbeddingFailed = "embedding_failed"
	retrieveFallbackRetrieveFailed  = "retrieve_failed"
	retrieveFallbackColdStart       = "cold_start"
	retrieveFallbackNoCandidates    = "no_candidates"
)

// retrievalGate runs the v3 retrieve-then-select recall step that the
// pipeline executes BEFORE AI analysis. It is constructed once per
// pipeline and is safe for concurrent use (the cold-start cache is
// mutex-guarded, the embedder/lister are themselves concurrency-safe).
//
// enabled() is false when no embedding model is configured — the whole
// retrieval path is then skipped and the pipeline keeps its pre-v3
// free-generation behaviour with no fallback metric emitted (a disabled
// feature is not a degradation).
type retrievalGate struct {
	embedder     RetrievalEmbedder
	candidates   concept.CandidateLister
	topK         int
	coldStartMin int
	// metrics records the per-leg candidate contribution. Owned here rather
	// than by recallCandidates because the split only exists inside the
	// merge; nil-safe for test pipelines without a registry.
	metrics *observability.Metrics

	mu        sync.Mutex
	count     int
	countAt   time.Time
	countPrim bool
}

// newRetrievalGate wires the gate. Either dependency being nil, or the
// embedder reporting Enabled()==false, turns the gate off (enabled()
// returns false). topK / coldStartMin fall back to the Spec defaults when
// non-positive so a zero-value config does not produce a LIMIT 0 recall or
// a never-cold词表.
func newRetrievalGate(embedder RetrievalEmbedder, candidates concept.CandidateLister, topK, coldStartMin int, metrics *observability.Metrics) *retrievalGate {
	if topK <= 0 {
		topK = 40
	}
	if coldStartMin < 0 {
		coldStartMin = 0
	}
	return &retrievalGate{
		embedder:     embedder,
		candidates:   candidates,
		topK:         topK,
		coldStartMin: coldStartMin,
		metrics:      metrics,
	}
}

// recordRecallLegs reports how many candidates each leg contributed AFTER
// dedup, so the neighbour series measures what that leg added rather than
// what it returned.
func (g *retrievalGate) recordRecallLegs(direct, neighbour int) {
	if g.metrics == nil {
		return
	}
	if direct > 0 {
		g.metrics.TaggingRecallCandidatesTotal.WithLabelValues("direct").Add(float64(direct))
	}
	if neighbour > 0 {
		g.metrics.TaggingRecallCandidatesTotal.WithLabelValues("neighbour").Add(float64(neighbour))
	}
}

// enabled reports whether the retrieval path is live. Off when the gate or
// either dependency is nil, or the embedder has no model configured.
func (g *retrievalGate) enabled() bool {
	return g != nil &&
		g.embedder != nil && g.embedder.Enabled() &&
		g.candidates != nil
}

// recall returns the candidate concept set for the content, or a nil slice
// + a non-empty fallbackReason when the pipeline should drop to
// free-generation. Those two are mutually exclusive: a non-empty candidate
// slice always carries an empty reason, and vice versa. recall never fails
// the caller — every failure mode (cold start, embedding outage, recall
// query error, empty recall) degrades to a labelled fallback so the parse
// run always completes.
//
// partialErr carries the underlying cause whenever something actually
// failed — the embedding call, or either recall leg. It is NOT mutually
// exclusive with fallbackReason: on a total loss both are set (the reason
// labels the metric, partialErr explains it). The case it exists for is the
// other one, where a leg failed but the surviving leg still produced
// candidates: there is no fallback to report, and without partialErr a
// permanently broken leg would stay invisible for as long as the other one
// keeps answering.
//
// A nil partialErr with a non-empty fallbackReason is normal — cold start,
// empty content and an empty-but-healthy recall are all expected states with
// nothing to blame.
func (g *retrievalGate) recall(ctx context.Context, linkID uuid.UUID, content fetcher.Content) (cands []concept.CandidateConcept, fallbackReason string, partialErr error) {
	// Cold start: a thin词表 cannot recall useful candidates, so keep the
	// legacy free-generation prompt (which injects the full existing-tags
	// list) until the词表 crosses CONCEPT_COLD_START_MIN.
	if n, ok := g.conceptCount(ctx); ok && n < g.coldStartMin {
		return nil, retrieveFallbackColdStart, nil
	}

	text := retrievalText(content)
	if strings.TrimSpace(text) == "" {
		// No usable content text to embed — treat as a recall miss so the
		// run still completes via free-generation.
		return nil, retrieveFallbackNoCandidates, nil
	}

	vectors, err := g.embedder.Embed(ctx, []string{text})
	if err != nil || len(vectors) != 1 || len(vectors[0]) == 0 {
		// The embedding_failed label already tells the operator what class of
		// failure this was; carrying the cause up too is what makes the WARN
		// in recallCandidates actionable (timeout vs auth vs bad model name).
		// err is nil in the malformed-vector case, which the reason covers.
		return nil, retrieveFallbackEmbeddingFailed, err
	}

	direct, directErr := g.candidates.ListNearestConcepts(ctx, vectors[0], g.embedder.Model(), g.topK)
	neighbour, neighbourErr := g.candidates.ListConceptsOfNearestLinks(
		ctx, vectors[0], g.embedder.Model(), neighbourLinkTopK, neighbourConceptLimit, linkID,
	)
	// The legs are independent queries over different tables, so one
	// erroring must not discard the other's candidates. A surviving
	// candidate set is still a successful recall — but the error is
	// reported as partialErr so a permanently broken leg cannot hide
	// behind the other one's results.
	partialErr = errors.Join(directErr, neighbourErr)

	candidates, directKept, neighbourKept := mergeCandidates(direct, neighbour)
	g.recordRecallLegs(directKept, neighbourKept)
	if len(candidates) == 0 {
		// Zero candidates plus any error means the degradation cost us the
		// whole recall; label it as the operational event it is rather than
		// as the steady-state "词表 has nothing for this content" case.
		if partialErr != nil {
			return nil, retrieveFallbackRetrieveFailed, partialErr
		}
		return nil, retrieveFallbackNoCandidates, nil
	}
	return candidates, "", partialErr
}

// mergeCandidates unions the two recall legs, keeping direct hits first and
// appending only the neighbour concepts not already present.
//
// Order is load-bearing: the analyzer prompt tells the model to prefer the
// candidates it is given, and models weight earlier list items more heavily.
// Direct content↔concept matches are the higher-precision signal, so they
// lead; co-occurrence suggestions ride behind them as additional options
// rather than displacing them.
//
// Dedup is by ConceptID, not name — two concepts can legitimately render the
// same display name mid-merge (before a merge proposal is approved), and
// collapsing them by name would drop a distinct id the caller still needs to
// route the model's chosen tag back to a concept.
// The two counts are the post-dedup contribution of each leg, for the
// recall_candidates_total metric.
func mergeCandidates(direct, neighbour []concept.CandidateConcept) (merged []concept.CandidateConcept, directKept, neighbourKept int) {
	if len(neighbour) == 0 {
		// Nothing to dedup against. ListNearestConcepts selects by concept.id
		// so direct cannot contain internal duplicates — the merge loop below
		// would be an identity transform that still allocates a map and a
		// slice per parse. This is a steady state, not a rare one: a sparse
		// 词表 or a failed neighbour leg both land here.
		return direct, len(direct), 0
	}
	seen := make(map[uuid.UUID]struct{}, len(direct)+len(neighbour))
	out := make([]concept.CandidateConcept, 0, len(direct)+len(neighbour))
	for _, c := range direct {
		if _, dup := seen[c.ConceptID]; dup {
			continue
		}
		seen[c.ConceptID] = struct{}{}
		out = append(out, c)
		directKept++
	}
	for _, c := range neighbour {
		if _, dup := seen[c.ConceptID]; dup {
			continue
		}
		seen[c.ConceptID] = struct{}{}
		out = append(out, c)
		neighbourKept++
	}
	return out, directKept, neighbourKept
}

// conceptCount returns the cached concept-table size, refreshing it at
// most once per coldStartCountTTL. ok is false when the count has never
// been primed AND the refresh query failed — the caller then SKIPS the
// cold-start check (fails open to the retrieval path) rather than block
// tagging on a transient count error. A previously-primed value is served
// (stale-but-present) through a transient error.
func (g *retrievalGate) conceptCount(ctx context.Context) (int, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.countPrim && time.Since(g.countAt) < coldStartCountTTL {
		return g.count, true
	}

	n, err := g.candidates.CountConcepts(ctx)
	if err != nil {
		// Serve the last good value if we have one; otherwise tell the
		// caller to skip the cold-start gate this run.
		if g.countPrim {
			return g.count, true
		}
		return 0, false
	}
	g.count = n
	g.countAt = time.Now()
	g.countPrim = true
	return g.count, true
}

// retrievalText builds the string embedded for candidate recall: the
// content title on the first line, then the body truncated to
// retrieveTextRunes. Mirrors the analyzer's own truncation discipline so
// the embedded text is representative of what the model will see, while
// staying well under any embedding context window.
func retrievalText(content fetcher.Content) string {
	title := strings.TrimSpace(content.Title)
	body := strings.TrimSpace(truncateRunesRetrieve(content.Body, retrieveTextRunes))
	switch {
	case title != "" && body != "":
		return title + "\n" + body
	case title != "":
		return title
	default:
		return body
	}
}

// truncateRunesRetrieve returns the first n runes of s (whole string when
// shorter). It single-passes the string with `range`（按 rune 迭代，索引是字节
// 偏移），在第 n 个 rune 处用切片返回——避免此前「先 len([]rune) 计数、再
// []rune 截断」两次全量 rune 分配（热路径每链接一次）。n<=0 返回空串。
func truncateRunesRetrieve(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
