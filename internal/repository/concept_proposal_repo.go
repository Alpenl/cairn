// Package repository — concept merge proposal storage.
//
// PGXConceptProposalRepository owns concept_merge_proposal and the
// pg_trgm-powered candidate search added in migration f2a3b4c5d6e7.
// Kept separate from PGXConceptRepository so the resolver hot path
// (concept_repo.go) does not pull in candidate-finder SQL it never
// uses.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/concept"
	"webtag/internal/database"
)

// proposalColumns keeps llm_reason / decided_by / decided_at as raw
// nullable columns so the scan path can use sql.Null* and distinguish
// "pending, no decision yet" from "decided at the zero timestamp"
// without a sentinel-value dance.
const proposalColumns = `id, winner_id, loser_id, score, llm_reason,
	status, decided_by, created_at, decided_at`

// PGXConceptProposalRepository 是 concept_merge_proposal 表的 PG 实现，
// 承载提案列表/计数/审批合并（MergeConceptsByProposal）的事务路径。v3 起提案
// 的唯一来源是向量闸模糊带（concept.Resolver 写入），pg_trgm 候选扫描已退役。
type PGXConceptProposalRepository struct {
	db database.Querier
	tx txBeginner
}

// NewPGXConceptProposalRepository requires the supplied Querier to
// also implement txBeginner so the merge-approval path can run inside
// a transaction. Both *pgxpool.Pool and pgxmock satisfy it. Panicking
// at boot is the same fail-loud strategy PGXLinkRepository uses for
// its SubmitNew tx requirement.
//
// A nil db is permitted so a handful of validation-only tests (e.g.
// CreateProposal(nil id)) can construct the repo without a backing
// store. Production wiring always passes the real pgxpool.Pool.
func NewPGXConceptProposalRepository(db database.Querier) *PGXConceptProposalRepository {
	if db == nil {
		return &PGXConceptProposalRepository{}
	}
	begin, ok := db.(txBeginner)
	if !ok {
		panic("repository: Querier must implement txBeginner for MergeConceptsByProposal tx")
	}
	return &PGXConceptProposalRepository{db: db, tx: begin}
}

// maxLLMReasonChars caps the judge's free-text rationale. The review
// UI renders it inline (one row per proposal); 200 chars keeps the
// table compact and prevents an LLM that ignores the prompt's
// brevity instruction from stuffing the DB with paragraphs.
const maxLLMReasonChars = 200

// CreateProposal inserts the proposal and returns its ID. ON CONFLICT
// makes a second submission of the same pair a no-op that still
// returns the existing ID, so the judge does not need to pre-check.
//
// The proposal IDs are durable historical snapshots, so the table deliberately
// has no live-concept foreign keys. The materialized CTE replaces the creation
// guarantee those foreign keys used to provide: both rows must exist in the
// statement snapshot, and FOR KEY SHARE prevents either row from being deleted
// before the insert commits. A later concept deletion is intentionally allowed
// and leaves an orphaned proposal that can still be audited or rejected.
//
// Score is bounded to [0,1] — pg_trgm.similarity already lives in that
// range, but the param is a raw float32 so a future caller could
// hand-roll garbage; rejecting at the boundary is cheaper than letting
// it poison the review-UI sort order.
func (r *PGXConceptProposalRepository) CreateProposal(ctx context.Context, params concept.CreateMergeProposalParams) (uuid.UUID, error) {
	if params.WinnerID == uuid.Nil || params.LoserID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("create merge proposal: nil id")
	}
	if params.WinnerID == params.LoserID {
		return uuid.Nil, fmt.Errorf("create merge proposal: winner == loser")
	}
	if params.Score < 0 || params.Score > 1 || params.Score != params.Score { // NaN catch
		return uuid.Nil, fmt.Errorf("create merge proposal: score %f out of [0,1]", params.Score)
	}

	reason := params.LLMReason
	if len(reason) > maxLLMReasonChars {
		reason = reason[:maxLLMReasonChars]
	}
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}

	var id uuid.UUID
	err := r.db.QueryRow(ctx,
		`WITH live_concepts AS MATERIALIZED (
		   SELECT id FROM concept
		   WHERE id IN ($1, $2)
		   FOR KEY SHARE
		 )
		 INSERT INTO concept_merge_proposal (winner_id, loser_id, score, llm_reason)
		 SELECT $1, $2, $3, $4
		 WHERE (SELECT count(*) FROM live_concepts) = 2
		 ON CONFLICT (winner_id, loser_id) DO UPDATE SET winner_id = EXCLUDED.winner_id
		 RETURNING id`,
		params.WinnerID, params.LoserID, params.Score, reasonArg,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("create merge proposal: winner or loser no longer exists: %w", ErrConceptMergeOrphaned)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("create merge proposal: %w", err)
	}
	return id, nil
}

// ListPendingProposals returns the admin review queue. Newest first
// because high-throughput tagging tends to produce its most
// interesting pairs in batches that align with recent ingest activity.
func (r *PGXConceptProposalRepository) ListPendingProposals(ctx context.Context, limit, offset int) ([]concept.MergeProposal, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := r.db.Query(ctx,
		`SELECT `+proposalColumns+`
		 FROM concept_merge_proposal
		 WHERE status = 'pending'
		 ORDER BY created_at DESC
		 LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list pending proposals: %w", err)
	}
	defer rows.Close()

	var out []concept.MergeProposal
	for rows.Next() {
		p, err := scanProposalRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending proposals: %w", err)
	}
	return out, nil
}

// CountPendingProposals returns the full count of pending proposals.
// Backs the concept_merge_proposals_pending gauge — unlike
// ListPendingProposals it is not page-bounded, so a deep backlog is
// reported faithfully.
func (r *PGXConceptProposalRepository) CountPendingProposals(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM concept_merge_proposal WHERE status = 'pending'`,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending proposals: %w", err)
	}
	return n, nil
}

// GetProposal fetches one row. Returns (nil, nil) on not-found so the
// caller's NotFound mapping is unambiguous. A nil UUID input is a
// programmer error and surfaces as an explicit error rather than
// silently aliasing with not-found.
func (r *PGXConceptProposalRepository) GetProposal(ctx context.Context, id uuid.UUID) (*concept.MergeProposal, error) {
	if id == uuid.Nil {
		return nil, fmt.Errorf("get proposal: nil id")
	}
	row := r.db.QueryRow(ctx,
		`SELECT `+proposalColumns+` FROM concept_merge_proposal WHERE id = $1`,
		id,
	)
	p, err := scanProposalRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get proposal %s: %w", id, err)
	}
	return &p, nil
}

// MarkProposalDecided transitions pending → approved/rejected. The
// WHERE status='pending' guard makes the operation idempotent against
// double-clicks but rejects re-decisions on an already-terminal row;
// callers map that to "stale UI, refresh and try again".
func (r *PGXConceptProposalRepository) MarkProposalDecided(ctx context.Context, id uuid.UUID, status, decidedBy string) error {
	if id == uuid.Nil {
		return fmt.Errorf("mark proposal decided: nil id")
	}
	if status != concept.MergeProposalApproved && status != concept.MergeProposalRejected {
		return fmt.Errorf("mark proposal decided: status %q is not approved/rejected", status)
	}
	if strings.TrimSpace(decidedBy) == "" {
		return fmt.Errorf("mark proposal decided: decided_by is required")
	}

	tag, err := r.db.Exec(ctx,
		`UPDATE concept_merge_proposal
		 SET status = $2, decided_by = $3, decided_at = now()
		 WHERE id = $1 AND status = 'pending'`,
		id, status, decidedBy,
	)
	if err != nil {
		return fmt.Errorf("mark proposal decided: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mark proposal decided: proposal %s already decided or missing: %w", id, ErrConceptMergeAlreadyDecided)
	}
	return nil
}

// scanProposalRow is shared by GetProposal and the list-loop. The
// rowScanner interface is defined in link_scan.go in this package.
//
// llm_reason / decided_by / decided_at are nullable in the DB; the
// repo flattens them onto the non-pointer fields of MergeProposal so
// callers can use the zero value as "no value" without juggling
// pointers. DecidedAt stays at time.Time zero for pending rows.
func scanProposalRow(r rowScanner) (concept.MergeProposal, error) {
	var (
		p         concept.MergeProposal
		reason    sql.NullString
		decidedBy sql.NullString
		decidedAt sql.NullTime
	)
	if err := r.Scan(
		&p.ID, &p.WinnerID, &p.LoserID, &p.Score, &reason,
		&p.Status, &decidedBy, &p.CreatedAt, &decidedAt,
	); err != nil {
		return concept.MergeProposal{}, err
	}
	p.LLMReason = reason.String
	p.DecidedBy = decidedBy.String
	if decidedAt.Valid {
		p.DecidedAt = decidedAt.Time
	}
	return p, nil
}

// Compile-time guard that the concrete repo satisfies the concept
// package's store interface, so a future signature drift breaks build
// instead of failing at runtime.
var _ concept.MergeProposalStore = (*PGXConceptProposalRepository)(nil)
