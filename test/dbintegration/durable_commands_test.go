package dbintegration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/app/durablework"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

// failAfterLinkQueue delegates the infrastructure operation first, then
// returns the injected error. A passing rollback test therefore proves that
// both the already-issued River mutation and the business mutation share the
// durablework-owned transaction.
type failAfterLinkQueue struct {
	delegate        durablework.LinkQueue
	enqueueErr      error
	cancelActiveErr error
	cancelAllErr    error
}

func (q *failAfterLinkQueue) EnqueueTx(ctx context.Context, tx pgx.Tx, linkID, jobID uuid.UUID) error {
	if err := q.delegate.EnqueueTx(ctx, tx, linkID, jobID); err != nil {
		return err
	}
	return q.enqueueErr
}

func (q *failAfterLinkQueue) CancelActiveTx(ctx context.Context, tx pgx.Tx, linkID, keepJobID uuid.UUID) error {
	if err := q.delegate.CancelActiveTx(ctx, tx, linkID, keepJobID); err != nil {
		return err
	}
	return q.cancelActiveErr
}

func (q *failAfterLinkQueue) CancelAllActiveTx(ctx context.Context, tx pgx.Tx, linkID uuid.UUID) error {
	if err := q.delegate.CancelAllActiveTx(ctx, tx, linkID); err != nil {
		return err
	}
	return q.cancelAllErr
}

func TestDurableSubmitRollsBackBusinessAndRiverAfterRiverInsert(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	realQueue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	wantErr := errors.New("fail after River insert")
	queue := &failAfterLinkQueue{delegate: realQueue, enqueueErr: wantErr}
	commands := dbLinkCommands(pool, links, queue)

	_, err := commands.SubmitLink(t.Context(), service.SubmitLinkCommand{Capture: service.LinkCapture{
		URL: "https://durable.example.com/submit-rollback", Status: model.LinkStatusPending,
	}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("SubmitLink() error = %v, want %v", err, wantErr)
	}
	if got := rawCountLinks(t, pool); got != 0 {
		t.Fatalf("links after rollback = %d, want 0", got)
	}
	if got := rawCountJobs(t, pool); got != 0 {
		t.Fatalf("parse jobs after rollback = %d, want 0", got)
	}
	assertRiverParseRows(t, pool, 0)
}

func TestDurableRequeueRollsBackBusinessAndRiverAfterCancellation(t *testing.T) {
	pool := StartPostgres(t)
	linkID, oldJobID := insertPendingLinkAndJob(t, pool, "https://durable.example.com/requeue-cancel-rollback")
	ctx := t.Context()
	realQueue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := realQueue.Enqueue(ctx, linkID, oldJobID); err != nil {
		t.Fatalf("enqueue old attempt: %v", err)
	}
	wantErr := errors.New("fail after River cancellation")
	queue := &failAfterLinkQueue{delegate: realQueue, cancelActiveErr: wantErr}
	commands := dbLinkCommands(pool, repository.NewPGXLinkRepository(pool), queue)

	_, err := commands.RequeueLink(ctx, service.RequeueLinkCommand{LinkID: linkID})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RequeueLink() error = %v, want %v", err, wantErr)
	}
	assertLinkAndAttemptRemain(t, pool, linkID, oldJobID)
	assertActiveRiverAttempt(t, pool, oldJobID)
}

func TestDurableLinkDeleteRollsBackBusinessAndRiverAfterCancellation(t *testing.T) {
	pool := StartPostgres(t)
	linkID, jobID := insertPendingLinkAndJob(t, pool, "https://durable.example.com/link-delete-rollback")
	ctx := t.Context()
	realQueue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := realQueue.Enqueue(ctx, linkID, jobID); err != nil {
		t.Fatalf("enqueue attempt: %v", err)
	}
	wantErr := errors.New("fail after link River cancellation")
	queue := &failAfterLinkQueue{delegate: realQueue, cancelAllErr: wantErr}
	commands := dbLinkCommands(pool, repository.NewPGXLinkRepository(pool), queue)

	err := commands.DeleteLink(ctx, service.DeleteLinkCommand{LinkID: linkID})
	if !errors.Is(err, wantErr) {
		t.Fatalf("DeleteLink() error = %v, want %v", err, wantErr)
	}
	assertLinkAndAttemptRemain(t, pool, linkID, jobID)
	assertActiveRiverAttempt(t, pool, jobID)
}

func TestDurableLinkDeleteTerminalizesAttemptAndResubmitRestoresSameID(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	commands := dbLinkCommands(pool, links, queue)
	const rawURL = "https://durable.example.com/delete-resubmit"
	created, err := commands.SubmitLink(ctx, service.SubmitLinkCommand{Capture: service.LinkCapture{
		URL: rawURL, Status: model.LinkStatusPending,
	}})
	if err != nil || created.Link == nil || created.Job == nil {
		t.Fatalf("SubmitLink() = %#v, %v", created, err)
	}
	linkID, oldJobID := created.Link.ID, created.Job.ID

	if err := commands.DeleteLink(ctx, service.DeleteLinkCommand{LinkID: linkID}); err != nil {
		t.Fatalf("DeleteLink() error = %v", err)
	}
	var oldStatus, oldReason string
	if err := pool.QueryRow(ctx, `SELECT status,COALESCE(error_msg,'') FROM parse_jobs WHERE id=$1`, oldJobID).Scan(&oldStatus, &oldReason); err != nil {
		t.Fatalf("read deleted attempt: %v", err)
	}
	if oldStatus != string(model.JobStatusFailed) || oldReason != "link_deleted" {
		t.Fatalf("deleted attempt = %s/%s, want failed/link_deleted", oldStatus, oldReason)
	}
	assertReaderFeedLinkLive(t, pool, linkID, false)

	restored, err := commands.SubmitLink(ctx, service.SubmitLinkCommand{Capture: service.LinkCapture{
		URL: rawURL, Status: model.LinkStatusPending,
	}})
	if err != nil || restored.Link == nil || restored.Link.ID != linkID || restored.Job == nil || restored.Job.ID == oldJobID || !restored.Restored || restored.Inserted {
		t.Fatalf("restored SubmitLink() = %#v, %v", restored, err)
	}
	assertReaderFeedLinkLive(t, pool, linkID, true)
	var runnable int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM parse_jobs WHERE link_id=$1 AND status IN ('pending','processing')`, linkID).Scan(&runnable); err != nil {
		t.Fatalf("count restored attempts: %v", err)
	}
	if runnable != 1 {
		t.Fatalf("runnable restored attempts = %d, want 1", runnable)
	}
	assertActiveRiverAttempt(t, pool, restored.Job.ID)
}

func TestDurableLinkDeleteRollsBackRiverAndLinkWhenAttemptTerminalizationFails(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	commands := dbLinkCommands(pool, links, queue)
	created, err := commands.SubmitLink(ctx, service.SubmitLinkCommand{Capture: service.LinkCapture{
		URL: "https://durable.example.com/delete-attempt-rollback", Status: model.LinkStatusPending,
	}})
	if err != nil || created.Link == nil || created.Job == nil {
		t.Fatalf("SubmitLink() = %#v, %v", created, err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_link_deleted_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.error_msg = 'link_deleted' THEN
				RAISE EXCEPTION 'injected link_deleted attempt failure';
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER fail_link_deleted_attempt
		BEFORE UPDATE ON parse_jobs
		FOR EACH ROW EXECUTE FUNCTION fail_link_deleted_attempt()`); err != nil {
		t.Fatalf("install attempt failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS fail_link_deleted_attempt ON parse_jobs; DROP FUNCTION IF EXISTS fail_link_deleted_attempt()`)
	})

	if err := commands.DeleteLink(ctx, service.DeleteLinkCommand{LinkID: created.Link.ID}); err == nil {
		t.Fatal("DeleteLink() with injected attempt failure succeeded")
	}
	assertReaderFeedLinkLive(t, pool, created.Link.ID, true)
	assertLinkAndAttemptRemain(t, pool, created.Link.ID, created.Job.ID)
	assertActiveRiverAttempt(t, pool, created.Job.ID)
}

func TestDurableConversionRollsBackBusinessAndRiverAfterInsert(t *testing.T) {
	pool := StartPostgres(t)
	harness := &savedGenerationHarness{PGXLinkRepository: repository.NewPGXLinkRepository(pool), pool: pool}
	fixture, site := newGenerationSiteFixture(t, harness, "durable-conversion-rollback")
	before := readSavedGeneration(t, harness, fixture.linkID)
	realQueue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	wantErr := errors.New("fail after conversion River insert")
	queue := &failAfterLinkQueue{delegate: realQueue, enqueueErr: wantErr}
	commands := dbLinkCommands(pool, harness.PGXLinkRepository, queue)

	_, err := commands.ConvertLink(t.Context(), service.ConvertLinkCommand{
		LinkID: fixture.linkID, TargetKind: model.LibraryKindReading,
		ExpectedContentRevision: before.revision, ExpectedSiteRevision: site.SiteRevision,
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("ConvertLink() error = %v, want %v", err, wantErr)
	}
	after := readSavedGeneration(t, harness, fixture.linkID)
	if after.revision != before.revision || after.status != model.LinkStatusDone || !after.hasKind || after.kind != model.LibraryKindSite {
		t.Fatalf("conversion rollback state = %#v, want unchanged %#v", after, before)
	}
	assertRiverParseRows(t, pool, 0)
}

func assertRiverParseRows(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM river_job WHERE kind='parse_link'`).Scan(&got); err != nil {
		t.Fatalf("count River parse rows: %v", err)
	}
	if got != want {
		t.Fatalf("River parse rows = %d, want %d", got, want)
	}
}

func rawCountLinks(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM links`).Scan(&count); err != nil {
		t.Fatalf("count links: %v", err)
	}
	return count
}

func assertLinkAndAttemptRemain(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, linkID, jobID uuid.UUID) {
	t.Helper()
	var status string
	if err := pool.QueryRow(t.Context(), `SELECT status FROM links WHERE id=$1`, linkID).Scan(&status); err != nil {
		t.Fatalf("read rolled-back link: %v", err)
	}
	if status != string(model.LinkStatusPending) {
		t.Fatalf("link status after rollback = %q, want pending", status)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM parse_jobs WHERE id=$1`, jobID).Scan(&count); err != nil {
		t.Fatalf("read rolled-back parse attempt: %v", err)
	}
	if count != 1 {
		t.Fatalf("original parse attempt count = %d, want 1", count)
	}
}

func assertActiveRiverAttempt(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, parseJobID uuid.UUID) {
	t.Helper()
	var state string
	var cancellationMarked bool
	if err := pool.QueryRow(t.Context(), `SELECT state::text, metadata ? 'cancel_attempted_at'
		FROM river_job WHERE kind='parse_link' AND args->>'parse_job_id'=$1`, parseJobID.String()).
		Scan(&state, &cancellationMarked); err != nil {
		t.Fatalf("read River attempt after rollback: %v", err)
	}
	if state == "cancelled" || cancellationMarked {
		t.Fatalf("River attempt after rollback = state %q, cancellation marked %v", state, cancellationMarked)
	}
}
