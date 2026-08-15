package dbintegration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/app/durablework"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
	"webtag/internal/worker"
)

type runnableLinkRestoreFixture struct {
	pool                        *pgxpool.Pool
	reader                      *repository.PGXReaderVNextRepository
	commands                    *durablework.LinkCommands
	queue                       *worker.RiverQueue
	url                         string
	linkID                      uuid.UUID
	oldJobID                    uuid.UUID
	thoughtID                   string
	feedItemID                  uuid.UUID
	legacyTranslationID         uuid.UUID
	legacyTranslationRiverJobID int64
}

type failAfterRestoreEnqueueQueue struct {
	delegate *worker.RiverQueue
	err      error
}

func (q *failAfterRestoreEnqueueQueue) EnqueueTx(ctx context.Context, tx pgx.Tx, linkID, jobID uuid.UUID) error {
	if err := q.delegate.EnqueueTx(ctx, tx, linkID, jobID); err != nil {
		return err
	}
	return q.err
}

func (q *failAfterRestoreEnqueueQueue) CancelAllActiveTx(ctx context.Context, tx pgx.Tx, linkID uuid.UUID) error {
	return q.delegate.CancelAllActiveTx(ctx, tx, linkID)
}

func TestDeletedInflightLinkRestoresWithOneRunnableReplacement(t *testing.T) {
	for _, mode := range []string{"feed-existing-association", "inbox-confirm", "public-restore", "submit"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newRunnableLinkRestoreFixture(t, mode)

			var restoredID uuid.UUID
			switch mode {
			case "feed-existing-association":
				result, err := fixture.reader.FeedbackFeed(t.Context(), "subscription:"+fixture.feedItemID.String(), "save")
				if err != nil || result.Association == nil {
					t.Fatalf("Feed re-save = %#v, %v", result, err)
				}
				restoredID = result.Association.LinkID
			case "inbox-confirm":
				inboxID := seedReaderVNextInbox(t, fixture.pool, fixture.url, "Restore capture", "Restore body", "Restore summary")
				var err error
				restoredID, err = fixture.reader.ConfirmInbox(t.Context(), inboxID)
				if err != nil {
					t.Fatalf("ConfirmInbox() error = %v", err)
				}
			case "public-restore":
				result, err := fixture.reader.RestoreHost(t.Context(), model.ReaderHostLink, fixture.linkID)
				if err != nil || !result.Changed {
					t.Fatalf("RestoreHost() = %#v, %v", result, err)
				}
				restoredID = result.HostID
			case "submit":
				result, err := fixture.commands.SubmitLink(t.Context(), service.SubmitLinkCommand{Capture: runnableRestoreCapture(fixture.url)})
				if err != nil || result.Link == nil || result.Job == nil || !result.Restored || result.Inserted {
					t.Fatalf("SubmitLink() restore = %#v, %v", result, err)
				}
				if result.Job.ID == fixture.oldJobID {
					t.Fatalf("SubmitLink() reused deleted parse attempt %s", fixture.oldJobID)
				}
				restoredID = result.Link.ID
			default:
				t.Fatalf("unknown restore mode %q", mode)
			}

			if restoredID != fixture.linkID {
				t.Fatalf("restored Link ID = %s, want canonical %s", restoredID, fixture.linkID)
			}
			assertRunnableLinkRestore(t, fixture)
		})
	}
}

func TestDeletedInflightLinkRestoreRollsBackAfterReplacementEnqueueFailure(t *testing.T) {
	fixture := newRunnableLinkRestoreFixture(t, "public-restore-rollback")
	wantErr := errors.New("injected failure after replacement enqueue")
	fixture.reader.BindLinkLifecycleQueue(&failAfterRestoreEnqueueQueue{delegate: fixture.queue, err: wantErr})

	if _, err := fixture.reader.RestoreHost(t.Context(), model.ReaderHostLink, fixture.linkID); !errors.Is(err, wantErr) {
		t.Fatalf("RestoreHost() error = %v, want %v", err, wantErr)
	}
	assertDeletedInflightLink(t, fixture.pool, fixture.reader, fixture.linkID, fixture.oldJobID, fixture.thoughtID)
	var attempts int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM parse_jobs WHERE link_id=$1`, fixture.linkID).Scan(&attempts); err != nil {
		t.Fatalf("count parse attempts after restore rollback: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("parse attempts after restore rollback = %d, want only original attempt", attempts)
	}
}

func newRunnableLinkRestoreFixture(t *testing.T, mode string) runnableLinkRestoreFixture {
	t.Helper()
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	reader := repository.NewPGXReaderVNextRepository(pool)
	reader.BindLinkLifecycleQueue(queue)
	commands := dbLinkCommands(pool, links, queue)
	url := "https://restore-runnable.example.test/" + mode

	created, err := commands.SubmitLink(t.Context(), service.SubmitLinkCommand{Capture: runnableRestoreCapture(url)})
	if err != nil || created.Link == nil || created.Job == nil || !created.Inserted {
		t.Fatalf("seed durable SubmitLink() = %#v, %v", created, err)
	}
	linkID, oldJobID := created.Link.ID, created.Job.ID
	const body = "restore runnable anchor"
	if _, err := pool.Exec(t.Context(), `UPDATE links SET content=$2,content_document=$2 WHERE id=$1`, linkID, body); err != nil {
		t.Fatalf("seed Link body: %v", err)
	}
	thought := seedReaderLifecycleThought(t, reader, t.Context(), readerLifecycleHostFixture{
		kind: model.ReaderHostLink, id: linkID, body: body, revision: 1,
	}, "restore-runnable-"+mode, "anchor")

	var feedItemID uuid.UUID
	if mode == "feed-existing-association" {
		feedItemID = seedReaderFeedSaveItem(t, pool, url, "restore-runnable")
		saved, err := reader.FeedbackFeed(t.Context(), "subscription:"+feedItemID.String(), "save")
		if err != nil || saved.Association == nil || saved.Association.LinkID != linkID || saved.Association.CreatedLink {
			t.Fatalf("seed Feed association = %#v, %v; want existing Link %s", saved, err, linkID)
		}
	}

	if err := commands.DeleteLink(t.Context(), service.DeleteLinkCommand{LinkID: linkID}); err != nil {
		t.Fatalf("durable DeleteLink() error = %v", err)
	}
	assertDeletedInflightLink(t, pool, reader, linkID, oldJobID, thought.id)
	var legacyTranslationID uuid.UUID
	var legacyTranslationRiverJobID int64
	if mode != "submit" {
		legacyTranslationID = uuid.New()
		legacyTranslationRiverJobID = insertActiveRiverJob(t, pool, map[string]any{
			"translation_id": legacyTranslationID,
		}, "translate_link_v2")
		seedLifecycleRepairTranslation(
			t, pool, linkID, legacyTranslationID, "pending", 1, legacyTranslationRiverJobID, 0,
		)
	}

	return runnableLinkRestoreFixture{
		pool: pool, reader: reader, commands: commands, queue: queue, url: url,
		linkID: linkID, oldJobID: oldJobID, thoughtID: thought.id, feedItemID: feedItemID,
		legacyTranslationID: legacyTranslationID, legacyTranslationRiverJobID: legacyTranslationRiverJobID,
	}
}

func runnableRestoreCapture(rawURL string) service.LinkCapture {
	return service.LinkCapture{
		URL: rawURL, SourceKind: "url", SourceKey: rawURL, Status: model.LinkStatusPending,
	}
}

func assertDeletedInflightLink(
	t *testing.T,
	pool *pgxpool.Pool,
	reader *repository.PGXReaderVNextRepository,
	linkID, oldJobID uuid.UUID,
	thoughtID string,
) {
	t.Helper()
	assertReaderFeedLinkLive(t, pool, linkID, false)
	assertReaderThoughtLifecycle(t, reader, t.Context(), thoughtID, false, "tombstone")

	var status, reason string
	if err := pool.QueryRow(t.Context(), `SELECT status,COALESCE(error_msg,'') FROM parse_jobs WHERE id=$1`, oldJobID).Scan(&status, &reason); err != nil {
		t.Fatalf("read deleted parse attempt: %v", err)
	}
	if status != string(model.JobStatusFailed) || reason != "link_deleted" {
		t.Fatalf("deleted parse attempt = %s/%s, want failed/link_deleted", status, reason)
	}
	var activeRiver int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM river_job
		WHERE kind='parse_link'
		  AND args->>'link_id'=$1
		  AND state IN ('available','pending','retryable','running','scheduled')`, linkID.String()).Scan(&activeRiver); err != nil {
		t.Fatalf("count active River jobs after delete: %v", err)
	}
	if activeRiver != 0 {
		t.Fatalf("active River jobs after delete = %d, want 0", activeRiver)
	}
}

func assertRunnableLinkRestore(t *testing.T, fixture runnableLinkRestoreFixture) {
	t.Helper()
	ctx := t.Context()
	var (
		status    string
		deleted   bool
		canonical int
	)
	if err := fixture.pool.QueryRow(ctx, `SELECT status,deleted_at IS NOT NULL FROM links WHERE id=$1`, fixture.linkID).Scan(&status, &deleted); err != nil {
		t.Fatalf("read restored Link: %v", err)
	}
	if status != string(model.LinkStatusPending) || deleted {
		t.Fatalf("restored Link = status:%q deleted:%v, want pending/live", status, deleted)
	}
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM links WHERE source_key=$1`, fixture.url).Scan(&canonical); err != nil {
		t.Fatalf("count canonical Links: %v", err)
	}
	if canonical != 1 {
		t.Fatalf("canonical Link count = %d, want 1", canonical)
	}
	if fixture.feedItemID != uuid.Nil && countReaderFeedSaves(t, fixture.pool, fixture.feedItemID, fixture.linkID) != 1 {
		t.Fatal("Feed restore did not preserve exactly one existing association")
	}

	var (
		runnable int
		attempts int
	)
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status IN ('pending','processing')),count(*)
		FROM parse_jobs WHERE link_id=$1`, fixture.linkID).Scan(&runnable, &attempts); err != nil {
		t.Fatalf("read restored parse attempts: %v", err)
	}
	var replacementID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `
		SELECT id FROM parse_jobs
		WHERE link_id=$1 AND status IN ('pending','processing')`, fixture.linkID).Scan(&replacementID); err != nil {
		t.Fatalf("read restored parse attempt ID: %v", err)
	}
	if runnable != 1 || attempts != 2 || replacementID == fixture.oldJobID {
		t.Fatalf("restored attempts = runnable:%d total:%d replacement:%s, want 1/2/new", runnable, attempts, replacementID)
	}

	var activeRiver, replacementRiver int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*),count(*) FILTER (WHERE args->>'parse_job_id'=$2) FROM river_job
		WHERE kind='parse_link'
		  AND args->>'link_id'=$1
		  AND state IN ('available','pending','retryable','running','scheduled')
		  AND NOT (metadata ? 'cancel_attempted_at')`, fixture.linkID.String(), replacementID.String()).Scan(&activeRiver, &replacementRiver); err != nil {
		t.Fatalf("count active replacement River jobs: %v", err)
	}
	if activeRiver != 1 || replacementRiver != 1 {
		t.Fatalf("active River jobs = total:%d replacement:%d, want 1/1", activeRiver, replacementRiver)
	}
	if fixture.legacyTranslationID != uuid.Nil {
		var translationStatus, translationReason, translationRiverState string
		var currentRiverJobID *int64
		if err := fixture.pool.QueryRow(ctx, `SELECT status,COALESCE(error_msg,''),current_river_job_id
			FROM link_translations WHERE id=$1`, fixture.legacyTranslationID).Scan(
			&translationStatus, &translationReason, &currentRiverJobID,
		); err != nil {
			t.Fatalf("read restored legacy translation: %v", err)
		}
		if translationStatus != "failed" || translationReason != "link_deleted" || currentRiverJobID != nil {
			t.Fatalf("restored legacy translation = %s/%s current:%v, want failed/link_deleted/nil",
				translationStatus, translationReason, currentRiverJobID)
		}
		if err := fixture.pool.QueryRow(ctx, `SELECT state::text FROM river_job WHERE id=$1`,
			fixture.legacyTranslationRiverJobID).Scan(&translationRiverState); err != nil {
			t.Fatalf("read restored legacy translation River job: %v", err)
		}
		if translationRiverState != "cancelled" {
			t.Fatalf("restored legacy translation River state = %q, want cancelled", translationRiverState)
		}
	}
	assertReaderThoughtLifecycle(t, fixture.reader, ctx, fixture.thoughtID, false, "active")
	var tombstones int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM reader_thought_tombstones WHERE thought_id=$1`, fixture.thoughtID).Scan(&tombstones); err != nil {
		t.Fatalf("count restored Thought tombstones: %v", err)
	}
	if tombstones != 0 {
		t.Fatalf("restored Thought tombstones = %d, want 0", tombstones)
	}
}
