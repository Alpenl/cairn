package service

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/concept"
)

// routeTags applies new-word routing to the analyzer output when candidates
// were injected (retrieve-then-select path). On the free-generation path
// (candidates empty) it returns the tags unchanged — there is no candidate
// set to route against, so the legacy "all tags pass through" behaviour
// holds. Dropped new words (the prompt allows at most one) are logged at
// WARN so an over-eager model is observable.
// routeTags 是 collectionFinalizer.routeTags 的薄委托，仅供 pipeline_route_test.go
// 直接驱动标签路由。实现在 pipeline_route.go 的 finalizer 方法上，改动请去那里。
func (p *ParsePipeline) routeTags(ctx context.Context, linkID uuid.UUID, tags []string, candidates []concept.CandidateConcept) []string {
	return p.collectionFinalizer().routeTags(ctx, linkID, tags, candidates)
}

func (f collectionFinalizer) routeTags(ctx context.Context, linkID uuid.UUID, tags []string, candidates []concept.CandidateConcept) []string {
	if len(candidates) == 0 {
		return tags
	}
	kept, dropped := routeRetrievedTags(tags, candidates)
	if len(dropped) > 0 {
		f.logWarn(ctx, "retrieve-then-select dropped excess new tags (only one new tag allowed per link)",
			"link_id", linkID.String(),
			"dropped", strings.Join(dropped, ","),
			"kept_count", len(kept),
		)
	}
	return kept
}

// routeRetrievedTags partitions the analyzer's output tags against the
// retrieve-then-select candidate set and enforces the "at most one new
// tag" contract:
//
//   - A tag whose normalised (lowercase+trim) form matches a candidate
//     name is a SELECTION. Selections always survive — they map to an
//     existing concept and the resolver's alias fast-table is guaranteed
//     to hit (the candidate came from the concept词表).
//   - Any other tag is a NEW WORD. The prompt allows at most one; if the
//     model emitted more, only the FIRST (in output order) is kept and the
//     rest are dropped. The vector gate (Phase 6) admits the surviving new
//     word — auto-merge, new concept, or review proposal.
//
// The returned slice preserves the analyzer's original tag order (minus
// dropped new words) so the link's tags column and concept attachment stay
// stable. dropped lists the new words that were truncated, for the WARN
// log the caller emits — empty when the model obeyed the one-new-tag rule.
//
// Candidates are matched by NAME only (not concept id): the model is
// instructed to copy candidate text verbatim, and the resolver re-resolves
// every surviving tag through the alias table anyway, so name membership is
// the sole routing signal here.
func routeRetrievedTags(tags []string, candidates []concept.CandidateConcept) (kept, dropped []string) {
	candidateSet := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		key := normalizeRouteTag(c.Name)
		if key == "" {
			continue
		}
		candidateSet[key] = struct{}{}
	}

	kept = make([]string, 0, len(tags))
	newWordKept := false
	for _, raw := range tags {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			continue
		}
		if _, isCandidate := candidateSet[normalizeRouteTag(tag)]; isCandidate {
			kept = append(kept, tag)
			continue
		}
		// New word: keep only the first, drop the rest.
		if newWordKept {
			dropped = append(dropped, tag)
			continue
		}
		newWordKept = true
		kept = append(kept, tag)
	}
	return kept, dropped
}

// normalizeRouteTag lowercases + trims a surface form for set membership.
// Matches the alias fast-table's own normalisation (GetConceptIDByAlias /
// UpsertAlias lowercase+trim) so a tag routed as a "selection" here is the
// same one the resolver will hit on the alias fast path.
func normalizeRouteTag(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
