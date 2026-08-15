// Package repository — concept canonicalization storage.
//
// PGXConceptRepository owns the three concept-layer tables introduced
// by migration e1f2a3b4c5d6: concept, concept_alias, link_concept.
// It is the single SQL boundary the concept.Resolver crosses; the
// resolver itself stays SQL-free so it can be unit-tested against a
// mock.
package repository

import (
	"webtag/internal/concept"
	"webtag/internal/database"
)

// Param types are owned by the concept package — they are domain
// shapes, not persistence shapes, and re-exporting them here would
// duplicate maintenance every time the resolver grows a field. Tests
// that exercise the repository in isolation construct
// concept.CreateConceptParams / concept.UpsertAliasParams /
// concept.AttachLinkConceptParams directly.

// PGXConceptRepository 是 concept.ConceptStore / AliasLookup 的 PG 实现，
// 负责 concept / concept_alias / link_concept 三张表的所有读写。
type PGXConceptRepository struct {
	db database.Querier
	tx txBeginner
}

// NewPGXConceptRepository 用给定的 Querier 构造 PGXConceptRepository。
func NewPGXConceptRepository(db database.Querier) *PGXConceptRepository {
	if db == nil {
		return &PGXConceptRepository{}
	}
	beginner, ok := db.(txBeginner)
	if !ok {
		panic("repository: concept Querier must support transactions")
	}
	return &PGXConceptRepository{db: db, tx: beginner}
}

// Compile-time guard: PGXConceptRepository satisfies the concept
// package's ConceptStore + AliasLookup interfaces so wiring breaks at
// build time if either side drifts.
var _ concept.ConceptStore = (*PGXConceptRepository)(nil)
var _ concept.AliasLookup = (*PGXConceptRepository)(nil)
var _ concept.MergeAdminConceptLookup = (*PGXConceptRepository)(nil)
var _ concept.CandidateLister = (*PGXConceptRepository)(nil)
