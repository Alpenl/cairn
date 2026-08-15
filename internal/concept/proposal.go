// Package concept — synonym-cleanup proposal types.
//
// MergeProposal represents one queued "these two concepts are the same
// thing under different surface forms" suggestion. In v3 the queue's
// sole producer is the vector gate's ambiguous band (gate.go): a new
// surface whose embedding lands between CONCEPT_NEW_THRESHOLD and
// CONCEPT_AUTO_MERGE_THRESHOLD of an existing concept. The admin review
// UI drains it by approving or rejecting each row. Approval triggers the
// merge API which re-points concept_alias and link_concept rows from
// loser_id to winner_id under one transaction.
//
// The legacy pg_trgm + LLM judge producer has been retired; the
// proposal types remain because the review queue and merge transaction
// are unchanged.
//
// The types live in the concept package rather than the repository so
// the gate / admin service can reason about them without importing pgx.
package concept

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// MergeProposalStatus values are stored verbatim in the DB. The
// migration enforces the closed set via CHECK constraint; keeping the
// constants here means the judge and UI cannot drift from the DB.
const (
	MergeProposalPending  = "pending"
	MergeProposalApproved = "approved"
	MergeProposalRejected = "rejected"
)

// MergeProposal is one row of concept_merge_proposal. In v3 the winner
// is the nearest existing concept the vector gate compared against and
// the loser is the freshly-created ambiguous-band concept.
type MergeProposal struct {
	ID        uuid.UUID
	WinnerID  uuid.UUID
	LoserID   uuid.UUID
	Score     float32 // cosine similarity that surfaced the pair (0..1)
	LLMReason string  // gate rationale, e.g. "embedding-ambiguous: cos=0.8612"
	Status    string  // one of MergeProposal* constants
	DecidedBy string  // operator identifier or "system" — empty until decided
	CreatedAt time.Time
	DecidedAt time.Time // zero when status is pending
}

// CreateMergeProposalParams is the gate's write surface. Score is the
// cosine similarity that triggered the consideration; LLMReason is a
// short machine-generated explanation (kept under ~200 chars so the
// review UI can render it inline without truncation).
type CreateMergeProposalParams struct {
	WinnerID  uuid.UUID
	LoserID   uuid.UUID
	Score     float32
	LLMReason string
}

// MergeApprover is the transactional approve path the admin handler
// calls. Kept separate from MergeProposalStore so the judge service
// (which only writes proposals) cannot accidentally invoke the heavy
// merge transaction.
type MergeApprover interface {
	MergeConceptsByProposal(ctx context.Context, proposalID uuid.UUID, decidedBy string) error
}

// MergeProposalStore is the persistence surface the gate and review
// API consume. Defined here for the same reason as ConceptStore — the
// concept package stays SQL-free.
type MergeProposalStore interface {
	// CreateProposal inserts a row. Idempotent: a second call with
	// the same (winner_id, loser_id) returns the existing ID without
	// modification.
	CreateProposal(ctx context.Context, params CreateMergeProposalParams) (uuid.UUID, error)

	// ListPendingProposals returns the review queue, newest first.
	ListPendingProposals(ctx context.Context, limit, offset int) ([]MergeProposal, error)

	// CountPendingProposals returns the total number of pending merge
	// proposals — the backlog the concept_merge_proposals_pending gauge
	// surfaces so the v3 review queue cannot silently pile up. Separate
	// from ListPendingProposals because the list is page-bounded (<=200)
	// and would undercount a real backlog.
	CountPendingProposals(ctx context.Context) (int, error)

	// GetProposal fetches one row by ID. Returns nil, nil when the
	// row does not exist (no error — review UI distinguishes "gone"
	// from "DB error").
	GetProposal(ctx context.Context, id uuid.UUID) (*MergeProposal, error)

	// MarkProposalDecided flips status from pending to approved or
	// rejected and records who decided + when. Returns an error if
	// the row is already in a terminal state (admin tooling should
	// treat that as a stale UI and refresh).
	MarkProposalDecided(ctx context.Context, id uuid.UUID, status, decidedBy string) error
}
