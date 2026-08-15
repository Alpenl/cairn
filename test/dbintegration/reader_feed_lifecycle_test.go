package dbintegration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
	"webtag/internal/worker"
)

type failAfterFeedLifecycleCancel struct {
	delegate repository.ReaderLinkLifecycleQueue
	err      error
}

func (q *failAfterFeedLifecycleCancel) EnqueueTx(
	ctx context.Context,
	tx pgx.Tx,
	linkID uuid.UUID,
	jobID uuid.UUID,
) error {
	return q.delegate.EnqueueTx(ctx, tx, linkID, jobID)
}

func (q *failAfterFeedLifecycleCancel) CancelAllActiveTx(
	ctx context.Context,
	tx pgx.Tx,
	linkID uuid.UUID,
) error {
	if err := q.delegate.CancelAllActiveTx(ctx, tx, linkID); err != nil {
		return err
	}
	return q.err
}

type readerFeedLifecycleFixture struct {
	reader        *repository.PGXReaderVNextRepository
	queue         *worker.RiverQueue
	feedItemID    uuid.UUID
	linkID        uuid.UUID
	parseJobID    uuid.UUID
	translationID uuid.UUID
}

func TestReaderFeedLastUnsaveCancelsInflightLifecycleAtomically(t *testing.T) {
	pool := StartPostgres(t)
	fixture := seedReaderFeedInflightLifecycle(t, pool, "cancel")
	fixture.reader.BindLinkLifecycleQueue(fixture.queue)

	result, err := fixture.reader.FeedbackFeed(
		t.Context(),
		"subscription:"+fixture.feedItemID.String(),
		"unsave",
	)
	if err != nil {
		t.Fatalf("FeedbackFeed(unsave) error = %v", err)
	}
	if result.Saved || result.Association == nil || result.Association.LinkID != fixture.linkID {
		t.Fatalf("FeedbackFeed(unsave) = %#v, want removed association for %s", result, fixture.linkID)
	}
	if got := countReaderFeedSaves(t, pool, fixture.feedItemID, fixture.linkID); got != 0 {
		t.Fatalf("Feed associations after unsave = %d, want 0", got)
	}

	assertReaderFeedLifecycleDeleted(t, pool, fixture)
}

func TestReaderFeedLastUnsaveRollsBackAfterLifecycleCancellationFailure(t *testing.T) {
	pool := StartPostgres(t)
	fixture := seedReaderFeedInflightLifecycle(t, pool, "cancel-rollback")
	wantErr := errors.New("injected Feed lifecycle cancellation failure")
	fixture.reader.BindLinkLifecycleQueue(&failAfterFeedLifecycleCancel{
		delegate: fixture.queue,
		err:      wantErr,
	})

	_, err := fixture.reader.FeedbackFeed(
		t.Context(),
		"subscription:"+fixture.feedItemID.String(),
		"unsave",
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("FeedbackFeed(unsave) error = %v, want %v", err, wantErr)
	}

	if got := countReaderFeedSaves(t, pool, fixture.feedItemID, fixture.linkID); got != 1 {
		t.Fatalf("Feed associations after rollback = %d, want 1", got)
	}
	var feedbackAction string
	if err := pool.QueryRow(t.Context(),
		`SELECT action FROM reader_feed_feedback WHERE item_key=$1`,
		"subscription:"+fixture.feedItemID.String(),
	).Scan(&feedbackAction); err != nil {
		t.Fatalf("read Feed feedback after rollback: %v", err)
	}
	if feedbackAction != "save" {
		t.Fatalf("Feed feedback after rollback = %q, want save", feedbackAction)
	}

	assertReaderFeedLifecycleActive(t, pool, fixture)
}

func seedReaderFeedInflightLifecycle(
	t *testing.T,
	pool *pgxpool.Pool,
	suffix string,
) readerFeedLifecycleFixture {
	t.Helper()
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	feedItemID := seedReaderFeedSaveItem(
		t,
		pool,
		"https://feed-lifecycle.example.test/"+suffix,
		suffix,
	)
	saved, err := reader.FeedbackFeed(ctx, "subscription:"+feedItemID.String(), "save")
	if err != nil || saved.Association == nil || !saved.Association.CreatedLink {
		t.Fatalf("seed Feed save = %#v, %v", saved, err)
	}
	linkID := saved.Association.LinkID

	commands := dbLinkCommands(pool, repository.NewPGXLinkRepository(pool), queue)
	requeued, err := commands.RequeueLink(ctx, service.RequeueLinkCommand{LinkID: linkID})
	if err != nil || requeued.Job == nil {
		t.Fatalf("RequeueLink() = %#v, %v", requeued, err)
	}

	translationID := schedulePendingTranslation(
		t,
		pool,
		queue,
		ctx,
		linkID,
		"Feed lifecycle translation "+suffix,
	)
	fixture := readerFeedLifecycleFixture{
		reader:        reader,
		queue:         queue,
		feedItemID:    feedItemID,
		linkID:        linkID,
		parseJobID:    requeued.Job.ID,
		translationID: translationID,
	}
	assertReaderFeedLifecycleActive(t, pool, fixture)
	return fixture
}

func assertReaderFeedLifecycleDeleted(
	t *testing.T,
	pool *pgxpool.Pool,
	fixture readerFeedLifecycleFixture,
) {
	t.Helper()
	ctx := t.Context()
	var trashed bool
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at IS NOT NULL FROM links WHERE id=$1`,
		fixture.linkID,
	).Scan(&trashed); err != nil {
		t.Fatalf("read Feed Link after unsave: %v", err)
	}
	if !trashed {
		t.Fatal("last Feed unsave did not move the Link to Trash")
	}

	var parseStatus model.JobStatus
	var parseReason string
	if err := pool.QueryRow(ctx,
		`SELECT status,COALESCE(error_msg,'') FROM parse_jobs WHERE id=$1`,
		fixture.parseJobID,
	).Scan(&parseStatus, &parseReason); err != nil {
		t.Fatalf("read parse attempt after Feed unsave: %v", err)
	}
	if parseStatus != model.JobStatusFailed || parseReason != "link_deleted" {
		t.Fatalf("parse attempt after Feed unsave = %s/%q, want failed/link_deleted", parseStatus, parseReason)
	}
	assertReaderFeedRiverCancellation(t, pool, "parse_link", "parse_job_id", fixture.parseJobID, true)

	var translationStatus model.TranslationStatus
	var translationReason string
	var currentRiverJobID *int64
	if err := pool.QueryRow(ctx,
		`SELECT status,COALESCE(error_msg,''),current_river_job_id
		 FROM link_translations WHERE id=$1`,
		fixture.translationID,
	).Scan(&translationStatus, &translationReason, &currentRiverJobID); err != nil {
		t.Fatalf("read translation attempt after Feed unsave: %v", err)
	}
	if translationStatus != model.TranslationStatusFailed || translationReason != "link_deleted" || currentRiverJobID != nil {
		t.Fatalf(
			"translation after Feed unsave = %s/%q current=%v, want failed/link_deleted current=nil",
			translationStatus,
			translationReason,
			currentRiverJobID,
		)
	}
	assertReaderFeedRiverCancellation(t, pool, "translate_link_v2", "translation_id", fixture.translationID, true)
}

func assertReaderFeedLifecycleActive(
	t *testing.T,
	pool *pgxpool.Pool,
	fixture readerFeedLifecycleFixture,
) {
	t.Helper()
	ctx := t.Context()
	var (
		linkStatus model.LinkStatus
		trashed    bool
	)
	if err := pool.QueryRow(ctx,
		`SELECT status,deleted_at IS NOT NULL FROM links WHERE id=$1`,
		fixture.linkID,
	).Scan(&linkStatus, &trashed); err != nil {
		t.Fatalf("read active Feed Link: %v", err)
	}
	if linkStatus != model.LinkStatusPending || trashed {
		t.Fatalf("active Feed Link = status %s trashed %v, want pending/false", linkStatus, trashed)
	}

	var parseStatus model.JobStatus
	var parseReason string
	if err := pool.QueryRow(ctx,
		`SELECT status,COALESCE(error_msg,'') FROM parse_jobs WHERE id=$1`,
		fixture.parseJobID,
	).Scan(&parseStatus, &parseReason); err != nil {
		t.Fatalf("read active parse attempt: %v", err)
	}
	if parseStatus != model.JobStatusPending || parseReason != "" {
		t.Fatalf("active parse attempt = %s/%q, want pending/empty", parseStatus, parseReason)
	}
	assertReaderFeedRiverCancellation(t, pool, "parse_link", "parse_job_id", fixture.parseJobID, false)

	var translationStatus model.TranslationStatus
	var translationReason string
	var currentRiverJobID *int64
	if err := pool.QueryRow(ctx,
		`SELECT status,COALESCE(error_msg,''),current_river_job_id
		 FROM link_translations WHERE id=$1`,
		fixture.translationID,
	).Scan(&translationStatus, &translationReason, &currentRiverJobID); err != nil {
		t.Fatalf("read active translation attempt: %v", err)
	}
	if translationStatus != model.TranslationStatusPending || translationReason != "" || currentRiverJobID == nil {
		t.Fatalf(
			"active translation = %s/%q current=%v, want pending/empty current job",
			translationStatus,
			translationReason,
			currentRiverJobID,
		)
	}
	assertReaderFeedRiverCancellation(t, pool, "translate_link_v2", "translation_id", fixture.translationID, false)
}

func assertReaderFeedRiverCancellation(
	t *testing.T,
	pool *pgxpool.Pool,
	kind string,
	argKey string,
	attemptID uuid.UUID,
	wantCancelled bool,
) {
	t.Helper()
	var (
		state  string
		marked bool
	)
	if err := pool.QueryRow(t.Context(), `SELECT state::text,metadata ? 'cancel_attempted_at'
		FROM river_job
		WHERE kind=$1 AND args->>$2=$3`, kind, argKey, attemptID.String()).Scan(&state, &marked); err != nil {
		t.Fatalf("read River %s job for %s: %v", kind, attemptID, err)
	}
	cancelled := state == "cancelled" || marked
	if cancelled != wantCancelled {
		t.Fatalf(
			"River %s job for %s = state %q marked %v, want cancelled/marked %v",
			kind,
			attemptID,
			state,
			marked,
			wantCancelled,
		)
	}
}
