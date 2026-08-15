// Package concept owns the tag-canonicalization layer.
//
// A Concept is the "real thing" a tag refers to — e.g. retrieval-augmented
// generation. Different articles may surface it as "RAG" / "检索增强生成"
// / "Retrieval Augmented Generation"; the resolver maps every surface form
// to the same Concept so search and filtering treat them as one.
//
// v3 (Spec 检索式打标) canonicalizes through semantic embeddings rather
// than an external knowledge graph: a model-proposed surface form that
// misses the local alias fast-table is embedded and compared, by cosine
// similarity, against the existing concept vectors. The vector gate
// (gate.go) auto-merges near-identical forms, admits genuinely new ones
// as fresh concepts, and routes the ambiguous middle band to human
// review. The legacy Wikidata anchoring path has been retired; the
// wikidata_qid column is preserved for historical rows but no longer
// written.
package concept

import (
	"time"

	"github.com/google/uuid"
)

// Concept is the canonical row stored in the `concept` table. WikidataQID
// is empty for v3-era concepts; the field and column are retained so the
// read path (GetConcept / merge-admin hydration) can still surface the QID
// of legacy rows created before the Wikidata path was retired.
type Concept struct {
	ID          uuid.UUID
	PrimaryName string
	WikidataQID string
	UseCount    int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Alias records one surface form that resolves to a Concept. The
// resolver inserts an "identity" alias (alias == PrimaryName) whenever
// a Concept is created so the fast path is a single index lookup.
//
// Source values are kept in this package's vocabulary (see Source*
// constants) — the resolver and the audit UI both branch on them, so
// avoiding free-form strings keeps them in sync.
type Alias struct {
	Alias      string
	ConceptID  uuid.UUID
	Lang       string  // BCP-47 short tag ("en", "zh"), empty when unknown
	Source     string  // one of the Source* constants
	Confidence float32 // resolver's confidence at insertion; 1.0 for identity rows, cosine similarity for embedding-merge rows
	CreatedAt  time.Time
}

// Source values populated by the resolver and audit UI. Stable strings
// rather than enums because they end up in DB rows that outlive any
// single binary.
const (
	SourceIdentity = "identity" // alias == concept.primary_name, written when a concept is created
	// SourceEmbeddingMerge marks an alias the v3 vector gate auto-merged
	// into an existing concept because the new surface form's embedding
	// landed within CONCEPT_AUTO_MERGE_THRESHOLD cosine of that concept.
	// The alias confidence carries the observed cosine similarity.
	SourceEmbeddingMerge = "embedding-merge"
	SourceUserMerge      = "user-merge" // operator confirmed a merge in the review UI
)
