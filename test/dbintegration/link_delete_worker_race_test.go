package dbintegration

import (
	"context"
	"errors"
	"fmt"
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
// commit orders around a parser's terminal write. The blocker holds only the
// Link row; whichever product operation enters PostgreSQL's lock queue first
// must finish first after the blocker commits.
func TestDurableLinkDeleteSerializesWithWorkerTerminalCompletion(t *testing.T) {
	pool := StartPostgres(t)
	setupRepo := repository.NewPGXLinkRepository(pool)
	setupQueue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	setupCommands := dbLinkCommands(pool, setupRepo, setupQueue)

	t.Run("worker completion commits before delete", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		fixture := newDeleteWorkerRaceFixture(
			t, ctx, pool, setupRepo, setupCommands,
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

		blocker := lockDeleteWorkerRaceLink(t, ctx, pool, fixture.attempt.LinkID)
		defer func() { _ = blocker.Rollback(context.Background()) }()

		workerDone := make(chan error, 1)
		go func() {
			workerDone <- completeDeleteWorkerRace(workerRepo, ctx, fixture, "worker committed")
		}()
		waitForPostgresLock(t, ctx, pool, workerApplication)

		deleteDone := make(chan error, 1)
		go func() {
			deleteDone <- deleteCommands.DeleteLink(ctx, service.DeleteLinkCommand{LinkID: fixture.attempt.LinkID})
		}()
		waitForPostgresLock(t, ctx, pool, deleteApplication)

		if err := blocker.Commit(ctx); err != nil {
			t.Fatalf("release Link blocker: %v", err)
		}
		if err := <-workerDone; err != nil {
			t.Fatalf("CompleteReadingParse() before delete: %v", err)
		}
		if err := <-deleteDone; err != nil {
			t.Fatalf("DeleteLink() after completion: %v", err)
		}

		assertDeleteWorkerRaceState(t, pool, fixture, "worker committed")
	})

	t.Run("delete commits before stale worker completion", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		fixture := newDeleteWorkerRaceFixture(
			t, ctx, pool, setupRepo, setupCommands,
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

		blocker := lockDeleteWorkerRaceLink(t, ctx, pool, fixture.attempt.LinkID)
		defer func() { _ = blocker.Rollback(context.Background()) }()

		deleteDone := make(chan error, 1)
		go func() {
			deleteDone <- deleteCommands.DeleteLink(ctx, service.DeleteLinkCommand{LinkID: fixture.attempt.LinkID})
		}()
		waitForPostgresLock(t, ctx, pool, deleteApplication)

		workerDone := make(chan error, 1)
		go func() {
			workerDone <- completeDeleteWorkerRace(workerRepo, ctx, fixture, "stale overwrite")
		}()
		waitForPostgresLock(t, ctx, pool, workerApplication)

		if err := blocker.Commit(ctx); err != nil {
			t.Fatalf("release Link blocker: %v", err)
		}
		if err := <-deleteDone; err != nil {
			t.Fatalf("DeleteLink() before completion: %v", err)
		}
		if workerErr := <-workerDone; !errors.Is(workerErr, repository.ErrParseAttemptNotRunnable) {
			t.Fatalf("stale CompleteReadingParse() error = %v, want ErrParseAttemptNotRunnable", workerErr)
		}

		assertDeleteWorkerRaceState(t, pool, fixture, "")
	})
}

type deleteWorkerRaceFixture struct {
	attempt model.ParseAttempt
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
	if err != nil || result.Link == nil || !result.Enqueued {
		t.Fatalf("SubmitLink() = %#v, %v", result, err)
	}
	attempt := parseAttemptForLink(result.Link)
	if err := repo.MarkParseProcessing(ctx, attempt); err != nil {
		t.Fatalf("MarkParseProcessing(): %v", err)
	}
	assertActiveRiverAttempt(t, pool, attempt)
	return deleteWorkerRaceFixture{attempt: attempt}
}

func completeDeleteWorkerRace(
	repo *repository.PGXLinkRepository,
	ctx context.Context,
	fixture deleteWorkerRaceFixture,
	title string,
) error {
	_, err := repo.CompleteReadingParse(ctx, repository.CompleteReadingParseParams{
		Analysis: repository.UpdateLinkAnalysisParams{
			ID: fixture.attempt.LinkID, ExpectedParseGeneration: fixture.attempt.Generation,
			ExpectedMetadataRevision: fixture.attempt.ExpectedMetadataRevision,
			Title:                    &title, Tags: []string{}, Status: model.LinkStatusDone,
		},
		Classification: repository.UpdateLibraryClassificationParams{
			ID: fixture.attempt.LinkID, Kind: model.LibraryKindReading,
		},
	})
	return err
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

func assertDeleteWorkerRaceState(
	t *testing.T,
	pool *pgxpool.Pool,
	fixture deleteWorkerRaceFixture,
	wantTitle string,
) {
	t.Helper()
	var (
		trashed    bool
		title      *string
		riverState string
		cancelled  bool
	)
	if err := pool.QueryRow(t.Context(), `SELECT deleted_at IS NOT NULL,title FROM links WHERE id=$1`, fixture.attempt.LinkID).
		Scan(&trashed, &title); err != nil {
		t.Fatalf("read final Link state: %v", err)
	}
	if !trashed {
		t.Fatal("final Link is live, want Trash")
	}
	if wantTitle == "" {
		if title != nil {
			t.Fatalf("final Link title = %q, want NULL after rejected stale completion", *title)
		}
	} else if title == nil || *title != wantTitle {
		t.Fatalf("final Link title = %v, want %q", title, wantTitle)
	}
	if err := pool.QueryRow(t.Context(), `SELECT state::text,metadata ? 'cancel_attempted_at'
		FROM river_job WHERE kind='parse_link' AND args->>'link_id'=$1 AND args->>'parse_generation'=$2`,
		fixture.attempt.LinkID.String(), fmt.Sprint(fixture.attempt.Generation)).Scan(&riverState, &cancelled); err != nil {
		t.Fatalf("read final River attempt: %v", err)
	}
	if riverState != "cancelled" || !cancelled {
		t.Fatalf("final River attempt = state:%q cancel_attempted:%v, want cancelled/true", riverState, cancelled)
	}
}
