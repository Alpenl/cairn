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
	pool       *pgxpool.Pool
	reader     *repository.PGXReaderVNextRepository
	commands   *durablework.LinkCommands
	queue      *worker.RiverQueue
	url        string
	linkID     uuid.UUID
	oldAttempt model.ParseAttempt
	thoughtID  string
	feedItemID uuid.UUID
}

type failAfterRestoreEnqueueQueue struct {
	delegate *worker.RiverQueue
	err      error
}

func (q *failAfterRestoreEnqueueQueue) EnqueueTx(ctx context.Context, tx pgx.Tx, attempt model.ParseAttempt) error {
	if err := q.delegate.EnqueueTx(ctx, tx, attempt); err != nil {
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
				if err != nil || result.LinkID == nil {
					t.Fatalf("Feed re-save = %#v, %v", result, err)
				}
				restoredID = *result.LinkID
			case "inbox-confirm":
				inboxID := seedReaderVNextInbox(t, fixture.pool, fixture.url, "Restore capture", "Restore body", "Restore summary")
				var err error
				restoredID, err = fixture.reader.ConfirmInbox(t.Context(), inboxID, nil)
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
				if err != nil || result.Link == nil || !result.Enqueued {
					t.Fatalf("SubmitLink() restore = %#v, %v", result, err)
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
	fixture.reader = repository.NewPGXReaderVNextRepositoryWithLinkLifecycle(
		fixture.pool,
		&failAfterRestoreEnqueueQueue{delegate: fixture.queue, err: wantErr},
	)

	if _, err := fixture.reader.RestoreHost(t.Context(), model.ReaderHostLink, fixture.linkID); !errors.Is(err, wantErr) {
		t.Fatalf("RestoreHost() error = %v, want %v", err, wantErr)
	}
	assertDeletedInflightLink(t, fixture.pool, fixture.reader, fixture.oldAttempt, fixture.thoughtID)
}

func newRunnableLinkRestoreFixture(t *testing.T, mode string) runnableLinkRestoreFixture {
	t.Helper()
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	reader := repository.NewPGXReaderVNextRepositoryWithLinkLifecycle(pool, queue)
	commands := dbLinkCommands(pool, links, queue)
	url := "https://restore-runnable.example.test/" + mode

	created, err := commands.SubmitLink(t.Context(), service.SubmitLinkCommand{Capture: runnableRestoreCapture(url)})
	if err != nil || created.Link == nil || !created.Enqueued {
		t.Fatalf("seed durable SubmitLink() = %#v, %v", created, err)
	}
	linkID := created.Link.ID
	oldAttempt := parseAttemptForLink(created.Link)
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
		if err != nil || saved.LinkID == nil || *saved.LinkID != linkID {
			t.Fatalf("seed Feed association = %#v, %v; want existing Link %s", saved, err, linkID)
		}
	}

	if err := commands.DeleteLink(t.Context(), service.DeleteLinkCommand{LinkID: linkID}); err != nil {
		t.Fatalf("durable DeleteLink() error = %v", err)
	}
	assertDeletedInflightLink(t, pool, reader, oldAttempt, thought.id)

	return runnableLinkRestoreFixture{
		pool: pool, reader: reader, commands: commands, queue: queue, url: url,
		linkID: linkID, oldAttempt: oldAttempt, thoughtID: thought.id, feedItemID: feedItemID,
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
	oldAttempt model.ParseAttempt,
	thoughtID string,
) {
	t.Helper()
	assertReaderFeedLinkLive(t, pool, oldAttempt.LinkID, false)
	assertReaderThoughtLifecycle(t, reader, t.Context(), thoughtID, false, "tombstone")

	var generation int64
	if err := pool.QueryRow(t.Context(), `SELECT parse_generation FROM links WHERE id=$1`, oldAttempt.LinkID).Scan(&generation); err != nil {
		t.Fatalf("read deleted Link generation: %v", err)
	}
	if generation != oldAttempt.Generation {
		t.Fatalf("deleted Link generation = %d, want %d", generation, oldAttempt.Generation)
	}
	var activeRiver int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM river_job
		WHERE kind='parse_link'
		  AND args->>'link_id'=$1
		  AND state IN ('available','pending','retryable','running','scheduled')`, oldAttempt.LinkID.String()).Scan(&activeRiver); err != nil {
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
		status     string
		generation int64
		metadata   int64
		deleted    bool
		canonical  int
	)
	if err := fixture.pool.QueryRow(ctx, `SELECT status,parse_generation,metadata_revision,deleted_at IS NOT NULL
		FROM links WHERE id=$1`, fixture.linkID).Scan(&status, &generation, &metadata, &deleted); err != nil {
		t.Fatalf("read restored Link: %v", err)
	}
	if status != string(model.LinkStatusPending) || deleted {
		t.Fatalf("restored Link = status:%q deleted:%v, want pending/live", status, deleted)
	}
	if generation != fixture.oldAttempt.Generation+1 {
		t.Fatalf("restored generation = %d, want %d", generation, fixture.oldAttempt.Generation+1)
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

	var activeRiver, replacementRiver int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*),count(*) FILTER (
			WHERE args->>'parse_generation'=$2::bigint::text
			  AND args->>'expected_metadata_revision'=$3::bigint::text
		) FROM river_job
		WHERE kind='parse_link'
		  AND args->>'link_id'=$1
		  AND state IN ('available','pending','retryable','running','scheduled')
		  AND NOT (river_job.metadata ? 'cancel_attempted_at')`,
		fixture.linkID.String(), generation, fixture.oldAttempt.ExpectedMetadataRevision).Scan(&activeRiver, &replacementRiver); err != nil {
		t.Fatalf("count active replacement River jobs: %v", err)
	}
	if activeRiver != 1 || replacementRiver != 1 {
		t.Fatalf("active River jobs = total:%d replacement:%d, want 1/1 (generation %d, metadata fence %d; live metadata revision %d)",
			activeRiver, replacementRiver, generation, fixture.oldAttempt.ExpectedMetadataRevision, metadata)
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
