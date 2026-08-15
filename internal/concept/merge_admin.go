// Package concept — admin merge service.
//
// MergeAdminService is the seam between the admin HTTP handlers and
// the proposal repository. It owns:
//
//   - hydration: turning a MergeProposal (just IDs + score) into a
//     human-readable view by joining against the concept rows the
//     UI needs to label each side.
//   - approve: delegating to the transactional MergeApprover.
//   - reject: a thin wrapper around MarkProposalDecided so the
//     handler does not need to know the "rejected" string literal.
package concept

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// MergeAdminConceptLookup is the slice of the concept store the
// admin hydration path needs. Defining it inline keeps the admin
// service decoupled from the full ConceptStore that the resolver
// holds.
type MergeAdminConceptLookup interface {
	GetConcept(ctx context.Context, id uuid.UUID) (*Concept, error)
}

// MergeAdminView is the hydrated row the admin UI consumes. Score
// and reason come from the proposal; names + use_counts come from
// the concept rows.
type MergeAdminView struct {
	ID             uuid.UUID
	WinnerID       uuid.UUID
	WinnerName     string
	WinnerUseCount int
	LoserID        uuid.UUID
	LoserName      string
	LoserUseCount  int
	Score          float32
	LLMReason      string
	CreatedAtUnix  int64
}

// MergeAdminService 是合并提案管理 HTTP 层背后的服务：列出待审、批准（触发合并事务）、驳回。
type MergeAdminService struct {
	proposals  MergeProposalStore
	merger     MergeApprover
	concepts   MergeAdminConceptLookup
	aliasCache AliasInvalidator
}

// MergeAdminServiceOptions 收集 MergeAdminService 的依赖；AliasCache 可为 nil。
type MergeAdminServiceOptions struct {
	Proposals MergeProposalStore
	Merger    MergeApprover
	Concepts  MergeAdminConceptLookup
	// AliasCache, when non-nil, gets InvalidateAll() called after a
	// successful Approve so the resolver's in-memory alias cache stops
	// returning the now-deleted loser concept_id for any of the
	// merge's repointed aliases. nil is supported for tests / opt-out
	// wiring; the cache is best-effort optimisation and the merge
	// completes correctly without it.
	//
	// MUST be the *same* AliasCache instance the Resolver was built
	// with. Wiring two distinct caches silently breaks invalidation:
	// the merge will flush one map while the resolver keeps reading
	// from the other. The constraint is documented here rather than
	// enforced at the type level because the shared-instance pattern
	// is composed at app/deps.go and would require dragging Resolver
	// into this package's API surface to encode in types.
	AliasCache AliasInvalidator
}

// NewMergeAdminService 按 opts 装配 MergeAdminService。
func NewMergeAdminService(opts MergeAdminServiceOptions) *MergeAdminService {
	return &MergeAdminService{
		proposals:  opts.Proposals,
		merger:     opts.Merger,
		concepts:   opts.Concepts,
		aliasCache: opts.AliasCache,
	}
}

// ListPendingViews hydrates each pending proposal with its winner /
// loser primary names so the UI can render labels without a second
// round trip. A missing concept (race: judge wrote the proposal,
// admin merged a different proposal that ate the concept, list
// rendered with stale ID) collapses to an empty name; the UI still
// shows the row so the admin can reject the now-orphaned proposal.
func (s *MergeAdminService) ListPendingViews(ctx context.Context, limit, offset int) ([]MergeAdminView, error) {
	if s == nil || s.proposals == nil {
		return nil, fmt.Errorf("merge admin: not configured")
	}
	props, err := s.proposals.ListPendingProposals(ctx, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("merge admin list: %w", err)
	}
	out := make([]MergeAdminView, 0, len(props))
	for _, p := range props {
		v := MergeAdminView{
			ID:            p.ID,
			WinnerID:      p.WinnerID,
			LoserID:       p.LoserID,
			Score:         p.Score,
			LLMReason:     p.LLMReason,
			CreatedAtUnix: p.CreatedAt.Unix(),
		}
		if s.concepts != nil {
			if w, err := s.concepts.GetConcept(ctx, p.WinnerID); err == nil && w != nil {
				v.WinnerName = w.PrimaryName
				v.WinnerUseCount = w.UseCount
			}
			if l, err := s.concepts.GetConcept(ctx, p.LoserID); err == nil && l != nil {
				v.LoserName = l.PrimaryName
				v.LoserUseCount = l.UseCount
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// GetAudit returns the proposal snapshot by its stable ID without hydrating
// either side from the live concept table. Approved and rejected history
// therefore remains queryable after a loser has been merged away.
func (s *MergeAdminService) GetAudit(ctx context.Context, proposalID uuid.UUID) (*MergeProposal, error) {
	if s == nil || s.proposals == nil {
		return nil, fmt.Errorf("merge admin: not configured")
	}
	proposal, err := s.proposals.GetProposal(ctx, proposalID)
	if err != nil {
		return nil, fmt.Errorf("merge admin audit: %w", err)
	}
	return proposal, nil
}

// Approve runs the transactional merge. On success the resolver's
// alias cache is flushed because the merge tx may have repointed any
// of the loser's aliases (concept_alias.concept_id from loser→winner)
// or dropped them entirely via the FK cascade when the loser concept
// row is deleted. Targeted invalidation would require enumerating the
// affected aliases up-front; the full flush is cheap by comparison
// and merges run at admin cadence. The handler maps the
// repository.ErrConceptMergeAlreadyDecided sentinel to 409.
func (s *MergeAdminService) Approve(ctx context.Context, proposalID uuid.UUID, decidedBy string) error {
	if s == nil || s.merger == nil {
		return fmt.Errorf("merge admin: approver not configured")
	}
	if err := s.merger.MergeConceptsByProposal(ctx, proposalID, decidedBy); err != nil {
		return err
	}
	if s.aliasCache != nil {
		s.aliasCache.InvalidateAll()
	}
	return nil
}

// Reject just marks the proposal decided — no data movement.
func (s *MergeAdminService) Reject(ctx context.Context, proposalID uuid.UUID, decidedBy string) error {
	if s == nil || s.proposals == nil {
		return fmt.Errorf("merge admin: store not configured")
	}
	return s.proposals.MarkProposalDecided(ctx, proposalID, MergeProposalRejected, decidedBy)
}
