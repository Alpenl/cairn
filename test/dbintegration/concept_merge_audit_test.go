package dbintegration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/concept"
	"webtag/internal/migrate"
	"webtag/internal/repository"
)

func TestConceptMergeApprovalPreservesAuditAndOrphanLifecycle(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXConceptProposalRepository(pool)
	winnerID, loserID, otherID := uuid.New(), uuid.New(), uuid.New()
	targetID, rejectedID, orphanID := uuid.New(), uuid.New(), uuid.New()
	createdAt := time.Date(2026, 8, 14, 8, 30, 0, 0, time.UTC)

	if _, err := pool.Exec(t.Context(), `INSERT INTO concept
		(id,primary_name,display_name,use_count) VALUES
		($1,'winner','winner',2),($2,'loser','loser',3),($3,'other','other',1)`,
		winnerID, loserID, otherID); err != nil {
		t.Fatalf("seed concepts: %v", err)
	}
	linkID := createRF3BReadingLink(t, pool,
		"https://concept-audit-"+uuid.NewString()+".example.com/article", "raw-tag", "concept-audit.example.com")
	if _, err := pool.Exec(t.Context(), `INSERT INTO link_concept (link_id,concept_id,surface_tag)
		VALUES ($1,$2,'loser-surface')`, linkID, loserID); err != nil {
		t.Fatalf("seed loser link relation: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO concept_alias (alias,concept_id,source)
		VALUES ('loser-alias',$1,'test')`, loserID); err != nil {
		t.Fatalf("seed loser alias: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO concept_merge_proposal
		(id,winner_id,loser_id,score,llm_reason,created_at) VALUES
		($1,$4,$5,0.91,'target reason',$7),
		($2,$6,$5,0.72,'rejected reason',$7 + interval '1 second'),
		($3,$5,$6,0.68,'orphan reason',$7 + interval '2 seconds')`,
		targetID, rejectedID, orphanID, winnerID, loserID, otherID, createdAt); err != nil {
		t.Fatalf("seed proposals: %v", err)
	}
	if err := repo.MarkProposalDecided(t.Context(), rejectedID, concept.MergeProposalRejected, "reviewer-before-merge"); err != nil {
		t.Fatalf("reject related proposal: %v", err)
	}
	rejectedBefore, err := repo.GetProposal(t.Context(), rejectedID)
	if err != nil || rejectedBefore == nil {
		t.Fatalf("read rejected audit before merge = (%+v, %v)", rejectedBefore, err)
	}

	if err := repo.MergeConceptsByProposal(t.Context(), targetID, "approver@example.com"); err != nil {
		t.Fatalf("approve target proposal: %v", err)
	}

	target, err := repo.GetProposal(t.Context(), targetID)
	if err != nil || target == nil {
		t.Fatalf("read approved audit = (%+v, %v)", target, err)
	}
	if target.WinnerID != winnerID || target.LoserID != loserID || target.Score != float32(0.91) ||
		target.LLMReason != "target reason" || target.Status != concept.MergeProposalApproved ||
		target.DecidedBy != "approver@example.com" || target.DecidedAt.IsZero() || !target.CreatedAt.Equal(createdAt) {
		t.Fatalf("approved audit = %+v, want immutable IDs/content and complete decision", target)
	}

	rejectedAfter, err := repo.GetProposal(t.Context(), rejectedID)
	if err != nil || rejectedAfter == nil {
		t.Fatalf("read rejected audit after merge = (%+v, %v)", rejectedAfter, err)
	}
	if *rejectedAfter != *rejectedBefore {
		t.Fatalf("rejected audit changed across related merge: before=%+v after=%+v", rejectedBefore, rejectedAfter)
	}

	var loserCount, winnerEdges, winnerAliases int
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM concept WHERE id=$1),
		(SELECT count(*) FROM link_concept WHERE link_id=$2 AND concept_id=$3),
		(SELECT count(*) FROM concept_alias WHERE alias='loser-alias' AND concept_id=$3)`,
		loserID, linkID, winnerID).Scan(&loserCount, &winnerEdges, &winnerAliases); err != nil {
		t.Fatalf("read merged relations: %v", err)
	}
	if loserCount != 0 || winnerEdges != 1 || winnerAliases != 1 {
		t.Fatalf("merged loser/edge/alias counts = %d/%d/%d, want 0/1/1", loserCount, winnerEdges, winnerAliases)
	}

	pending, err := repo.ListPendingProposals(t.Context(), 50, 0)
	if err != nil {
		t.Fatalf("list orphaned pending proposals: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != orphanID || pending[0].WinnerID != loserID || pending[0].LoserID != otherID {
		t.Fatalf("pending proposals = %+v, want durable orphan %s", pending, orphanID)
	}
	if err := repo.MergeConceptsByProposal(t.Context(), orphanID, "orphan-approver"); !errors.Is(err, repository.ErrConceptMergeOrphaned) {
		t.Fatalf("approve orphan error = %v, want ErrConceptMergeOrphaned", err)
	}
	if err := repo.MarkProposalDecided(t.Context(), orphanID, concept.MergeProposalRejected, "orphan-reviewer"); err != nil {
		t.Fatalf("reject orphaned proposal: %v", err)
	}
	orphanAudit, err := repo.GetProposal(t.Context(), orphanID)
	if err != nil || orphanAudit == nil || orphanAudit.Status != concept.MergeProposalRejected ||
		orphanAudit.DecidedBy != "orphan-reviewer" || orphanAudit.DecidedAt.IsZero() ||
		orphanAudit.WinnerID != loserID || orphanAudit.LoserID != otherID {
		t.Fatalf("orphan terminal audit = (%+v, %v), want complete rejection", orphanAudit, err)
	}
}

func TestCreateConceptMergeProposalRequiresLiveConceptsDB(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXConceptProposalRepository(pool)
	winnerID, missingLoserID := uuid.New(), uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO concept (id,primary_name) VALUES ($1,'winner')`, winnerID); err != nil {
		t.Fatalf("seed winner: %v", err)
	}

	_, err := repo.CreateProposal(t.Context(), concept.CreateMergeProposalParams{
		WinnerID: winnerID,
		LoserID:  missingLoserID,
		Score:    0.8,
	})
	if !errors.Is(err, repository.ErrConceptMergeOrphaned) {
		t.Fatalf("CreateProposal() error = %v, want ErrConceptMergeOrphaned", err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM concept_merge_proposal`).Scan(&count); err != nil {
		t.Fatalf("count invalid proposals: %v", err)
	}
	if count != 0 {
		t.Fatalf("invalid proposal count = %d, want 0", count)
	}
}

func TestConcurrentConceptMergeApprovalHasOneDurableWinner(t *testing.T) {
	pool := StartPostgres(t)
	winnerID, loserID, proposalID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO concept (id,primary_name)
		VALUES ($1,'winner'),($2,'loser')`, winnerID, loserID); err != nil {
		t.Fatalf("seed concurrent concepts: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO concept_merge_proposal
		(id,winner_id,loser_id,score,llm_reason) VALUES ($1,$2,$3,0.88,'concurrent approval')`,
		proposalID, winnerID, loserID); err != nil {
		t.Fatalf("seed concurrent proposal: %v", err)
	}

	type result struct {
		actor string
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, actor := range []string{"replica-a", "replica-b"} {
		wg.Add(1)
		go func(actor string) {
			defer wg.Done()
			<-start
			err := repository.NewPGXConceptProposalRepository(pool).
				MergeConceptsByProposal(context.Background(), proposalID, actor)
			results <- result{actor: actor, err: err}
		}(actor)
	}
	close(start)
	wg.Wait()
	close(results)

	succeeded, conflicted := 0, 0
	winnerActor := ""
	for got := range results {
		switch {
		case got.err == nil:
			succeeded++
			winnerActor = got.actor
		case errors.Is(got.err, repository.ErrConceptMergeAlreadyDecided):
			conflicted++
		default:
			t.Fatalf("concurrent approval by %s error = %v", got.actor, got.err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent approval success/conflict = %d/%d, want 1/1", succeeded, conflicted)
	}
	audit, err := repository.NewPGXConceptProposalRepository(pool).GetProposal(t.Context(), proposalID)
	if err != nil || audit == nil || audit.Status != concept.MergeProposalApproved ||
		audit.DecidedBy != winnerActor || audit.DecidedAt.IsZero() || audit.WinnerID != winnerID || audit.LoserID != loserID {
		t.Fatalf("concurrent winner audit = (%+v, %v), winner actor %q", audit, err, winnerActor)
	}
}

func TestConceptMergeAuditLossRollsBackWholeMergeDB(t *testing.T) {
	pool := StartPostgres(t)
	winnerID, loserID, proposalID := uuid.New(), uuid.New(), uuid.New()
	linkID := createRF3BReadingLink(t, pool,
		"https://concept-audit-rollback-"+uuid.NewString()+".example.com/article", "rollback-tag", "concept-audit.example.com")
	if _, err := pool.Exec(t.Context(), `INSERT INTO concept
		(id,primary_name,display_name,use_count) VALUES ($1,'winner','winner',4),($2,'loser','loser',7)`,
		winnerID, loserID); err != nil {
		t.Fatalf("seed rollback concepts: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO link_concept (link_id,concept_id,surface_tag)
		VALUES ($1,$2,'rollback-surface')`, linkID, loserID); err != nil {
		t.Fatalf("seed rollback link relation: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO concept_alias (alias,concept_id,source)
		VALUES ('rollback-alias',$1,'test')`, loserID); err != nil {
		t.Fatalf("seed rollback alias: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO concept_merge_proposal
		(id,winner_id,loser_id,score,llm_reason) VALUES ($1,$2,$3,0.93,'must rollback')`,
		proposalID, winnerID, loserID); err != nil {
		t.Fatalf("seed rollback proposal: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `ALTER TABLE concept_merge_proposal
		ADD CONSTRAINT concept_merge_proposal_winner_id_fkey FOREIGN KEY (winner_id) REFERENCES concept(id) ON DELETE CASCADE,
		ADD CONSTRAINT concept_merge_proposal_loser_id_fkey FOREIGN KEY (loser_id) REFERENCES concept(id) ON DELETE CASCADE`); err != nil {
		t.Fatalf("install legacy cascade fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE concept_merge_proposal
			DROP CONSTRAINT IF EXISTS concept_merge_proposal_winner_id_fkey,
			DROP CONSTRAINT IF EXISTS concept_merge_proposal_loser_id_fkey`)
	})

	err := repository.NewPGXConceptProposalRepository(pool).
		MergeConceptsByProposal(t.Context(), proposalID, "rollback-approver")
	if !errors.Is(err, repository.ErrConceptMergeAuditLost) {
		t.Fatalf("MergeConceptsByProposal() error = %v, want ErrConceptMergeAuditLost", err)
	}

	var conceptCount, loserEdges, loserAliases, pendingAudit int
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM concept WHERE id=ANY($1::uuid[])),
		(SELECT count(*) FROM link_concept WHERE link_id=$2 AND concept_id=$3),
		(SELECT count(*) FROM concept_alias WHERE alias='rollback-alias' AND concept_id=$3),
		(SELECT count(*) FROM concept_merge_proposal
		 WHERE id=$4 AND status='pending' AND decided_by IS NULL AND decided_at IS NULL)`,
		[]uuid.UUID{winnerID, loserID}, linkID, loserID, proposalID).
		Scan(&conceptCount, &loserEdges, &loserAliases, &pendingAudit); err != nil {
		t.Fatalf("read rolled-back state: %v", err)
	}
	if conceptCount != 2 || loserEdges != 1 || loserAliases != 1 || pendingAudit != 1 {
		t.Fatalf("rollback concepts/edges/aliases/audit = %d/%d/%d/%d, want 2/1/1/1",
			conceptCount, loserEdges, loserAliases, pendingAudit)
	}
}

func TestConceptMergeAuditUpgradePreservesLegacyRowsDB(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open isolated upgrade database: %v", err)
	}
	t.Cleanup(pool.Close)

	winnerID, loserID := uuid.New(), uuid.New()
	pendingID, rejectedID, incompleteID := uuid.New(), uuid.New(), uuid.New()
	createdAt := time.Date(2026, 8, 13, 7, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(t.Context(), `CREATE TABLE public.concept (
		id uuid PRIMARY KEY, primary_name text NOT NULL);
		CREATE TABLE public.concept_merge_proposal (
		id uuid PRIMARY KEY, winner_id uuid NOT NULL, loser_id uuid NOT NULL,
		score real NOT NULL, llm_reason text, status text NOT NULL DEFAULT 'pending',
		decided_by text, created_at timestamptz NOT NULL DEFAULT now(), decided_at timestamptz,
		CONSTRAINT concept_merge_proposal_winner_id_fkey FOREIGN KEY (winner_id) REFERENCES public.concept(id) ON DELETE CASCADE,
		CONSTRAINT concept_merge_proposal_loser_id_fkey FOREIGN KEY (loser_id) REFERENCES public.concept(id) ON DELETE CASCADE)`); err != nil {
		t.Fatalf("create legacy audit schema: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.concept (id,primary_name)
		VALUES ($1,'winner'),($2,'loser')`, winnerID, loserID); err != nil {
		t.Fatalf("seed legacy concepts: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.concept_merge_proposal
		(id,winner_id,loser_id,score,llm_reason,status,decided_by,created_at,decided_at) VALUES
		($3,$1,$2,0.80,'pending','pending','stale-actor',$6,$6),
		($4,$1,$2,0.81,'rejected','rejected','real-reviewer',$6,$6 + interval '1 minute'),
		($5,$1,$2,0.82,'incomplete','approved','   ',$6,NULL)`,
		winnerID, loserID, pendingID, rejectedID, incompleteID, createdAt); err != nil {
		t.Fatalf("seed legacy proposals: %v", err)
	}

	repairSQL := conceptMergeAuditRepairSQL(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin audit upgrade: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), repairSQL); err != nil {
		t.Fatalf("apply concept audit repair: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit concept audit repair: %v", err)
	}

	var rowCount, lifecycleFKs int
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM concept_merge_proposal),
		(SELECT count(*) FROM pg_catalog.pg_constraint
		 WHERE conrelid='public.concept_merge_proposal'::regclass
		   AND contype='f' AND confrelid='public.concept'::regclass)`).Scan(&rowCount, &lifecycleFKs); err != nil {
		t.Fatalf("inspect upgraded proposal ownership: %v", err)
	}
	if rowCount != 3 || lifecycleFKs != 0 {
		t.Fatalf("upgraded rows/FKs = %d/%d, want 3/0", rowCount, lifecycleFKs)
	}
	var pendingActor *string
	var pendingDecision *time.Time
	if err := pool.QueryRow(t.Context(), `SELECT decided_by,decided_at FROM concept_merge_proposal WHERE id=$1`, pendingID).
		Scan(&pendingActor, &pendingDecision); err != nil {
		t.Fatalf("read repaired pending audit: %v", err)
	}
	if pendingActor != nil || pendingDecision != nil {
		t.Fatalf("repaired pending actor/time = %v/%v, want nil/nil", pendingActor, pendingDecision)
	}
	var incompleteActor string
	var incompleteDecision time.Time
	if err := pool.QueryRow(t.Context(), `SELECT decided_by,decided_at FROM concept_merge_proposal WHERE id=$1`, incompleteID).
		Scan(&incompleteActor, &incompleteDecision); err != nil {
		t.Fatalf("read repaired terminal audit: %v", err)
	}
	if incompleteActor != "legacy-migration" || !incompleteDecision.Equal(createdAt) {
		t.Fatalf("repaired terminal actor/time = %q/%v, want legacy-migration/%v", incompleteActor, incompleteDecision, createdAt)
	}

	if _, err := pool.Exec(t.Context(), `DELETE FROM concept WHERE id=ANY($1::uuid[])`, []uuid.UUID{winnerID, loserID}); err != nil {
		t.Fatalf("delete live concepts after upgrade: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM concept_merge_proposal`).Scan(&rowCount); err != nil {
		t.Fatalf("count durable upgraded audits: %v", err)
	}
	if rowCount != 3 {
		t.Fatalf("audit rows after concept deletion = %d, want 3", rowCount)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE concept_merge_proposal
		SET status='approved',decided_by='   ',decided_at=now() WHERE id=$1`, pendingID); err == nil {
		t.Fatal("decision audit constraint accepted a blank terminal actor")
	}

	var rejectedActor, rejectedReason string
	var rejectedDecision time.Time
	if err := pool.QueryRow(t.Context(), `SELECT decided_by,llm_reason,decided_at
		FROM concept_merge_proposal WHERE id=$1`, rejectedID).
		Scan(&rejectedActor, &rejectedReason, &rejectedDecision); err != nil {
		t.Fatalf("read preserved rejected audit: %v", err)
	}
	if rejectedActor != "real-reviewer" || rejectedReason != "rejected" ||
		!rejectedDecision.Equal(createdAt.Add(time.Minute)) {
		t.Fatalf("rejected audit actor/reason/time = %q/%q/%v", rejectedActor, rejectedReason, rejectedDecision)
	}
}

func conceptMergeAuditRepairSQL(t *testing.T) string {
	t.Helper()
	for _, step := range migrate.Steps() {
		for _, statement := range step.SQL {
			lowered := strings.ToLower(statement)
			if strings.Contains(lowered, "drop constraint if exists concept_merge_proposal_loser_id_fkey") &&
				strings.Contains(lowered, "btrim(decided_by) <> ''") {
				return statement
			}
		}
	}
	t.Fatal("concept merge audit repair SQL not found in migration plan")
	return ""
}
