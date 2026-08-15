// Package concept — v3 vector gate.
//
// The vector gate is the new-tag admission control for retrieval-style
// tagging. When a model-proposed surface form misses the local alias
// fast-table, the gate embeds it and compares its embedding, by cosine
// similarity, against the existing concept vectors (top-1 nearest,
// restricted to the current embedding model). Three outcomes:
//
//   - similarity >= AutoMergeThreshold → the new surface is a spelling
//     variant of an existing concept. Write a concept_alias row
//     (source=embedding-merge, confidence=similarity) pointing at that
//     concept and reuse it. No new concept, no human review.
//
//   - similarity <= NewThreshold → genuinely new. Create a fresh
//     concept carrying the just-computed embedding + model so it is
//     immediately a comparison target for the next surface form.
//
//   - NewThreshold < similarity < AutoMergeThreshold (the ambiguous
//     band) → admit as a NEW concept (so tagging is not blocked) AND
//     enqueue a concept_merge_proposal (winner = the nearest existing
//     concept, loser = the freshly created concept, score = similarity,
//     llm_reason = "embedding-ambiguous: cos=<value>") for an operator
//     to confirm or reject later. The proposal is the v3 review queue's
//     sole source.
//
// All gate failures (embedding outage, nearest-neighbour query error,
// missing neighbour) fall through to a plain local-only create so the
// parse pipeline never stalls on the gate.
package concept

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// ProposalCreator is the slice of the proposal store the gate uses to
// enqueue ambiguous-band merge proposals. Kept narrow (one method) so
// the resolver does not depend on the full MergeProposalStore /
// MergeApprover surface that the admin path needs. nil disables
// proposal writes — the ambiguous band then degrades to a plain new
// concept with a logged warning (the surface is still tagged, it just
// skips the review queue).
type ProposalCreator interface {
	CreateProposal(ctx context.Context, params CreateMergeProposalParams) (uuid.UUID, error)
}

// GateConfig carries the two cosine thresholds that partition the
// similarity axis into auto-merge / ambiguous / new bands. Optional callers
// pass the values directly; the default Runtime no longer wires this gate.
// withDefaults applies stable values when a zero-value GateConfig is passed
// so custom callers and tests never divide the axis with a degenerate band.
type GateConfig struct {
	AutoMergeThreshold float64
	NewThreshold       float64
}

const (
	defaultAutoMergeThreshold = 0.92
	defaultNewThreshold       = 0.80
)

// withDefaults returns a copy with the Spec defaults substituted for any
// non-positive threshold. It does NOT re-validate the ordering
// invariant (NewThreshold < AutoMergeThreshold) — config.validateConfig
// owns that and fails fast at boot; here we only guard against the
// zero-value struct so direct NewResolver callers in tests get sane
// bands without spelling both knobs out.
func (g GateConfig) withDefaults() GateConfig {
	if g.AutoMergeThreshold <= 0 {
		g.AutoMergeThreshold = defaultAutoMergeThreshold
	}
	if g.NewThreshold <= 0 {
		g.NewThreshold = defaultNewThreshold
	}
	return g
}

// proposals is wired separately from the store so the gate can be
// constructed with the alias write surface but no proposal queue (the
// ambiguous band then logs + skips). Set via SetProposalCreator at
// wiring time.
//
// resolveViaGate takes surfaceTag + its precomputed embedding, finds the
// nearest existing concept for the current model, and dispatches to one of
// the three branches.
// Returns (id, true) when it owned the resolution (any branch), or
// (uuid.Nil, false) when it could not run the gate and the caller
// should fall through to a plain local-only create. The false path is
// the embedding/query outage degradation — never an error bubbled up.
//
// embedding 由调用方预先算好传入（单条 Resolve 现算、批量 ResolveAndAttachBatch
// 一次性批量算后分发），本函数不再自己调用 embedder.Embed——把 N 次每标签的
// embedding 网络往返收敛成调用方的一次批量调用。embedding 必须非空且非零长（调用
// 方负责校验，embedding 失败时走 vector-less 本地建词，不进本闸）。
func (r *Resolver) resolveViaGate(ctx context.Context, surfaceTag string, embedding []float32) (uuid.UUID, bool) {
	model := r.embedder.Model()

	nearest, err := r.store.FindNearestConcept(ctx, embedding, model)
	if err != nil {
		r.logf("warn", "vector gate nearest-neighbour query failed; creating local concept with vector",
			"surface", surfaceTag, "err", err.Error())
		// We still have a vector — create the concept WITH it so it
		// becomes a future comparison target even though we could not
		// compare it this time.
		id, createErr := r.createLocalConcept(ctx, surfaceTag, embedding, model)
		if createErr != nil {
			return uuid.Nil, false
		}
		return id, true
	}

	// No eligible neighbour (cold table, or no vectors for this model
	// yet): the surface is the first of its kind → brand-new concept
	// carrying its vector.
	if !nearest.Found {
		id, createErr := r.createLocalConcept(ctx, surfaceTag, embedding, model)
		if createErr != nil {
			return uuid.Nil, false
		}
		return id, true
	}

	switch {
	case nearest.Similarity >= r.gateCfg.AutoMergeThreshold:
		return r.gateAutoMerge(ctx, surfaceTag, nearest)
	case nearest.Similarity <= r.gateCfg.NewThreshold:
		return r.gateNewConcept(ctx, surfaceTag, embedding, model)
	default:
		return r.gateAmbiguous(ctx, surfaceTag, embedding, model, nearest)
	}
}

// gateAutoMerge reuses the nearest concept and records the new surface
// as an embedding-merge alias carrying the observed cosine similarity
// as its confidence. The alias write is fail-soft: if it loses a race
// (the same surface already maps elsewhere) we still return the nearest
// concept — the surface resolves correctly regardless of whether the
// alias row landed.
func (r *Resolver) gateAutoMerge(ctx context.Context, surfaceTag string, nearest NearestConcept) (uuid.UUID, bool) {
	if err := r.store.UpsertAlias(ctx, UpsertAliasParams{
		Alias:      surfaceTag,
		ConceptID:  nearest.ConceptID,
		Source:     SourceEmbeddingMerge,
		Confidence: float32(nearest.Similarity),
	}); err != nil {
		// A concurrent resolver may have assigned the same globally-unique
		// alias first. Re-read the authoritative mapping before deciding which
		// concept to cache and return.
		if existing, ok, lookupErr := r.store.GetConceptIDByAlias(ctx, surfaceTag); lookupErr == nil && ok {
			r.cache.Put(surfaceTag, existing)
			return existing, true
		}
		r.logf("warn", "embedding-merge alias write failed; reusing nearest concept",
			"surface", surfaceTag, "concept_id", nearest.ConceptID,
			"cos", nearest.Similarity, "err", err.Error())
	}
	r.cache.Put(surfaceTag, nearest.ConceptID)
	return nearest.ConceptID, true
}

// gateNewConcept admits a genuinely-new concept carrying its embedding.
func (r *Resolver) gateNewConcept(ctx context.Context, surfaceTag string, embedding []float32, model string) (uuid.UUID, bool) {
	id, err := r.createLocalConcept(ctx, surfaceTag, embedding, model)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// gateAmbiguous admits the surface as a NEW concept (carrying its
// vector) so tagging is not blocked, then enqueues a merge proposal
// pairing the new concept (loser) against the nearest existing concept
// (winner) for human review. The proposal write is best-effort: a
// failure (or no proposal queue wired) leaves the new concept standing
// and only loses the review hint, which a later identical surface would
// re-surface anyway.
func (r *Resolver) gateAmbiguous(ctx context.Context, surfaceTag string, embedding []float32, model string, nearest NearestConcept) (uuid.UUID, bool) {
	id, err := r.createLocalConcept(ctx, surfaceTag, embedding, model)
	if err != nil {
		return uuid.Nil, false
	}
	if r.proposals == nil {
		r.logf("warn", "vector gate ambiguous band but no proposal queue wired; new concept created without review hint",
			"surface", surfaceTag, "new_concept_id", id,
			"winner_id", nearest.ConceptID, "cos", nearest.Similarity)
		return id, true
	}
	if _, err := r.proposals.CreateProposal(ctx, CreateMergeProposalParams{
		WinnerID:  nearest.ConceptID,
		LoserID:   id,
		Score:     float32(nearest.Similarity),
		LLMReason: fmt.Sprintf("embedding-ambiguous: cos=%.4f", nearest.Similarity),
	}); err != nil {
		r.logf("warn", "vector gate ambiguous-band proposal write failed; new concept stands without review hint",
			"surface", surfaceTag, "new_concept_id", id,
			"winner_id", nearest.ConceptID, "cos", nearest.Similarity, "err", err.Error())
	}
	return id, true
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
