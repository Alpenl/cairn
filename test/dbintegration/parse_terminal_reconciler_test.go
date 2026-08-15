package dbintegration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/errsafe"
	"webtag/internal/repository"
	"webtag/internal/worker"
)

type repoTerminalProjector struct {
	repo *repository.PGXLinkRepository

	mu            sync.Mutex
	failuresLeft  int
	projectionCnt int
	appliedCnt    int
}

func (p *repoTerminalProjector) RecordDiscard(ctx context.Context, linkID, jobID uuid.UUID, cause error) error {
	p.mu.Lock()
	p.projectionCnt++
	if p.failuresLeft > 0 {
		p.failuresLeft--
		p.mu.Unlock()
		return errors.New("injected product-state database failure")
	}
	p.mu.Unlock()

	message := errsafe.SafeMessage(fmt.Errorf("parse worker exhausted retries: %w", cause))
	err := p.repo.MarkParseFailed(ctx, linkID, jobID, message)
	if errors.Is(err, repository.ErrParseJobNotRunnable) {
		return nil
	}
	if err == nil {
		p.mu.Lock()
		p.appliedCnt++
		p.mu.Unlock()
	}
	return err
}

func (p *repoTerminalProjector) applied() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.appliedCnt
}

func (p *repoTerminalProjector) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.projectionCnt
}

func finalizeRiverParseJob(t *testing.T, pool *pgxpool.Pool, linkID, jobID uuid.UUID, state string) {
	t.Helper()
	tag, err := pool.Exec(t.Context(),
		`UPDATE river_job
		 SET state = $3::river_job_state, finalized_at = NOW()
		 WHERE kind = 'parse_link'
		   AND args->>'link_id' = $1
		   AND args->>'parse_job_id' = $2`,
		linkID.String(), jobID.String(), state,
	)
	if err != nil {
		t.Fatalf("finalize River parse job: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("finalized River rows = %d, want 1", tag.RowsAffected())
	}
}

func assertParseProductStatus(t *testing.T, pool *pgxpool.Pool, linkID, jobID uuid.UUID, want string) {
	t.Helper()
	var linkStatus, jobStatus string
	if err := pool.QueryRow(t.Context(), `SELECT status FROM links WHERE id = $1`, linkID).Scan(&linkStatus); err != nil {
		t.Fatalf("read link status: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT status FROM parse_jobs WHERE id = $1`, jobID).Scan(&jobStatus); err != nil {
		t.Fatalf("read parse job status: %v", err)
	}
	if linkStatus != want || jobStatus != want {
		t.Fatalf("link/job status = %q/%q, want %q/%q", linkStatus, jobStatus, want, want)
	}
}

func assertNoParseTerminalMismatch(t *testing.T, reconciler *worker.ParseTerminalReconciler) {
	t.Helper()
	result, err := reconciler.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("follow-up RunOnce() error = %v", err)
	}
	if result.Scanned != 0 || result.Reconciled != 0 || result.Failed != 0 {
		t.Fatalf("follow-up result = %+v, want no remaining mismatch", result)
	}
}

func markParseProductProcessing(t *testing.T, pool *pgxpool.Pool, linkID, jobID uuid.UUID) {
	t.Helper()
	repo := repository.NewPGXLinkRepository(pool)
	if err := repo.MarkParseProcessing(t.Context(), linkID, jobID); err != nil {
		t.Fatalf("mark parse product processing: %v", err)
	}
}

func setParseProductTerminal(t *testing.T, pool *pgxpool.Pool, linkID, jobID uuid.UUID, status string) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin terminal product-state tx: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), `UPDATE links SET status = $2 WHERE id = $1`, linkID, status); err != nil {
		t.Fatalf("set terminal link status: %v", err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE parse_jobs SET status = $2 WHERE id = $1`, jobID, status); err != nil {
		t.Fatalf("set terminal parse job status: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit terminal product-state tx: %v", err)
	}
}

func newParseTerminalReconciler(t *testing.T, pool *pgxpool.Pool, projector worker.ParseTerminalProjector) *worker.ParseTerminalReconciler {
	t.Helper()
	reconciler, err := worker.NewParseTerminalReconciler(worker.ParseTerminalReconcilerOptions{
		Pool:      pool,
		Projector: projector,
		Interval:  time.Hour,
		BatchSize: 1,
	})
	if err != nil {
		t.Fatalf("NewParseTerminalReconciler() error = %v", err)
	}
	return reconciler
}

func deleteRiverParseJob(t *testing.T, pool *pgxpool.Pool, linkID, jobID uuid.UUID) {
	t.Helper()
	tag, err := pool.Exec(t.Context(), `DELETE FROM river_job
		WHERE kind = 'parse_link'
		  AND args->>'link_id' = $1
		  AND args->>'parse_job_id' = $2`, linkID.String(), jobID.String())
	if err != nil {
		t.Fatalf("delete River parse job: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("deleted River parse rows = %d, want 1", tag.RowsAffected())
	}
}

func ageParseAttemptForMissingRecovery(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `UPDATE parse_jobs
		SET updated_at = NOW() - INTERVAL '8 hours'
		WHERE id = $1`, jobID); err != nil {
		t.Fatalf("age parse attempt: %v", err)
	}
}

func assertParseMissingError(t *testing.T, pool *pgxpool.Pool, linkID, jobID uuid.UUID) {
	t.Helper()
	var linkError, jobError string
	if err := pool.QueryRow(t.Context(), `SELECT l.error_msg, p.error_msg
		FROM links l JOIN parse_jobs p ON p.link_id = l.id
		WHERE l.id = $1 AND p.id = $2`, linkID, jobID).Scan(&linkError, &jobError); err != nil {
		t.Fatalf("read parse missing errors: %v", err)
	}
	for surface, value := range map[string]string{"link": linkError, "parse job": jobError} {
		if !strings.HasPrefix(value, "parse_job_missing:") {
			t.Fatalf("%s error_msg = %q, want stable parse_job_missing prefix", surface, value)
		}
	}
}

// This reproduces River JobRescuer's final update: it changes a running job to
// discarded directly and therefore never invokes the configured ErrorHandler.
func TestParseTerminalReconcilerProjectsRescuerDiscard(t *testing.T) {
	pool := StartPostgres(t)
	linkID, jobID := insertPendingLinkAndJob(t, pool, "https://example.com/reconcile-rescuer")
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := queue.Enqueue(t.Context(), linkID, jobID); err != nil {
		t.Fatalf("enqueue parse job: %v", err)
	}
	markParseProductProcessing(t, pool, linkID, jobID)
	finalizeRiverParseJob(t, pool, linkID, jobID, "discarded")

	projector := &repoTerminalProjector{repo: repository.NewPGXLinkRepository(pool)}
	reconciler := newParseTerminalReconciler(t, pool, projector)
	result, err := reconciler.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Scanned != 1 || result.Reconciled != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v, want scanned=1 reconciled=1 failed=0", result)
	}
	assertParseProductStatus(t, pool, linkID, jobID, "failed")
	assertNoParseTerminalMismatch(t, reconciler)
}

func TestParseTerminalReconcilerProjectsCompletedMismatch(t *testing.T) {
	pool := StartPostgres(t)
	linkID, jobID := insertPendingLinkAndJob(t, pool, "https://example.com/reconcile-completed")
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := queue.Enqueue(t.Context(), linkID, jobID); err != nil {
		t.Fatalf("enqueue parse job: %v", err)
	}
	markParseProductProcessing(t, pool, linkID, jobID)
	finalizeRiverParseJob(t, pool, linkID, jobID, "completed")

	projector := &repoTerminalProjector{repo: repository.NewPGXLinkRepository(pool)}
	reconciler := newParseTerminalReconciler(t, pool, projector)
	result, err := reconciler.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Scanned != 1 || result.Reconciled != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v, want scanned=1 reconciled=1 failed=0", result)
	}
	assertParseProductStatus(t, pool, linkID, jobID, "failed")
	assertNoParseTerminalMismatch(t, reconciler)
}

func TestParseTerminalReconcilerSkipsConsistentTerminalAttempts(t *testing.T) {
	pool := StartPostgres(t)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))

	for i, tc := range []struct {
		productStatus string
		riverState    string
	}{
		{productStatus: "done", riverState: "completed"},
		{productStatus: "failed", riverState: "discarded"},
	} {
		linkID, jobID := insertPendingLinkAndJob(t, pool, fmt.Sprintf("https://example.com/reconcile-consistent-%d", i))
		if err := queue.Enqueue(t.Context(), linkID, jobID); err != nil {
			t.Fatalf("enqueue parse job: %v", err)
		}
		setParseProductTerminal(t, pool, linkID, jobID, tc.productStatus)
		finalizeRiverParseJob(t, pool, linkID, jobID, tc.riverState)
	}

	projector := &repoTerminalProjector{repo: repository.NewPGXLinkRepository(pool)}
	result, err := newParseTerminalReconciler(t, pool, projector).RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Scanned != 0 || projector.count() != 0 {
		t.Fatalf("result/projections = %+v/%d, want no normal terminal work", result, projector.count())
	}
}

func TestParseTerminalReconcilerRetriesFailedProjection(t *testing.T) {
	pool := StartPostgres(t)
	linkID, jobID := insertPendingLinkAndJob(t, pool, "https://example.com/reconcile-retry")
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := queue.Enqueue(t.Context(), linkID, jobID); err != nil {
		t.Fatalf("enqueue parse job: %v", err)
	}
	markParseProductProcessing(t, pool, linkID, jobID)
	finalizeRiverParseJob(t, pool, linkID, jobID, "discarded")

	projector := &repoTerminalProjector{
		repo:         repository.NewPGXLinkRepository(pool),
		failuresLeft: 1,
	}
	reconciler := newParseTerminalReconciler(t, pool, projector)

	first, err := reconciler.RunOnce(t.Context())
	if err == nil {
		t.Fatal("first RunOnce() error = nil, want injected projection failure")
	}
	if first.Scanned != 1 || first.Reconciled != 0 || first.Failed != 1 {
		t.Fatalf("first result = %+v, want scanned=1 reconciled=0 failed=1", first)
	}
	assertParseProductStatus(t, pool, linkID, jobID, "processing")

	second, err := reconciler.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if second.Scanned != 1 || second.Reconciled != 1 || second.Failed != 0 {
		t.Fatalf("second result = %+v, want scanned=1 reconciled=1 failed=0", second)
	}
	assertParseProductStatus(t, pool, linkID, jobID, "failed")
	assertNoParseTerminalMismatch(t, reconciler)
	if got := projector.count(); got != 2 {
		t.Fatalf("projection count = %d, want 2", got)
	}
}

func TestParseTerminalReconcilerIsSafeAcrossReplicas(t *testing.T) {
	pool := StartPostgres(t)
	linkID, jobID := insertPendingLinkAndJob(t, pool, "https://example.com/reconcile-replicas")
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := queue.Enqueue(t.Context(), linkID, jobID); err != nil {
		t.Fatalf("enqueue parse job: %v", err)
	}
	markParseProductProcessing(t, pool, linkID, jobID)
	finalizeRiverParseJob(t, pool, linkID, jobID, "cancelled")

	projector := &repoTerminalProjector{repo: repository.NewPGXLinkRepository(pool)}
	reconcilers := []*worker.ParseTerminalReconciler{
		newParseTerminalReconciler(t, pool, projector),
		newParseTerminalReconciler(t, pool, projector),
	}
	errCh := make(chan error, len(reconcilers))
	var wg sync.WaitGroup
	for _, reconciler := range reconcilers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := reconciler.RunOnce(context.Background())
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Error(fmt.Errorf("replica reconciliation: %w", err))
		}
	}
	assertParseProductStatus(t, pool, linkID, jobID, "failed")
	assertNoParseTerminalMismatch(t, reconcilers[0])
}

func TestParseTerminalReconcilerRecoversMissingRiverRow(t *testing.T) {
	pool := StartPostgres(t)
	linkID, jobID := insertPendingLinkAndJob(t, pool, "https://example.com/reconcile-missing")
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := queue.Enqueue(t.Context(), linkID, jobID); err != nil {
		t.Fatalf("enqueue parse job: %v", err)
	}
	markParseProductProcessing(t, pool, linkID, jobID)
	ageParseAttemptForMissingRecovery(t, pool, jobID)
	deleteRiverParseJob(t, pool, linkID, jobID)

	projector := &repoTerminalProjector{repo: repository.NewPGXLinkRepository(pool)}
	reconciler := newParseTerminalReconciler(t, pool, projector)
	result, err := reconciler.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Scanned != 1 || result.Reconciled != 1 || result.Missing != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v, want one missing-row recovery", result)
	}
	if projector.applied() != 1 {
		t.Fatalf("effective projections = %d, want 1", projector.applied())
	}
	assertParseProductStatus(t, pool, linkID, jobID, "failed")
	assertParseMissingError(t, pool, linkID, jobID)
	assertNoParseTerminalMismatch(t, reconciler)
}

func TestParseTerminalReconcilerDoesNotFailAttemptWithActiveReplacement(t *testing.T) {
	pool := StartPostgres(t)
	linkID, jobID := insertPendingLinkAndJob(t, pool, "https://example.com/reconcile-active-replacement")
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := queue.Enqueue(t.Context(), linkID, jobID); err != nil {
		t.Fatalf("enqueue original parse job: %v", err)
	}
	markParseProductProcessing(t, pool, linkID, jobID)
	ageParseAttemptForMissingRecovery(t, pool, jobID)
	deleteRiverParseJob(t, pool, linkID, jobID)
	if err := queue.Enqueue(t.Context(), linkID, uuid.New()); err != nil {
		t.Fatalf("enqueue active replacement: %v", err)
	}

	projector := &repoTerminalProjector{repo: repository.NewPGXLinkRepository(pool)}
	result, err := newParseTerminalReconciler(t, pool, projector).RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Scanned != 0 || result.Reconciled != 0 || result.Missing != 0 || projector.count() != 0 {
		t.Fatalf("result/projections = %+v/%d, want active replacement no-op", result, projector.count())
	}
	assertParseProductStatus(t, pool, linkID, jobID, "processing")
}

func TestParseTerminalReconcilerDoesNotFailSupersededAttempt(t *testing.T) {
	pool := StartPostgres(t)
	linkID, oldJobID := insertPendingLinkAndJob(t, pool, "https://example.com/reconcile-newer-attempt")
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := queue.Enqueue(t.Context(), linkID, oldJobID); err != nil {
		t.Fatalf("enqueue original parse job: %v", err)
	}
	markParseProductProcessing(t, pool, linkID, oldJobID)
	ageParseAttemptForMissingRecovery(t, pool, oldJobID)
	deleteRiverParseJob(t, pool, linkID, oldJobID)

	var newJobID uuid.UUID
	if err := pool.QueryRow(t.Context(), `INSERT INTO parse_jobs (link_id, status)
		VALUES ($1, 'processing') RETURNING id`, linkID).Scan(&newJobID); err != nil {
		t.Fatalf("insert newer parse attempt: %v", err)
	}
	if err := queue.Enqueue(t.Context(), linkID, newJobID); err != nil {
		t.Fatalf("enqueue newer parse attempt: %v", err)
	}

	projector := &repoTerminalProjector{repo: repository.NewPGXLinkRepository(pool)}
	result, err := newParseTerminalReconciler(t, pool, projector).RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Scanned != 0 || result.Reconciled != 0 || result.Missing != 0 || projector.count() != 0 {
		t.Fatalf("result/projections = %+v/%d, want superseded attempt no-op", result, projector.count())
	}
	assertParseProductStatus(t, pool, linkID, oldJobID, "processing")
}

func TestParseTerminalReconcilerMissingRecoveryIsSafeAcrossReplicas(t *testing.T) {
	pool := StartPostgres(t)
	linkID, jobID := insertPendingLinkAndJob(t, pool, "https://example.com/reconcile-missing-replicas")
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := queue.Enqueue(t.Context(), linkID, jobID); err != nil {
		t.Fatalf("enqueue parse job: %v", err)
	}
	markParseProductProcessing(t, pool, linkID, jobID)
	ageParseAttemptForMissingRecovery(t, pool, jobID)
	deleteRiverParseJob(t, pool, linkID, jobID)

	projector := &repoTerminalProjector{repo: repository.NewPGXLinkRepository(pool)}
	reconcilers := []*worker.ParseTerminalReconciler{
		newParseTerminalReconciler(t, pool, projector),
		newParseTerminalReconciler(t, pool, projector),
	}
	errCh := make(chan error, len(reconcilers))
	var wg sync.WaitGroup
	for _, reconciler := range reconcilers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := reconciler.RunOnce(context.Background())
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Error(fmt.Errorf("missing-row replica reconciliation: %w", err))
		}
	}
	if projector.applied() != 1 {
		t.Fatalf("effective projections = %d, want exactly 1", projector.applied())
	}
	assertParseProductStatus(t, pool, linkID, jobID, "failed")
	assertParseMissingError(t, pool, linkID, jobID)
	assertNoParseTerminalMismatch(t, reconcilers[0])
}
