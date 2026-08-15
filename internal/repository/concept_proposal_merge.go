package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/concept"
)

// MergeConceptsByProposal is the transactional approve path. It:
//
//  1. Locks the proposal FOR UPDATE and confirms it is still pending
//     (catches double-clicks from two admin sessions racing the same
//     row).
//  2. Drops link_concept duplicates that would violate the
//     (link_id, concept_id) PK on UPDATE — i.e. links that already
//     have both winner and loser attached.
//  3. Re-points the remaining link_concept rows from loser to winner.
//  4. Drops concept_alias duplicates whose alias already exists on
//     the winner (the alias unique index would otherwise reject the
//     re-pointing).
//  5. Re-points the remaining aliases.
//  6. Folds loser.use_count into winner.use_count.
//  7. Marks the proposal approved.
//  8. Deletes the loser concept. Proposal winner/loser UUIDs are historical
//     snapshots and intentionally have no live-concept ownership FK.
//  9. Reads the approved proposal back and verifies its complete audit before
//     commit, failing closed on installations that still have the old CASCADE.
//
// Returns ErrConceptMergeAlreadyDecided when step 1 finds the
// proposal in a terminal state, so the handler can map it to a 409
// instead of a 500. All other errors roll back the transaction.
//
//nolint:gocyclo // one transaction deliberately keeps proposal lock, revision prelock, repoints, display-name recalc, decision, and loser deletion in visible execution order.
func (r *PGXConceptProposalRepository) MergeConceptsByProposal(ctx context.Context, proposalID uuid.UUID, decidedBy string) error {
	if proposalID == uuid.Nil {
		return fmt.Errorf("merge: nil proposal id")
	}
	if strings.TrimSpace(decidedBy) == "" {
		return fmt.Errorf("merge: decided_by is required")
	}

	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("merge: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prelockRepresentationWriteGateExclusive(ctx, tx); err != nil {
		return fmt.Errorf("merge: %w", err)
	}

	proposal, err := scanProposalRow(tx.QueryRow(ctx,
		`SELECT `+proposalColumns+`
		 FROM concept_merge_proposal
		 WHERE id = $1 AND status = 'pending'
		 FOR UPDATE`,
		proposalID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConceptMergeAlreadyDecided
	}
	if err != nil {
		return fmt.Errorf("merge: lock proposal: %w", err)
	}
	winner, loser := proposal.WinnerID, proposal.LoserID
	if winner == loser {
		// Defensive: the table CHECK rejects this at insert time, but
		// belt-and-braces protects against a manually-inserted row
		// (operator hot-patch, restored backup) bypassing the check.
		return fmt.Errorf("merge: winner == loser (data corruption?)")
	}
	var winnerExists, loserExists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM concept WHERE id=$1),
		       EXISTS (SELECT 1 FROM concept WHERE id=$2)`,
		winner, loser,
	).Scan(&winnerExists, &loserExists); err != nil {
		return fmt.Errorf("merge: verify live concepts: %w", err)
	}
	if !winnerExists || !loserExists {
		return fmt.Errorf("merge: winner or loser no longer exists: %w", ErrConceptMergeOrphaned)
	}
	if err := prelockLibraryGlobalRevisions(ctx, tx); err != nil {
		return fmt.Errorf("merge: prelock representation revisions: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM link_concept
		 WHERE concept_id = $1
		   AND link_id IN (SELECT link_id FROM link_concept WHERE concept_id = $2)`,
		loser, winner,
	); err != nil {
		return fmt.Errorf("merge: dedupe link_concept: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE link_concept SET concept_id = $1 WHERE concept_id = $2`,
		winner, loser,
	); err != nil {
		return fmt.Errorf("merge: repoint link_concept: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM concept_alias
		 WHERE concept_id = $1
		   AND alias IN (SELECT alias FROM concept_alias WHERE concept_id = $2)`,
		loser, winner,
	); err != nil {
		return fmt.Errorf("merge: dedupe concept_alias: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE concept_alias SET concept_id = $1 WHERE concept_id = $2`,
		winner, loser,
	); err != nil {
		return fmt.Errorf("merge: repoint concept_alias: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE concept
		 SET use_count = use_count + COALESCE((SELECT use_count FROM concept WHERE id = $2), 0),
		     updated_at = now()
		 WHERE id = $1`,
		winner, loser,
	); err != nil {
		return fmt.Errorf("merge: fold use_count: %w", err)
	}

	// Recalculate winner.display_name inside the tx so the next list
	// query reflects the merged surface frequencies atomically with
	// the link_concept re-pointing. Without this the UI would keep
	// showing the pre-merge display until some unrelated AttachLinkConcept
	// fires the next recalc.
	if _, err := tx.Exec(ctx,
		`UPDATE concept
		 SET display_name = (
		   SELECT surface_tag FROM link_concept
		   WHERE concept_id = $1
		   GROUP BY surface_tag
		   ORDER BY count(*) DESC, surface_tag ASC
		   LIMIT 1
		 )
		 WHERE id = $1`,
		winner,
	); err != nil {
		return fmt.Errorf("merge: recalc display_name: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE concept_merge_proposal
		 SET status = 'approved', decided_by = $2, decided_at = now()
		 WHERE id = $1 AND status = 'pending'`,
		proposalID, decidedBy,
	)
	if err != nil {
		return fmt.Errorf("merge: mark proposal approved: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrConceptMergeAlreadyDecided
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM concept WHERE id = $1`,
		loser,
	); err != nil {
		return fmt.Errorf("merge: delete loser concept: %w", err)
	}

	// The proposal is an immutable audit snapshot, not a child of either live
	// concept. Read it back after deleting the loser so an old ON DELETE CASCADE
	// schema or an incomplete terminal write aborts the entire merge.
	persisted, err := scanProposalRow(tx.QueryRow(ctx,
		`SELECT `+proposalColumns+` FROM concept_merge_proposal WHERE id = $1`,
		proposalID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("merge: proposal audit disappeared after loser deletion: %w", ErrConceptMergeAuditLost)
	}
	if err != nil {
		return fmt.Errorf("merge: verify proposal audit: %w", err)
	}
	if !approvedProposalAuditMatches(proposal, persisted, decidedBy) {
		return fmt.Errorf("merge: proposal audit is incomplete after loser deletion: %w", ErrConceptMergeAuditLost)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("merge: commit: %w", err)
	}
	return nil
}

// ErrConceptMergeAlreadyDecided is returned by MergeConceptsByProposal
// when the target proposal is already in a terminal status (approved
// or rejected). The handler maps it to 409 Conflict so an admin
// double-click does not surface as 500.
var ErrConceptMergeAlreadyDecided = errors.New("concept merge proposal already decided")

// ErrConceptMergeOrphaned is returned when a pending proposal still exists but
// one of its snapshotted live concepts has already been removed. It remains
// listable/rejectable, but approval cannot fabricate a merge.
var ErrConceptMergeOrphaned = errors.New("concept merge proposal references a missing concept")

// ErrConceptMergeAuditLost aborts an approval when deleting the loser removes
// or corrupts the proposal snapshot. This makes the dangerous legacy CASCADE
// schema fail closed until its forward migration has run.
var ErrConceptMergeAuditLost = errors.New("concept merge proposal audit was not preserved")

func approvedProposalAuditMatches(before, after concept.MergeProposal, decidedBy string) bool {
	return after.ID == before.ID &&
		after.WinnerID == before.WinnerID &&
		after.LoserID == before.LoserID &&
		after.Score == before.Score &&
		after.LLMReason == before.LLMReason &&
		after.CreatedAt.Equal(before.CreatedAt) &&
		after.Status == concept.MergeProposalApproved &&
		after.DecidedBy == decidedBy &&
		!after.DecidedAt.IsZero()
}

// Compile-time guard for the additional interface MergeConceptsByProposal
// satisfies (defined in concept package below P3-C wiring).
var _ concept.MergeApprover = (*PGXConceptProposalRepository)(nil)
