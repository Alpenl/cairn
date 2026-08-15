// Package concept — Resolver service.
//
// Resolver.Resolve maps an LLM-produced surface tag (e.g. "RAG",
// "检索增强生成", "Tencent") to a concept ID. The v3 resolution strategy
// is layered for cost and accuracy:
//
//  1. **Local alias** — a single SELECT against concept_alias. Hits
//     the fast path for any surface form the resolver has seen
//     before. Backed by a process-local AliasCache so a repeated
//     surface form skips the round-trip entirely.
//
//  2. **Vector gate** — only on local miss, and only when an embedding
//     client is wired (EMBEDDING_MODEL set). The surface form is
//     embedded and compared by cosine similarity against the existing
//     concept vectors; gate.go decides between auto-merge, new-concept,
//     and human-review based on CONCEPT_AUTO_MERGE_THRESHOLD /
//     CONCEPT_NEW_THRESHOLD. See gate.go for the branch contract.
//
//  3. **Local-only concept** — when no embedding client is wired or
//     the gate's embedding call fails. Creates a concept row with a
//     single identity alias and no vector; the vector is back-filled
//     later by the Phase 8 backfill task once embeddings are available.
//
// The resolver writes through the ConceptStore interface (typically
// PGXConceptRepository) and never blocks the parse pipeline on an
// embedding outage — every embedding error degrades gracefully to the
// local-only branch with a logged warning.
package concept

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
)

// AliasLookup is the slice of ConceptStore the resolver actually
// consults on the fast path. Carving it out lets benchmarks /
// future caching layers wrap just the read side without dragging the
// write methods along.
type AliasLookup interface {
	GetConceptIDByAlias(ctx context.Context, alias string) (uuid.UUID, bool, error)
}

// CandidateLister is the read-only retrieval slice the parse pipeline
// consults before AI analysis. It recalls candidate concepts for a content
// embedding so the analyzer can pick from an existing vocabulary instead of
// free-generating, and reports the concept table size so the pipeline can
// apply the cold-start switch. PGXConceptRepository satisfies it; the narrow
// interface keeps the pipeline free of the resolver's write surface and lets
// tests stub the recall without a DB.
//
// The two recall methods answer different questions and are meant to be
// unioned, not chosen between:
//
//   - ListNearestConcepts measures content↔concept similarity directly. High
//     precision, but it can only surface a concept whose own embedding sits
//     near the text.
//   - ListConceptsOfNearestLinks goes content↔link↔concept: it finds the
//     nearest already-parsed links and returns the concepts they carry,
//     ranked by how many of those neighbours share each concept. This is a
//     co-occurrence signal, so it recalls concepts the text does not itself
//     resemble — "科技周刊" is the standard example: a weekly digest's body
//     is about AI and chips, never about being a weekly, so its own vector
//     never lands near the 周刊 concept, but every neighbouring digest link
//     already carries it.
type CandidateLister interface {
	ListNearestConcepts(ctx context.Context, embedding []float32, embeddingModel string, topK int) ([]CandidateConcept, error)
	ListConceptsOfNearestLinks(ctx context.Context, embedding []float32, embeddingModel string, linkTopK, conceptLimit int, excludeLinkID uuid.UUID) ([]CandidateConcept, error)
	CountConcepts(ctx context.Context) (int, error)
}

// ConceptStore is the full write surface the resolver needs. The
// repository package defines an identical interface and the concrete
// PGXConceptRepository satisfies both — Go duck-typing keeps the
// concept package free of any pgx imports.
//
// RecalculateDisplayName recomputes the "show this string in the UI"
// label for a concept by picking the most-used surface form among
// its link_concept rows. Called after every successful attach so the
// UI converges on majority spelling without an out-of-band job.
//
// FindNearestConcept powers the vector gate: it returns the cosine
// similarity (NOT the pgvector `<=>` distance) of the single closest
// concept in the installation vocabulary whose embedding_model matches
// the current model. found is false when no eligible neighbour exists
// (cold table / no vectors for the current model yet).
type ConceptStore interface {
	AliasLookup
	GetConceptIDByQID(ctx context.Context, qid string) (uuid.UUID, bool, error)
	CreateConcept(ctx context.Context, params CreateConceptParams) (uuid.UUID, error)
	UpsertAlias(ctx context.Context, params UpsertAliasParams) error
	IncrementUseCount(ctx context.Context, conceptID uuid.UUID) error
	AttachLinkConcept(ctx context.Context, params AttachLinkConceptParams) error
	RecalculateDisplayName(ctx context.Context, conceptID uuid.UUID) error
	FindNearestConcept(ctx context.Context, embedding []float32, embeddingModel string) (NearestConcept, error)

	// Batch counterparts used by ResolveAndAttachBatch to collapse a
	// per-tag round-trip storm (typically 4N statements for an N-tag
	// link) into a handful of bulk statements. Implementations MUST
	// lower-case aliases the same way GetConceptIDByAlias does so the
	// returned map keys round-trip with caller-side lookups.
	GetConceptIDsByAliases(ctx context.Context, aliases []string) (map[string]uuid.UUID, error)
	// AttachLinkConceptsBatch must acquire the Link row lock and verify that
	// expectedMetadataRevision and expectedTags still match before inserting.
	// Its boolean result is false when a metadata replacement won the race.
	AttachLinkConceptsBatch(ctx context.Context, linkID uuid.UUID, expectedMetadataRevision int64, expectedTags []string, items []AttachLinkConceptItem) (bool, error)
	IncrementUseCounts(ctx context.Context, conceptIDs []uuid.UUID) error
	RecalculateDisplayNames(ctx context.Context, conceptIDs []uuid.UUID) error
	// DetachLinkConceptsExcept removes every link_concept row for linkID
	// whose concept_id is NOT in keepConceptIDs, returning the concept IDs
	// that were actually detached. It gives re-parse "replace" semantics:
	// a link re-analysed with a new tag set must shed the concepts it no
	// longer carries instead of accumulating every concept it ever had.
	// keepConceptIDs is the freshly-attached set for this run; an empty
	// slice would detach everything, so callers MUST only invoke this
	// after a successful attach of a non-empty set.
	// DetachLinkConceptsExcept repeats the same Link metadata fence because it
	// may run after a successful attach and must not prune edges created by a
	// newer tuple after a concurrent Reader save.
	DetachLinkConceptsExcept(ctx context.Context, linkID uuid.UUID, expectedMetadataRevision int64, expectedTags []string, keepConceptIDs []uuid.UUID) ([]uuid.UUID, error)
}

// NearestConcept is the top-1 cosine neighbour FindNearestConcept
// returns. Similarity is already converted from the pgvector cosine
// distance (1 - distance) so callers compare it directly against the
// CONCEPT_* thresholds. Found is false when the eligible concept set
// (embedding_model = current) is empty.
type NearestConcept struct {
	ConceptID  uuid.UUID
	Similarity float64
	Found      bool
}

// CandidateConcept is one row of the retrieve-then-select candidate set
// ListNearestConcepts returns: the concept's id plus its UI-facing
// display name (COALESCE(display_name, primary_name)). The parse pipeline
// injects the names into the analyzer prompt and uses the id/name pair to
// route the model's chosen tags back to existing concepts.
type CandidateConcept struct {
	ConceptID uuid.UUID
	Name      string
}

// CreateConceptParams / UpsertAliasParams / AttachLinkConceptParams
// mirror the repository struct shapes. Duplicated here to keep the
// concept package free of repository imports — the resolver layer
// must not depend on persistence details. Production wiring uses
// PGXConceptRepository which has structurally-identical types so
// pass-through is zero-cost.
//
// CreateConcept atomically creates the canonical row and its lower-cased
// identity alias. If another writer already owns that alias, it returns the
// existing concept ID instead, so concurrent admission converges without
// leaving an alias-less concept behind.
//
// Embedding / EmbeddingModel are written when the vector gate admits a
// brand-new concept; both empty means "no vector yet" (local-only fallback
// path), which the backfill task later fills in.
type CreateConceptParams struct {
	PrimaryName    string
	WikidataQID    string
	Embedding      []float32
	EmbeddingModel string
}

type UpsertAliasParams struct {
	Alias      string
	ConceptID  uuid.UUID
	Lang       string
	Source     string
	Confidence float32
}

type AttachLinkConceptParams struct {
	LinkID     uuid.UUID
	ConceptID  uuid.UUID
	SurfaceTag string
}

// AttachLinkConceptItem is one row in an AttachLinkConceptsBatch call.
// LinkID is shared across the batch and passed alongside, so each item
// only carries the per-row concept_id + surface_tag pair.
type AttachLinkConceptItem struct {
	ConceptID  uuid.UUID
	SurfaceTag string
}

// Embedder is the subset of the embedding client the vector gate uses.
// Defining it here lets unit tests stub the network without importing
// internal/embedding, and keeps the concept package's dependency surface
// minimal. Embed returns one vector per input, in input order.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	Model() string
	Enabled() bool
}

// Resolver is safe for concurrent use as long as the underlying store
// and embedder are.
type Resolver struct {
	store     ConceptStore
	embedder  Embedder
	proposals ProposalCreator
	gateCfg   GateConfig
	logger    *slog.Logger
	// cache, when non-nil, short-circuits the concept_alias SELECT for
	// surface tags previously seen by this process. nil is a valid
	// "no caching" mode (tests, opt-out wiring) and every code path
	// guards on nil so the resolver continues to operate against the
	// store alone.
	cache *AliasCache
}

// ResolverOptions bundles dependencies. Logger may be nil — internal
// log calls degrade to no-ops in that case. Cache may be nil to
// disable the in-memory alias cache; production wiring (app/deps.go)
// always supplies one, shared with MergeAdminService so merges flush
// stale entries promptly.
//
// Embedder may be nil (or disabled) to turn the vector gate off
// entirely — the resolver then degrades every local miss straight to
// local-only concept creation. Gate carries the two cosine thresholds
// (defaults applied by GateConfig.withDefaults when zero).
type ResolverOptions struct {
	Store    ConceptStore
	Embedder Embedder
	// Proposals, when non-nil, receives the ambiguous-band merge
	// proposals the vector gate enqueues. nil disables proposal writes
	// (the band then degrades to a plain new concept + WARN). Production
	// wiring passes the PGXConceptProposalRepository.
	Proposals ProposalCreator
	Gate      GateConfig
	Logger    *slog.Logger
	Cache     *AliasCache
}

// NewResolver 用 opts 装配 Resolver；Embedder / Proposals / Logger / Cache 均可为 nil。
func NewResolver(opts ResolverOptions) *Resolver {
	return &Resolver{
		store:     opts.Store,
		embedder:  opts.Embedder,
		proposals: opts.Proposals,
		gateCfg:   opts.Gate.withDefaults(),
		logger:    opts.Logger,
		cache:     opts.Cache,
	}
}

// Resolve maps a single surface tag to a concept ID. The empty input
// returns (uuid.Nil, nil) so the caller can loop over a tag slice
// without filtering. All vector-gate failures degrade silently to the
// local-only branch — we never bubble them up because the parse
// pipeline must complete even when the embedding service is down.
func (r *Resolver) Resolve(ctx context.Context, surfaceTag string) (uuid.UUID, error) {
	surfaceTag = strings.TrimSpace(surfaceTag)
	if surfaceTag == "" {
		return uuid.Nil, nil
	}

	// Step 1: local alias fast path. Cache short-circuits the SELECT
	// when this process has resolved the same surface form before;
	// missed lookups fall through to the store and back-fill on hit.
	// AliasCache.Get is nil-safe so the cache==nil wiring (tests)
	// silently skips the fast path.
	if id, ok := r.cache.Get(surfaceTag); ok {
		return id, nil
	}
	if id, ok, err := r.store.GetConceptIDByAlias(ctx, surfaceTag); err != nil {
		return uuid.Nil, fmt.Errorf("alias lookup: %w", err)
	} else if ok {
		r.cache.Put(surfaceTag, id)
		return id, nil
	}

	// Step 2: vector gate. Best effort — when the embedder is wired and
	// enabled, gate decides auto-merge / new-concept / human-review by
	// cosine similarity. embedding 在此现算（单条路径）；失败则降级到 step 3
	// 的 vector-less 本地建词（Phase 8 backfill 后补向量）。
	if r.embedder != nil && r.embedder.Enabled() {
		vectors, err := r.embedder.Embed(ctx, []string{surfaceTag})
		if err != nil || len(vectors) != 1 || len(vectors[0]) == 0 {
			r.logf("warn", "vector gate embedding failed; creating local concept without vector",
				"surface", surfaceTag, "err", errString(err))
		} else if id, handled := r.resolveViaGate(ctx, surfaceTag, vectors[0]); handled {
			return id, nil
		}
	}

	// Step 3: local-only concept creation (no vector).
	return r.createLocalConcept(ctx, surfaceTag, nil, "")
}

// resolveMiss resolves a surface tag that ResolveAndAttachBatch already
// confirmed is an alias miss, using an embedding the batch precomputed in one
// upstream call. Mirrors Resolve's step 2-3 but skips the store alias lookup
// the batch already did (a cheap in-memory cache.Get is kept to catch a
// concurrent create that landed between the batch lookup and now). A nil
// embedding (batch embed disabled/failed for this tag) goes straight to the
// vector-less local create — same terminal behaviour as Resolve.
func (r *Resolver) resolveMiss(ctx context.Context, surfaceTag string, embedding []float32) (uuid.UUID, error) {
	if id, ok := r.cache.Get(surfaceTag); ok {
		return id, nil
	}
	if len(embedding) > 0 && r.embedder != nil && r.embedder.Enabled() {
		if id, handled := r.resolveViaGate(ctx, surfaceTag, embedding); handled {
			return id, nil
		}
	}
	return r.createLocalConcept(ctx, surfaceTag, nil, "")
}
