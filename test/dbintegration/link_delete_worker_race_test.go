package dbintegration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

// TestDurableLinkDeleteSerializesWithWorkerTerminalCompletion fixes both
// possible commit orders around a parser's terminal write. The blocker holds
// only the Link row; whichever product operation enters PostgreSQL's lock
// queue first must finish first after the blocker commits.
func TestDurableLinkDeleteSerializesWithWorkerTerminalCompletion(t *testing.T) {
	pool := StartPostgres(t)
	setupRepo := repository.NewPGXLinkRepository(pool)
	setupQueue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	setupCommands := dbLinkCommands(pool, setupRepo, setupQueue)

	t.Run("worker completion commits before delete", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		fixture := newDeleteWorkerRaceFixture(
			t,
			ctx,
			pool,
			setupRepo,
			setupCommands,
			"https://race.example.com/worker-before-delete",
		)

		workerApplication := "link_delete_race_worker_first"
		deleteApplication := "link_delete_race_delete_second"
		workerPool := openNamedPool(t, workerApplication)
		deletePool := openNamedPool(t, deleteApplication)
		workerRepo := repository.NewPGXLinkRepository(workerPool)
		deleteCommands := dbLinkCommands(
			deletePool,
			repository.NewPGXLinkRepository(deletePool),
			newRiverQueue(t, deletePool, newRecordingProcessor(deletePool)),
		)

		blocker := lockDeleteWorkerRaceLink(t, ctx, pool, fixture.linkID)
		defer func() { _ = blocker.Rollback(context.Background()) }()

		workerDone := make(chan error, 1)
		go func() {
			workerDone <- workerRepo.CompleteParse(ctx, fixture.completion("worker committed"), fixture.jobID)
		}()
		waitForPostgresLock(t, ctx, pool, workerApplication)

		deleteDone := make(chan error, 1)
		go func() {
			deleteDone <- deleteCommands.DeleteLink(ctx, service.DeleteLinkCommand{LinkID: fixture.linkID})
		}()
		waitForPostgresLock(t, ctx, pool, deleteApplication)

		if err := blocker.Commit(ctx); err != nil {
			t.Fatalf("release Link blocker: %v", err)
		}
		if err := <-workerDone; err != nil {
			t.Fatalf("CompleteParse() before delete: %v", err)
		}
		if err := <-deleteDone; err != nil {
			t.Fatalf("DeleteLink() after completion: %v", err)
		}

		assertDeleteWorkerRaceState(t, pool, fixture, deleteWorkerRaceWant{
			jobStatus: model.JobStatusDone,
			title:     "worker committed",
		})
	})

	t.Run("delete commits before stale worker completion", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		fixture := newDeleteWorkerRaceFixture(
			t,
			ctx,
			pool,
			setupRepo,
			setupCommands,
			"https://race.example.com/delete-before-worker",
		)

		deleteApplication := "link_delete_race_delete_first"
		workerApplication := "link_delete_race_worker_second"
		deletePool := openNamedPool(t, deleteApplication)
		workerPool := openNamedPool(t, workerApplication)
		deleteCommands := dbLinkCommands(
			deletePool,
			repository.NewPGXLinkRepository(deletePool),
			newRiverQueue(t, deletePool, newRecordingProcessor(deletePool)),
		)
		workerRepo := repository.NewPGXLinkRepository(workerPool)

		blocker := lockDeleteWorkerRaceLink(t, ctx, pool, fixture.linkID)
		defer func() { _ = blocker.Rollback(context.Background()) }()

		deleteDone := make(chan error, 1)
		go func() {
			deleteDone <- deleteCommands.DeleteLink(ctx, service.DeleteLinkCommand{LinkID: fixture.linkID})
		}()
		waitForPostgresLock(t, ctx, pool, deleteApplication)

		workerDone := make(chan error, 1)
		go func() {
			workerDone <- workerRepo.CompleteParse(ctx, fixture.completion("stale overwrite"), fixture.jobID)
		}()
		waitForPostgresLock(t, ctx, pool, workerApplication)

		if err := blocker.Commit(ctx); err != nil {
			t.Fatalf("release Link blocker: %v", err)
		}
		if err := <-deleteDone; err != nil {
			t.Fatalf("DeleteLink() before completion: %v", err)
		}
		workerErr := <-workerDone
		if !errors.Is(workerErr, repository.ErrNotFound) && !errors.Is(workerErr, repository.ErrParseJobNotRunnable) {
			t.Fatalf("stale CompleteParse() error = %v, want ErrNotFound or ErrParseJobNotRunnable", workerErr)
		}

		assertDeleteWorkerRaceState(t, pool, fixture, deleteWorkerRaceWant{
			jobStatus: model.JobStatusFailed,
			jobReason: "link_deleted",
		})
	})
}

type deleteWorkerRaceFixture struct {
	linkID                   uuid.UUID
	jobID                    uuid.UUID
	expectedMetadataRevision int64
}

func newDeleteWorkerRaceFixture(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repo *repository.PGXLinkRepository,
	commands interface {
		SubmitLink(context.Context, service.SubmitLinkCommand) (service.LinkSubmissionResult, error)
	},
	rawURL string,
) deleteWorkerRaceFixture {
	t.Helper()
	result, err := commands.SubmitLink(ctx, service.SubmitLinkCommand{Capture: service.LinkCapture{
		URL: rawURL, Status: model.LinkStatusPending,
	}})
	if err != nil || result.Link == nil || result.Job == nil {
		t.Fatalf("SubmitLink() = %#v, %v", result, err)
	}
	if err := repo.MarkParseProcessing(ctx, result.Link.ID, result.Job.ID); err != nil {
		t.Fatalf("MarkParseProcessing(): %v", err)
	}
	assertActiveRiverAttempt(t, pool, result.Job.ID)
	return deleteWorkerRaceFixture{
		linkID:                   result.Link.ID,
		jobID:                    result.Job.ID,
		expectedMetadataRevision: result.Job.ExpectedMetadataRevision,
	}
}

func (f deleteWorkerRaceFixture) completion(title string) repository.UpdateLinkAnalysisParams {
	return repository.UpdateLinkAnalysisParams{
		ID:                       f.linkID,
		ExpectedMetadataRevision: f.expectedMetadataRevision,
		Title:                    &title,
		Tags:                     []string{},
		Status:                   model.LinkStatusDone,
	}
}

func lockDeleteWorkerRaceLink(t *testing.T, ctx context.Context, pool *pgxpool.Pool, linkID uuid.UUID) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin Link blocker: %v", err)
	}
	var lockedID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM links WHERE id=$1 FOR UPDATE`, linkID).Scan(&lockedID); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("lock Link: %v", err)
	}
	return tx
}

type deleteWorkerRaceWant struct {
	jobStatus model.JobStatus
	jobReason string
	title     string
}

func assertDeleteWorkerRaceState(
	t *testing.T,
	pool *pgxpool.Pool,
	fixture deleteWorkerRaceFixture,
	want deleteWorkerRaceWant,
) {
	t.Helper()
	var (
		trashed    bool
		title      *string
		jobStatus  model.JobStatus
		jobReason  string
		riverState string
		cancelled  bool
	)
	if err := pool.QueryRow(t.Context(), `SELECT deleted_at IS NOT NULL,title FROM links WHERE id=$1`, fixture.linkID).
		Scan(&trashed, &title); err != nil {
		t.Fatalf("read final Link state: %v", err)
	}
	if !trashed {
		t.Fatal("final Link is live, want Trash")
	}
	if want.title == "" {
		if title != nil {
			t.Fatalf("final Link title = %q, want NULL after rejected stale completion", *title)
		}
	} else if title == nil || *title != want.title {
		t.Fatalf("final Link title = %v, want %q", title, want.title)
	}
	if err := pool.QueryRow(t.Context(), `SELECT status,COALESCE(error_msg,'') FROM parse_jobs WHERE id=$1`, fixture.jobID).
		Scan(&jobStatus, &jobReason); err != nil {
		t.Fatalf("read final parse attempt: %v", err)
	}
	if jobStatus != want.jobStatus || jobReason != want.jobReason {
		t.Fatalf("final parse attempt = %s/%q, want %s/%q", jobStatus, jobReason, want.jobStatus, want.jobReason)
	}
	if err := pool.QueryRow(t.Context(), `SELECT state::text,metadata ? 'cancel_attempted_at'
		FROM river_job WHERE kind='parse_link' AND args->>'parse_job_id'=$1`, fixture.jobID.String()).
		Scan(&riverState, &cancelled); err != nil {
		t.Fatalf("read final River attempt: %v", err)
	}
	if riverState != "cancelled" || !cancelled {
		t.Fatalf("final River attempt = state:%q cancel_attempted:%v, want cancelled/true", riverState, cancelled)
	}
}
