package dbintegration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/app/durablework"
	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
	"webtag/internal/service/urllock"
	"webtag/internal/worker"
)

type failAfterInboxQueue struct {
	delegate durablework.InboxQueue
	err      error

	mu    sync.Mutex
	calls int
}

type noopInboxSummaryProcessor struct{}

func (noopInboxSummaryProcessor) RunReaderInboxSummaryJob(context.Context, service.ReaderInboxSummaryJobArgs, int, int) error {
	return nil
}

func newInboxRiverQueue(t *testing.T, pool *pgxpool.Pool) *worker.RiverQueue {
	t.Helper()
	queue, err := worker.NewRiverQueue(worker.RiverQueueOptions{
		Pool:                 pool,
		Processor:            newRecordingProcessor(pool),
		TranslationProcessor: noopTranslationProcessor{},
		ReaderInboxProcessor: noopInboxSummaryProcessor{},
		MaxWorkers:           2,
		JobTimeout:           10 * time.Second,
	})
	if err != nil {
		t.Fatalf("new Inbox River queue: %v", err)
	}
	return queue
}

func (q *failAfterInboxQueue) EnqueueReaderInboxSummaryTx(ctx context.Context, tx pgx.Tx, args service.ReaderInboxSummaryJobArgs) error {
	if err := q.delegate.EnqueueReaderInboxSummaryTx(ctx, tx, args); err != nil {
		return err
	}
	q.mu.Lock()
	q.calls++
	q.mu.Unlock()
	return q.err
}

func (q *failAfterInboxQueue) callCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.calls
}

func dbInboxCommands(pool *pgxpool.Pool, inbox *repository.PGXReaderVNextRepository, queue durablework.InboxQueue) *durablework.InboxCommands {
	return durablework.NewInboxCommands(durablework.InboxCommandsOptions{
		Transactions: pool,
		Inbox:        inbox,
		Queue:        queue,
	})
}

func insertInboxDispatchOrphan(t *testing.T, pool *pgxpool.Pool, label, status string) (uuid.UUID, uuid.UUID, int64) {
	t.Helper()
	repo := repository.NewPGXReaderVNextRepository(pool)
	item, err := repo.CreateInbox(t.Context(), model.ReaderInbox{
		URL:             "https://inbox-orphan.example/" + label,
		IdentityKey:     "https://inbox-orphan.example/" + label,
		SourceKind:      "url",
		Body:            "private proposal body",
		ProposalStatus:  "pending",
		ProposalSignals: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create Inbox orphan %q: %v", label, err)
	}
	job, created, err := repo.BeginInboxResummarizeJob(t.Context(), item.ID, item.MetadataRevision)
	if err != nil || !created {
		t.Fatalf("begin Inbox orphan job %q = (%+v, %t, %v), want new job", label, job, created, err)
	}
	if status == "running" {
		if _, err := pool.Exec(t.Context(), `
			UPDATE reader_inbox_jobs
			SET status='running',attempts=1,started_at=NOW(),updated_at=NOW()
			WHERE id=$1`, job.ID); err != nil {
			t.Fatalf("mark Inbox orphan running: %v", err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE reader_inbox SET proposal_status='running' WHERE id=$1`, item.ID); err != nil {
			t.Fatalf("mark Inbox proposal running: %v", err)
		}
	}
	return item.ID, job.ID, job.ExpectedMetadataRevision
}

func countActiveInboxRiverJobs(t *testing.T, pool *pgxpool.Pool, inboxID, jobID uuid.UUID, revision int64) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*)
		FROM river_job
		WHERE kind=$1
			AND args->>'job_id'=$2
			AND args->>'inbox_id'=$3
			AND args->>'expected_metadata_revision'=$4
			AND state IN ('available','pending','retryable','running','scheduled')`,
		service.ReaderInboxSummaryJobKind, jobID.String(), inboxID.String(), fmt.Sprint(revision)).Scan(&count); err != nil {
		t.Fatalf("count active Inbox River jobs: %v", err)
	}
	return count
}

func waitForActiveInboxRiverJob(t *testing.T, pool *pgxpool.Pool, inboxID, jobID uuid.UUID, revision int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if countActiveInboxRiverJobs(t, pool, inboxID, jobID, revision) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for active River job for Inbox %s attempt %s", inboxID, jobID)
}

func TestInboxOrphanReconcilerStartupAutomaticallyRepairsPreloadedOrphan(t *testing.T) {
	pool := StartPostgres(t)
	inboxID, jobID, revision := insertInboxDispatchOrphan(t, pool, "startup", "queued")
	queue := newInboxRiverQueue(t, pool)
	commands := dbInboxCommands(pool, repository.NewPGXReaderVNextRepository(pool), queue)
	reconciler, err := worker.NewReaderInboxOrphanReconciler(worker.ReaderInboxOrphanReconcilerOptions{
		Repairer: commands,
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewReaderInboxOrphanReconciler() error = %v", err)
	}
	if err := reconciler.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := reconciler.Stop(stopCtx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})

	waitForActiveInboxRiverJob(t, pool, inboxID, jobID, revision)
	var jobStatus string
	if err := pool.QueryRow(t.Context(), `SELECT status FROM reader_inbox_jobs WHERE id=$1`, jobID).Scan(&jobStatus); err != nil {
		t.Fatalf("read repaired Inbox attempt: %v", err)
	}
	if jobStatus != "queued" {
		t.Fatalf("repaired Inbox attempt status = %q, want queued", jobStatus)
	}
}

func TestInboxOrphanRepairConcurrentReplicasClaimDisjointBatches(t *testing.T) {
	pool := StartPostgres(t)
	type orphan struct {
		inboxID  uuid.UUID
		jobID    uuid.UUID
		revision int64
	}
	orphans := make([]orphan, 0, 3)
	for index, status := range []string{"queued", "running", "queued"} {
		inboxID, jobID, revision := insertInboxDispatchOrphan(t, pool, fmt.Sprintf("replica-%d", index), status)
		orphans = append(orphans, orphan{inboxID: inboxID, jobID: jobID, revision: revision})
	}
	queue := newInboxRiverQueue(t, pool)
	commands := []*durablework.InboxCommands{
		dbInboxCommands(pool, repository.NewPGXReaderVNextRepository(pool), queue),
		dbInboxCommands(pool, repository.NewPGXReaderVNextRepository(pool), queue),
	}
	type outcome struct {
		result service.InboxProposalOrphanRepairResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, len(commands))
	for _, command := range commands {
		command := command
		go func() {
			<-start
			result, err := command.RepairInboxProposalOrphans(t.Context(), 2)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	var repaired int
	for range commands {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatalf("concurrent RepairInboxProposalOrphans() error = %v", outcome.err)
		}
		if outcome.result.Repaired == 0 || outcome.result.Repaired > 2 {
			t.Fatalf("replica result = %+v, want one bounded disjoint batch", outcome.result)
		}
		repaired += outcome.result.Repaired
	}
	if repaired != len(orphans) {
		t.Fatalf("total repaired = %d, want %d", repaired, len(orphans))
	}
	for _, orphan := range orphans {
		if got := countActiveInboxRiverJobs(t, pool, orphan.inboxID, orphan.jobID, orphan.revision); got != 1 {
			t.Fatalf("active River jobs for attempt %s = %d, want exactly 1", orphan.jobID, got)
		}
		var status string
		var startedAt *time.Time
		if err := pool.QueryRow(t.Context(), `SELECT status,started_at FROM reader_inbox_jobs WHERE id=$1`, orphan.jobID).
			Scan(&status, &startedAt); err != nil {
			t.Fatalf("read repaired attempt %s: %v", orphan.jobID, err)
		}
		if status != "queued" || startedAt != nil {
			t.Fatalf("repaired attempt %s = status %q started_at %v, want queued/nil", orphan.jobID, status, startedAt)
		}
	}
}

func TestInboxOrphanRepairRollsBackRealRiverInsertAndRetriesNextPass(t *testing.T) {
	pool := StartPostgres(t)
	inboxID, jobID, revision := insertInboxDispatchOrphan(t, pool, "rollback-retry", "queued")
	realQueue := newInboxRiverQueue(t, pool)
	wantErr := errors.New("fail after real Inbox River insert")
	failingQueue := &failAfterInboxQueue{delegate: realQueue, err: wantErr}
	failingCommands := dbInboxCommands(pool, repository.NewPGXReaderVNextRepository(pool), failingQueue)

	result, err := failingCommands.RepairInboxProposalOrphans(t.Context(), 10)
	if !errors.Is(err, wantErr) {
		t.Fatalf("RepairInboxProposalOrphans() error = %v, want %v", err, wantErr)
	}
	if result.Claimed != 1 || result.Repaired != 0 || failingQueue.callCount() != 1 {
		t.Fatalf("failed repair = %+v, real insert calls = %d", result, failingQueue.callCount())
	}
	if got := countActiveInboxRiverJobs(t, pool, inboxID, jobID, revision); got != 0 {
		t.Fatalf("active River jobs after rollback = %d, want 0", got)
	}
	var status string
	if err := pool.QueryRow(t.Context(), `SELECT status FROM reader_inbox_jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
		t.Fatalf("read rolled-back orphan: %v", err)
	}
	if status != "queued" {
		t.Fatalf("orphan status after rollback = %q, want queued", status)
	}

	healthyCommands := dbInboxCommands(pool, repository.NewPGXReaderVNextRepository(pool), realQueue)
	retry, err := healthyCommands.RepairInboxProposalOrphans(t.Context(), 10)
	if err != nil || retry.Repaired != 1 {
		t.Fatalf("healthy retry = (%+v, %v), want one repair", retry, err)
	}
	if got := countActiveInboxRiverJobs(t, pool, inboxID, jobID, revision); got != 1 {
		t.Fatalf("active River jobs after retry = %d, want 1", got)
	}
}

func TestInboxDurableProductionEntrypointsRollbackAfterRealRiverInsert(t *testing.T) {
	for _, entrypoint := range []string{"Create", "Submit", "Batch", "Ingest", "resummarize"} {
		entrypoint := entrypoint
		t.Run(entrypoint, func(t *testing.T) {
			pool := StartPostgres(t)
			readerRepo := repository.NewPGXReaderVNextRepository(pool)
			realQueue := newInboxRiverQueue(t, pool)
			wantErr := errors.New("fail after production Inbox River insert")
			failingQueue := &failAfterInboxQueue{delegate: realQueue, err: wantErr}
			inboxCommands := dbInboxCommands(pool, readerRepo, failingQueue)

			reader := service.NewReaderVNextService(readerRepo, nil)
			reader.ConfigureReaderInboxProposalCommands(inboxCommands)
			links := repository.NewPGXLinkRepository(pool)
			jobs := repository.NewPGXJobRepository(pool)
			linkCommands := dbLinkCommands(pool, links, realQueue)
			submit, ingest := service.NewLinkServices(
				links,
				jobs,
				linkCommands,
				urllock.NewInProcessURLLocker(),
				service.SubmitServiceOptions{
					InboxWriter:           readerRepo,
					InboxProposalCommands: inboxCommands,
				},
			)

			url := "https://production-inbox.example/" + entrypoint
			wantInboxRows := 0
			switch entrypoint {
			case "Create":
				_, err := reader.CreateInbox(t.Context(), dto.ReaderInboxCreateRequest{URL: url, Body: "body"})
				if err == nil {
					t.Fatal("CreateInbox() error = nil, want post-insert failure")
				}
			case "Submit":
				_, err := submit.Submit(t.Context(), dto.LinkCreateRequest{URL: url, Destination: "inbox"})
				if err == nil {
					t.Fatal("Submit() error = nil, want post-insert failure")
				}
			case "Batch":
				response, err := submit.Batch(t.Context(), dto.BatchCreateRequest{Items: []dto.LinkCreateRequest{{URL: url, Destination: "inbox"}}})
				if err != nil {
					t.Fatalf("Batch() top-level error = %v, want per-item failure", err)
				}
				if len(response.Results) != 1 || response.Results[0].Error == "" || response.Results[0].Result != nil {
					t.Fatalf("Batch() response = %+v, want one item error", response)
				}
			case "Ingest":
				_, err := ingest.Ingest(t.Context(), dto.IngestRequest{
					Destination: "inbox",
					Sources: []dto.IngestSource{{
						Kind: "browser_capture", URL: url, Title: "Captured", Text: "body",
					}},
				})
				if err == nil {
					t.Fatal("Ingest() error = nil, want post-insert failure")
				}
			case "resummarize":
				item, err := readerRepo.CreateInbox(t.Context(), model.ReaderInbox{
					URL: url, IdentityKey: url, SourceKind: "url", Body: "body", ProposalStatus: "pending",
				})
				if err != nil {
					t.Fatalf("preload Inbox for resummarize: %v", err)
				}
				wantInboxRows = 1
				if _, err := reader.ResummarizeInboxJob(t.Context(), item.ID.String()); err == nil {
					t.Fatal("ResummarizeInboxJob() error = nil, want post-insert failure")
				}
			}

			if failingQueue.callCount() != 1 {
				t.Fatalf("real Inbox InsertTx calls = %d, want 1", failingQueue.callCount())
			}
			var inboxRows, jobRows, riverRows int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_inbox`).Scan(&inboxRows); err != nil {
				t.Fatalf("count reader_inbox: %v", err)
			}
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_inbox_jobs`).Scan(&jobRows); err != nil {
				t.Fatalf("count reader_inbox_jobs: %v", err)
			}
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM river_job WHERE kind=$1`, service.ReaderInboxSummaryJobKind).Scan(&riverRows); err != nil {
				t.Fatalf("count Inbox river_job: %v", err)
			}
			if inboxRows != wantInboxRows || jobRows != 0 || riverRows != 0 {
				t.Fatalf("rows after %s rollback = inbox %d jobs %d River %d, want %d/0/0",
					entrypoint, inboxRows, jobRows, riverRows, wantInboxRows)
			}
		})
	}
}
