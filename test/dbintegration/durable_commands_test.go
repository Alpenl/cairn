package dbintegration

import (
	"context"
	"errors"
	"fmt"
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

func (q *failAfterLinkQueue) EnqueueTx(ctx context.Context, tx pgx.Tx, attempt model.ParseAttempt) error {
	if err := q.delegate.EnqueueTx(ctx, tx, attempt); err != nil {
		return err
	}
	return q.enqueueErr
}

func (q *failAfterLinkQueue) CancelActiveTx(ctx context.Context, tx pgx.Tx, linkID uuid.UUID) error {
	if err := q.delegate.CancelActiveTx(ctx, tx, linkID); err != nil {
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
	assertRiverParseRows(t, pool, 0)
}

func TestDurableRequeueRollsBackBusinessAndRiverAfterCancellation(t *testing.T) {
	pool := StartPostgres(t)
	linkID, oldAttempt := insertPendingLinkAttempt(t, pool, "https://durable.example.com/requeue-cancel-rollback")
	ctx := t.Context()
	realQueue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := realQueue.Enqueue(ctx, oldAttempt); err != nil {
		t.Fatalf("enqueue old attempt: %v", err)
	}
	wantErr := errors.New("fail after River cancellation")
	queue := &failAfterLinkQueue{delegate: realQueue, cancelActiveErr: wantErr}
	commands := dbLinkCommands(pool, repository.NewPGXLinkRepository(pool), queue)

	_, err := commands.RequeueLink(ctx, service.RequeueLinkCommand{LinkID: linkID})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RequeueLink() error = %v, want %v", err, wantErr)
	}
	assertLinkAndAttemptRemain(t, pool, oldAttempt)
	assertActiveRiverAttempt(t, pool, oldAttempt)
}

func TestDurableLinkDeleteRollsBackBusinessAndRiverAfterCancellation(t *testing.T) {
	pool := StartPostgres(t)
	linkID, attempt := insertPendingLinkAttempt(t, pool, "https://durable.example.com/link-delete-rollback")
	ctx := t.Context()
	realQueue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := realQueue.Enqueue(ctx, attempt); err != nil {
		t.Fatalf("enqueue attempt: %v", err)
	}
	wantErr := errors.New("fail after link River cancellation")
	queue := &failAfterLinkQueue{delegate: realQueue, cancelAllErr: wantErr}
	commands := dbLinkCommands(pool, repository.NewPGXLinkRepository(pool), queue)

	err := commands.DeleteLink(ctx, service.DeleteLinkCommand{LinkID: linkID})
	if !errors.Is(err, wantErr) {
		t.Fatalf("DeleteLink() error = %v, want %v", err, wantErr)
	}
	assertLinkAndAttemptRemain(t, pool, attempt)
	assertActiveRiverAttempt(t, pool, attempt)
}

func TestDurableLinkDeleteCancelsAttemptAndResubmitRestoresSameID(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	commands := dbLinkCommands(pool, links, queue)
	const rawURL = "https://durable.example.com/delete-resubmit"
	created, err := commands.SubmitLink(ctx, service.SubmitLinkCommand{Capture: service.LinkCapture{
		URL: rawURL, Status: model.LinkStatusPending,
	}})
	if err != nil || created.Link == nil || !created.Enqueued {
		t.Fatalf("SubmitLink() = %#v, %v", created, err)
	}
	linkID := created.Link.ID
	oldAttempt := parseAttemptForLink(created.Link)

	if err := commands.DeleteLink(ctx, service.DeleteLinkCommand{LinkID: linkID}); err != nil {
		t.Fatalf("DeleteLink() error = %v", err)
	}
	assertReaderFeedLinkLive(t, pool, linkID, false)
	assertCancelledRiverAttempt(t, pool, oldAttempt)

	restored, err := commands.SubmitLink(ctx, service.SubmitLinkCommand{Capture: service.LinkCapture{
		URL: rawURL, Status: model.LinkStatusPending,
	}})
	if err != nil || restored.Link == nil || restored.Link.ID != linkID || !restored.Enqueued {
		t.Fatalf("restored SubmitLink() = %#v, %v", restored, err)
	}
	assertReaderFeedLinkLive(t, pool, linkID, true)
	// SubmitLink returns the conflict/restore decision's input snapshot. The
	// durable restore increments parse_generation in the same transaction, so
	// read the committed Link before asserting the replacement attempt.
	restoredLink, err := links.GetByID(ctx, linkID)
	if err != nil || restoredLink == nil {
		t.Fatalf("read restored Link: %v", err)
	}
	restoredAttempt := parseAttemptForLink(restoredLink)
	if restoredAttempt.Generation <= oldAttempt.Generation {
		t.Fatalf("restored generation = %d, want newer than %d", restoredAttempt.Generation, oldAttempt.Generation)
	}
	assertActiveRiverAttempt(t, pool, restoredAttempt)
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
}, attempt model.ParseAttempt) {
	t.Helper()
	var (
		status     string
		generation int64
	)
	if err := pool.QueryRow(t.Context(), `SELECT status,parse_generation FROM links WHERE id=$1`, attempt.LinkID).Scan(&status, &generation); err != nil {
		t.Fatalf("read rolled-back link: %v", err)
	}
	if status != string(model.LinkStatusPending) || generation != attempt.Generation {
		t.Fatalf("link after rollback = %q generation %d, want pending generation %d", status, generation, attempt.Generation)
	}
}

func assertActiveRiverAttempt(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, attempt model.ParseAttempt) {
	t.Helper()
	var state string
	var cancellationMarked bool
	if err := pool.QueryRow(t.Context(), `SELECT state::text, metadata ? 'cancel_attempted_at'
		FROM river_job WHERE kind='parse_link' AND args->>'link_id'=$1 AND args->>'parse_generation'=$2`,
		attempt.LinkID.String(), fmt.Sprint(attempt.Generation)).
		Scan(&state, &cancellationMarked); err != nil {
		t.Fatalf("read River attempt after rollback: %v", err)
	}
	if state == "cancelled" || cancellationMarked {
		t.Fatalf("River attempt after rollback = state %q, cancellation marked %v", state, cancellationMarked)
	}
}

func assertCancelledRiverAttempt(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, attempt model.ParseAttempt) {
	t.Helper()
	var state string
	if err := pool.QueryRow(t.Context(), `SELECT state::text FROM river_job
		WHERE kind='parse_link' AND args->>'link_id'=$1 AND args->>'parse_generation'=$2`,
		attempt.LinkID.String(), fmt.Sprint(attempt.Generation)).Scan(&state); err != nil {
		t.Fatalf("read cancelled River attempt: %v", err)
	}
	if state != "cancelled" {
		t.Fatalf("River attempt state = %q, want cancelled", state)
	}
}
