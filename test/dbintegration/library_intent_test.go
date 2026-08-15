package dbintegration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

func TestActiveRequestedLibraryIntentUpdateReusesProcessingAttempt(t *testing.T) {
	pool := StartPostgres(t)
	linkID, jobID := insertPendingLinkAndJob(t, pool, "https://example.com/active-library-intent")
	ctx := t.Context()
	repo := repository.NewPGXLinkRepository(pool)
	if err := repo.MarkParseProcessing(ctx, linkID, jobID); err != nil {
		t.Fatalf("MarkParseProcessing(): %v", err)
	}

	commands := dbLinkCommands(pool, repo, &countingSubmitQueue{})
	result, err := commands.UpdateLinkIntent(ctx, service.UpdateLinkIntentCommand{
		LinkID: linkID, Kind: model.RequestedLibraryKindSite, Source: model.RequestedLibraryKindSourceUser,
	})
	if err != nil {
		t.Fatalf("UpdateLinkIntent(): %v", err)
	}
	if result.Status != model.LinkStatusProcessing || result.Job != nil {
		t.Fatalf("result = %#v, want processing with no replacement job", result)
	}

	assertRequestedIntent(t, pool, linkID, model.RequestedLibraryKindSite, model.RequestedLibraryKindSourceUser)
	var (
		persistedID uuid.UUID
		status      model.JobStatus
	)
	if err := pool.QueryRow(t.Context(), `SELECT id, status
		FROM parse_jobs WHERE link_id=$1`, linkID).
		Scan(&persistedID, &status); err != nil {
		t.Fatalf("read active parse attempt: %v", err)
	}
	var attemptCount int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM parse_jobs WHERE link_id=$1`, linkID).Scan(&attemptCount); err != nil {
		t.Fatalf("count active parse attempts: %v", err)
	}
	if attemptCount != 1 || persistedID != jobID || status != model.JobStatusProcessing {
		t.Fatalf("attempt = count %d id %s status %s, want original processing %s", attemptCount, persistedID, status, jobID)
	}
}

func TestIntentUpdateAfterTerminalCompletionCreatesReplacementAttempt(t *testing.T) {
	pool := StartPostgres(t)
	linkID, oldJobID := insertPendingLinkAndJob(t, pool, "https://example.com/terminal-library-intent")
	ctx := t.Context()
	repo := repository.NewPGXLinkRepository(pool)
	jobs := repository.NewPGXJobRepository(pool)
	if err := repo.MarkParseProcessing(ctx, linkID, oldJobID); err != nil {
		t.Fatalf("MarkParseProcessing(): %v", err)
	}
	completeReadingForIntent(t, repo, jobs, ctx, linkID, oldJobID,
		model.RequestedLibraryKindAuto, model.RequestedLibraryKindSourceAuto,
		model.LibraryKindSourceAuto, false)

	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	commands := dbLinkCommands(pool, repo, queue)
	result, err := commands.UpdateLinkIntent(ctx, service.UpdateLinkIntentCommand{
		LinkID: linkID, Kind: model.RequestedLibraryKindSite, Source: model.RequestedLibraryKindSourceUser,
	})
	if err != nil {
		t.Fatalf("UpdateLinkIntent(): %v", err)
	}
	if result.Status != model.LinkStatusPending || result.Job == nil || result.Job.ID == oldJobID {
		t.Fatalf("result = %#v, want pending replacement distinct from %s", result, oldJobID)
	}
	assertRequestedIntent(t, pool, linkID, model.RequestedLibraryKindSite, model.RequestedLibraryKindSourceUser)

	var oldStatus, newStatus model.JobStatus
	if err := pool.QueryRow(t.Context(), `SELECT status FROM parse_jobs WHERE id=$1`, oldJobID).Scan(&oldStatus); err != nil {
		t.Fatalf("read completed attempt: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT status FROM parse_jobs WHERE id=$1`, result.Job.ID).Scan(&newStatus); err != nil {
		t.Fatalf("read replacement attempt: %v", err)
	}
	if oldStatus != model.JobStatusDone || newStatus != model.JobStatusPending {
		t.Fatalf("attempt statuses old/new = %s/%s, want done/pending", oldStatus, newStatus)
	}
	assertActiveRiverAttempt(t, pool, result.Job.ID)
}

func TestCompletedSiteCanBeReprocessedAsUserLockedReading(t *testing.T) {
	pool := StartPostgres(t)
	linkID, siteJobID := insertPendingLinkAndJob(t, pool, "https://site-to-user-reading-intent.example/")
	ctx := t.Context()
	repo := repository.NewPGXLinkRepository(pool)
	jobs := repository.NewPGXJobRepository(pool)
	siteJob, err := jobs.GetByID(ctx, siteJobID)
	if err != nil || siteJob == nil {
		t.Fatalf("GetByID site job = %#v, %v", siteJob, err)
	}
	if err := repo.MarkParseProcessing(ctx, linkID, siteJobID); err != nil {
		t.Fatalf("MarkParseProcessing(site): %v", err)
	}

	predictedSite := model.LibraryKindSite
	siteConfidence := float32(.94)
	siteReason := "ai_site"
	siteTitle := "Site before explicit reading choice"
	siteResult, err := repo.CompleteSiteParse(ctx, repository.CompleteSiteParseParams{
		Analysis: repository.UpdateLinkAnalysisParams{
			ID: linkID, ExpectedMetadataRevision: siteJob.ExpectedMetadataRevision, Title: &siteTitle, Status: model.LinkStatusDone,
		},
		Classification: repository.UpdateLibraryClassificationParams{
			ID: linkID, Kind: model.LibraryKindSite, Source: model.LibraryKindSourceAuto,
			PredictedKind: &predictedSite, Confidence: &siteConfidence, Reason: &siteReason,
		},
		Site: repository.AggregateSiteParams{
			LinkID: linkID, IdentityKey: "v1:host:site-to-user-reading-intent.example",
			NormalizedURL: "https://site-to-user-reading-intent.example/",
			Name:          siteTitle,
			EntryName:     "Home",
		},
		ExpectedRequestedLibraryKind:       model.RequestedLibraryKindAuto,
		ExpectedRequestedLibraryKindSource: model.RequestedLibraryKindSourceAuto,
	}, siteJobID)
	if err != nil {
		t.Fatalf("CompleteSiteParse(): %v", err)
	}
	var persistedEntryID uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM site_entries
		WHERE site_id=$1 AND link_id=$2`, siteResult.SiteID, linkID).
		Scan(&persistedEntryID); err != nil {
		t.Fatalf("read completed site entry: %v", err)
	}
	if persistedEntryID != siteResult.EntryID {
		t.Fatalf("site entry = %s, want completion entry %s", persistedEntryID, siteResult.EntryID)
	}

	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	commands := dbLinkCommands(pool, repo, queue)
	intentResult, err := commands.UpdateLinkIntent(ctx, service.UpdateLinkIntentCommand{
		LinkID: linkID, Kind: model.RequestedLibraryKindReading, Source: model.RequestedLibraryKindSourceUser,
	})
	if err != nil {
		t.Fatalf("UpdateLinkIntent(reading): %v", err)
	}
	if intentResult.Status != model.LinkStatusPending || intentResult.Job == nil || intentResult.Job.ID == siteJobID {
		t.Fatalf("intent result = %#v, want pending replacement distinct from %s", intentResult, siteJobID)
	}
	readingJobID := intentResult.Job.ID
	assertRequestedIntent(t, pool, linkID, model.RequestedLibraryKindReading, model.RequestedLibraryKindSourceUser)
	assertActiveRiverAttempt(t, pool, readingJobID)
	if err := repo.MarkParseProcessing(ctx, linkID, readingJobID); err != nil {
		t.Fatalf("MarkParseProcessing(reading): %v", err)
	}

	lockedConfidence := float32(1)
	lockedReason := "user_locked"
	lockedExplanation := "user selected reading while the analyzer still predicted site"
	readingTitle := "Explicit reading choice"
	if _, err := repo.CompleteReadingParse(ctx, repository.CompleteReadingParseParams{
		Analysis: repository.UpdateLinkAnalysisParams{
			ID: linkID, ExpectedMetadataRevision: intentResult.Job.ExpectedMetadataRevision, Title: &readingTitle, Status: model.LinkStatusDone,
		},
		Classification: repository.UpdateLibraryClassificationParams{
			ID: linkID, Kind: model.LibraryKindReading, Source: model.LibraryKindSourceUser, Locked: true,
			PredictedKind: &predictedSite, Confidence: &lockedConfidence, Reason: &lockedReason,
			Explanation: &lockedExplanation,
		},
		ExpectedRequestedLibraryKind:       model.RequestedLibraryKindReading,
		ExpectedRequestedLibraryKindSource: model.RequestedLibraryKindSourceUser,
		DetachSiteEntry:                    true,
	}, readingJobID); err != nil {
		t.Fatalf("CompleteReadingParse(): %v", err)
	}

	var entryCount int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM site_entries
		WHERE link_id=$1`, linkID).Scan(&entryCount); err != nil {
		t.Fatalf("count detached site entries: %v", err)
	}
	if entryCount != 0 {
		t.Fatalf("site entry count = %d, want 0 after reading completion", entryCount)
	}

	var (
		linkStatus       model.LinkStatus
		requestedKind    model.RequestedLibraryKind
		requestedSource  model.RequestedLibraryKindSource
		kind             model.LibraryKind
		source           model.LibraryKindSource
		locked           bool
		predictedStored  model.LibraryKind
		reasonStored     string
		siteJobStatus    model.JobStatus
		readingJobStatus model.JobStatus
	)
	if err := pool.QueryRow(t.Context(), `SELECT l.status, l.requested_library_kind,
		l.requested_library_kind_source, l.library_kind, l.library_kind_source,
		l.library_kind_locked, l.predicted_library_kind, l.classification_reason,
		old_job.status, new_job.status
		FROM links l
		JOIN parse_jobs old_job ON old_job.id=$2
		JOIN parse_jobs new_job ON new_job.id=$3
		WHERE l.id=$1`, linkID, siteJobID, readingJobID).
		Scan(&linkStatus, &requestedKind, &requestedSource, &kind, &source, &locked,
			&predictedStored, &reasonStored, &siteJobStatus, &readingJobStatus); err != nil {
		t.Fatalf("read completed reading state: %v", err)
	}
	if linkStatus != model.LinkStatusDone || siteJobStatus != model.JobStatusDone || readingJobStatus != model.JobStatusDone {
		t.Fatalf("link/old job/new job status = %s/%s/%s, want done/done/done",
			linkStatus, siteJobStatus, readingJobStatus)
	}
	if requestedKind != model.RequestedLibraryKindReading || requestedSource != model.RequestedLibraryKindSourceUser ||
		kind != model.LibraryKindReading || source != model.LibraryKindSourceUser || !locked ||
		predictedStored != model.LibraryKindSite || reasonStored != lockedReason {
		t.Fatalf("final intent/kind/source/locked/predicted/reason = %s/%s %s/%s/%v/%s/%s",
			requestedKind, requestedSource, kind, source, locked, predictedStored, reasonStored)
	}
}

func TestConcurrentIntentCommitMakesStaleCompletionRetryLatestChoice(t *testing.T) {
	pool := StartPostgres(t)
	linkID, jobID := insertPendingLinkAndJob(t, pool, "https://concurrent-library-intent.example/")
	ctx := t.Context()
	repo := repository.NewPGXLinkRepository(pool)
	jobs := repository.NewPGXJobRepository(pool)
	if err := repo.MarkParseProcessing(ctx, linkID, jobID); err != nil {
		t.Fatalf("MarkParseProcessing(): %v", err)
	}

	intentTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin intent transaction: %v", err)
	}
	defer func() { _ = intentTx.Rollback(context.Background()) }()
	if _, err := repo.UpdateRequestedLibraryIntentTx(ctx, intentTx, repository.UpdateRequestedLibraryIntentParams{
		ID: linkID, Kind: model.RequestedLibraryKindSite, Source: model.RequestedLibraryKindSourceUser,
	}); err != nil {
		t.Fatalf("stage requested intent: %v", err)
	}

	completionPool, completionPID := oneConnectionPool(t, DSN(t))
	completionRepo := repository.NewPGXLinkRepository(completionPool)
	completionJobs := repository.NewPGXJobRepository(completionPool)
	completionErr := make(chan error, 1)
	go func() {
		completionErr <- completeReadingForIntentResult(
			completionRepo, completionJobs, context.Background(), linkID, jobID,
			model.RequestedLibraryKindAuto, model.RequestedLibraryKindSourceAuto,
			model.LibraryKindSourceAuto, false,
		)
	}()
	waitForBackendRowLock(t, pool, completionPID)
	if err := intentTx.Commit(ctx); err != nil {
		t.Fatalf("commit requested intent: %v", err)
	}

	select {
	case err := <-completionErr:
		if !errors.Is(err, repository.ErrLibraryIntentChanged) {
			t.Fatalf("stale completion error = %v, want ErrLibraryIntentChanged", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stale completion did not unblock after intent commit")
	}

	predicted := model.LibraryKindReading
	confidence := float32(.81)
	reason := "ai_reading"
	title := "Latest committed intent"
	job, err := jobs.GetByID(ctx, jobID)
	if err != nil || job == nil {
		t.Fatalf("GetByID retry job = %#v, %v", job, err)
	}
	_, err = repo.CompleteSiteParse(ctx, repository.CompleteSiteParseParams{
		Analysis: repository.UpdateLinkAnalysisParams{
			ID: linkID, ExpectedMetadataRevision: job.ExpectedMetadataRevision, Title: &title, Status: model.LinkStatusDone,
		},
		Classification: repository.UpdateLibraryClassificationParams{
			ID: linkID, Kind: model.LibraryKindSite, Source: model.LibraryKindSourceUser, Locked: true,
			PredictedKind: &predicted, Confidence: &confidence, Reason: &reason,
		},
		Site: repository.AggregateSiteParams{
			LinkID: linkID, IdentityKey: "v1:host:concurrent-library-intent.example",
			NormalizedURL: "https://concurrent-library-intent.example/", Name: "Latest committed intent",
			EntryName: "Home",
		},
		ExpectedRequestedLibraryKind:       model.RequestedLibraryKindSite,
		ExpectedRequestedLibraryKindSource: model.RequestedLibraryKindSourceUser,
	}, jobID)
	if err != nil {
		t.Fatalf("retry latest site completion: %v", err)
	}

	var (
		linkStatus      model.LinkStatus
		kind            model.LibraryKind
		source          model.LibraryKindSource
		locked          bool
		predictedStored model.LibraryKind
		jobStatus       model.JobStatus
	)
	if err := pool.QueryRow(t.Context(), `SELECT l.status, l.library_kind, l.library_kind_source,
		l.library_kind_locked, l.predicted_library_kind, j.status
		FROM links l JOIN parse_jobs j ON j.link_id=l.id AND j.id=$2
		WHERE l.id=$1`, linkID, jobID).
		Scan(&linkStatus, &kind, &source, &locked, &predictedStored, &jobStatus); err != nil {
		t.Fatalf("read retried terminal state: %v", err)
	}
	if linkStatus != model.LinkStatusDone || jobStatus != model.JobStatusDone ||
		kind != model.LibraryKindSite || source != model.LibraryKindSourceUser || !locked ||
		predictedStored != model.LibraryKindReading {
		t.Fatalf("terminal state = link/job %s/%s kind/source/locked/predicted %s/%s/%v/%s",
			linkStatus, jobStatus, kind, source, locked, predictedStored)
	}
}

func TestAutomaticRequeueAndRetryPreserveCommittedUserIntent(t *testing.T) {
	pool := StartPostgres(t)
	linkID, _ := insertPendingLinkAndJob(t, pool, "https://example.com/retry-library-intent")
	ctx := t.Context()
	repo := repository.NewPGXLinkRepository(pool)
	predicted := model.LibraryKindSite
	if err := repo.UpdateLibraryClassification(ctx, repository.UpdateLibraryClassificationParams{
		ID: linkID, Kind: model.LibraryKindSite, Source: model.LibraryKindSourceUser,
		Locked: true, PredictedKind: &predicted,
	}); err != nil {
		t.Fatalf("seed explicit site classification: %v", err)
	}

	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	commands := dbLinkCommands(pool, repo, queue)
	for _, command := range []service.RequeueLinkCommand{
		{LinkID: linkID},
		{LinkID: linkID, Capture: &service.LinkCapture{
			URL: "https://example.com/retry-library-intent", SourceKind: "rss",
			SourceKey: "https://example.com/retry-library-intent", Status: model.LinkStatusPending,
			RequestedLibraryKind:       model.RequestedLibraryKindReading,
			RequestedLibraryKindSource: model.RequestedLibraryKindSourceAuto,
		}},
	} {
		if _, err := commands.RequeueLink(ctx, command); err != nil {
			t.Fatalf("RequeueLink(%#v): %v", command.Capture, err)
		}
		assertRequestedIntent(t, pool, linkID, model.RequestedLibraryKindSite, model.RequestedLibraryKindSourceUser)
	}
}

func completeReadingForIntent(
	t *testing.T,
	repo *repository.PGXLinkRepository,
	jobs *repository.PGXJobRepository,
	ctx context.Context,
	linkID, jobID uuid.UUID,
	expectedKind model.RequestedLibraryKind,
	expectedSource model.RequestedLibraryKindSource,
	source model.LibraryKindSource,
	locked bool,
) {
	t.Helper()
	if err := completeReadingForIntentResult(repo, jobs, ctx, linkID, jobID, expectedKind, expectedSource, source, locked); err != nil {
		t.Fatalf("CompleteReadingParse(): %v", err)
	}
}

func completeReadingForIntentResult(
	repo *repository.PGXLinkRepository,
	jobs *repository.PGXJobRepository,
	ctx context.Context,
	linkID, jobID uuid.UUID,
	expectedKind model.RequestedLibraryKind,
	expectedSource model.RequestedLibraryKindSource,
	source model.LibraryKindSource,
	locked bool,
) error {
	job, err := jobs.GetByID(ctx, jobID)
	if err != nil {
		return err
	}
	if job == nil {
		return repository.ErrNotFound
	}
	predicted := model.LibraryKindReading
	title := "Intent completion"
	_, err = repo.CompleteReadingParse(ctx, repository.CompleteReadingParseParams{
		Analysis: repository.UpdateLinkAnalysisParams{ID: linkID, ExpectedMetadataRevision: job.ExpectedMetadataRevision, Title: &title, Status: model.LinkStatusDone},
		Classification: repository.UpdateLibraryClassificationParams{
			ID: linkID, Kind: model.LibraryKindReading, Source: source, Locked: locked,
			PredictedKind: &predicted,
		},
		ExpectedRequestedLibraryKind:       expectedKind,
		ExpectedRequestedLibraryKindSource: expectedSource,
	}, jobID)
	return err
}

func assertRequestedIntent(
	t *testing.T,
	pool *pgxpool.Pool,
	linkID uuid.UUID,
	wantKind model.RequestedLibraryKind,
	wantSource model.RequestedLibraryKindSource,
) {
	t.Helper()
	var kind model.RequestedLibraryKind
	var source model.RequestedLibraryKindSource
	if err := pool.QueryRow(t.Context(), `SELECT requested_library_kind, requested_library_kind_source
		FROM links WHERE id=$1`, linkID).Scan(&kind, &source); err != nil {
		t.Fatalf("read requested intent: %v", err)
	}
	if kind != wantKind || source != wantSource {
		t.Fatalf("requested intent = %s/%s, want %s/%s", kind, source, wantKind, wantSource)
	}
}

func oneConnectionPool(t *testing.T, dsn string) (*pgxpool.Pool, int32) {
	t.Helper()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse completion pool config: %v", err)
	}
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("open completion pool: %v", err)
	}
	t.Cleanup(pool.Close)
	connection, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire completion connection: %v", err)
	}
	defer connection.Release()
	var pid int32
	if err := connection.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read completion backend pid: %v", err)
	}
	return pool, pid
}

func waitForBackendRowLock(t *testing.T, pool *pgxpool.Pool, pid int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := pool.QueryRow(t.Context(), `SELECT wait_event_type = 'Lock'
			FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&waiting)
		if err == nil && waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("backend %d did not block on the held link row", pid)
}
