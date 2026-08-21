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

func TestActiveLibrarySelectionReusesProcessingAttempt(t *testing.T) {
	pool := StartPostgres(t)
	linkID, attempt := insertPendingLinkAttempt(t, pool, "https://example.com/active-library-selection")
	ctx := t.Context()
	repo := repository.NewPGXLinkRepository(pool)
	if err := repo.MarkParseProcessing(ctx, attempt); err != nil {
		t.Fatalf("MarkParseProcessing(): %v", err)
	}

	queue := &countingSubmitQueue{}
	result, err := dbLinkCommands(pool, repo, queue).SetLinkLibraryKind(ctx, service.SetLinkLibraryKindCommand{
		LinkID: linkID, Kind: model.LibraryKindSite, Override: true,
	})
	if err != nil {
		t.Fatalf("SetLinkLibraryKind(): %v", err)
	}
	if result.Status != model.LinkStatusProcessing {
		t.Fatalf("result = %#v, want processing", result)
	}
	if got := queue.enqueueTxCalls.Load(); got != 0 {
		t.Fatalf("replacement enqueue calls = %d, want 0", got)
	}
	assertLibrarySelection(t, pool, linkID, model.LibraryKindSite, true)

	current, err := repo.GetByID(ctx, linkID)
	if err != nil || current == nil {
		t.Fatalf("GetByID() = %#v, %v", current, err)
	}
	if current.Status != model.LinkStatusProcessing || current.ParseGeneration != attempt.Generation {
		t.Fatalf("active Link = status %s generation %d, want processing generation %d",
			current.Status, current.ParseGeneration, attempt.Generation)
	}
}

func TestLibrarySelectionAfterTerminalCompletionCreatesReplacementAttempt(t *testing.T) {
	pool := StartPostgres(t)
	linkID, oldAttempt := insertPendingLinkAttempt(t, pool, "https://example.com/terminal-library-selection")
	ctx := t.Context()
	repo := repository.NewPGXLinkRepository(pool)
	if err := repo.MarkParseProcessing(ctx, oldAttempt); err != nil {
		t.Fatalf("MarkParseProcessing(): %v", err)
	}
	if err := completeReadingSelection(repo, ctx, oldAttempt, nil, false, false); err != nil {
		t.Fatalf("CompleteReadingParse(): %v", err)
	}

	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	result, err := dbLinkCommands(pool, repo, queue).SetLinkLibraryKind(ctx, service.SetLinkLibraryKindCommand{
		LinkID: linkID, Kind: model.LibraryKindSite, Override: true,
	})
	if err != nil {
		t.Fatalf("SetLinkLibraryKind(): %v", err)
	}
	if result.Status != model.LinkStatusPending {
		t.Fatalf("result = %#v, want pending replacement", result)
	}
	assertLibrarySelection(t, pool, linkID, model.LibraryKindSite, true)

	current, err := repo.GetByID(ctx, linkID)
	if err != nil || current == nil {
		t.Fatalf("GetByID(replacement) = %#v, %v", current, err)
	}
	replacement := parseAttemptForLink(current)
	if replacement.Generation != oldAttempt.Generation+1 {
		t.Fatalf("replacement generation = %d, want %d", replacement.Generation, oldAttempt.Generation+1)
	}
	assertActiveRiverAttempt(t, pool, replacement)
}

func TestCompletedSiteCanBeReprocessedAsLockedReading(t *testing.T) {
	pool := StartPostgres(t)
	linkID, siteAttempt := insertPendingLinkAttempt(t, pool, "https://site-to-reading-selection.example/")
	ctx := t.Context()
	repo := repository.NewPGXLinkRepository(pool)
	if err := repo.MarkParseProcessing(ctx, siteAttempt); err != nil {
		t.Fatalf("MarkParseProcessing(site): %v", err)
	}

	siteTitle := "Site before reading choice"
	siteResult, err := repo.CompleteSiteParse(ctx, repository.CompleteSiteParseParams{
		Analysis: repository.UpdateLinkAnalysisParams{
			ID: linkID, ExpectedParseGeneration: siteAttempt.Generation,
			ExpectedMetadataRevision: siteAttempt.ExpectedMetadataRevision,
			Title:                    &siteTitle, Status: model.LinkStatusDone,
		},
		Classification: repository.UpdateLibraryClassificationParams{ID: linkID, Kind: model.LibraryKindSite},
		Site: repository.AggregateSiteParams{
			LinkID: linkID, IdentityKey: "v1:host:site-to-reading-selection.example",
			NormalizedURL: "https://site-to-reading-selection.example/",
			Name:          siteTitle,
			EntryName:     "Home",
		},
	})
	if err != nil {
		t.Fatalf("CompleteSiteParse(): %v", err)
	}
	var persistedEntryID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM site_entries WHERE site_id=$1 AND link_id=$2`, siteResult.SiteID, linkID).Scan(&persistedEntryID); err != nil {
		t.Fatalf("read completed site entry: %v", err)
	}
	if persistedEntryID != siteResult.EntryID {
		t.Fatalf("site entry = %s, want %s", persistedEntryID, siteResult.EntryID)
	}

	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	selection, err := dbLinkCommands(pool, repo, queue).SetLinkLibraryKind(ctx, service.SetLinkLibraryKindCommand{
		LinkID: linkID, Kind: model.LibraryKindReading, Override: true,
	})
	if err != nil {
		t.Fatalf("SetLinkLibraryKind(reading): %v", err)
	}
	if selection.Status != model.LinkStatusPending {
		t.Fatalf("selection result = %#v, want pending replacement", selection)
	}
	assertLibrarySelection(t, pool, linkID, model.LibraryKindReading, true)

	current, err := repo.GetByID(ctx, linkID)
	if err != nil || current == nil {
		t.Fatalf("GetByID(reading replacement) = %#v, %v", current, err)
	}
	readingAttempt := parseAttemptForLink(current)
	if err := repo.MarkParseProcessing(ctx, readingAttempt); err != nil {
		t.Fatalf("MarkParseProcessing(reading): %v", err)
	}
	expectedReading := model.LibraryKindReading
	if err := completeReadingSelection(repo, ctx, readingAttempt, &expectedReading, true, true); err != nil {
		t.Fatalf("CompleteReadingParse(): %v", err)
	}

	var entryCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM site_entries WHERE link_id=$1`, linkID).Scan(&entryCount); err != nil {
		t.Fatalf("count detached site entries: %v", err)
	}
	if entryCount != 0 {
		t.Fatalf("site entry count = %d, want 0", entryCount)
	}
	assertLibrarySelection(t, pool, linkID, model.LibraryKindReading, true)
}

func TestConcurrentSelectionCommitRejectsStaleCompletion(t *testing.T) {
	pool := StartPostgres(t)
	linkID, attempt := insertPendingLinkAttempt(t, pool, "https://concurrent-library-selection.example/")
	ctx := t.Context()
	repo := repository.NewPGXLinkRepository(pool)
	if err := repo.MarkParseProcessing(ctx, attempt); err != nil {
		t.Fatalf("MarkParseProcessing(): %v", err)
	}

	selectionTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin selection transaction: %v", err)
	}
	defer func() { _ = selectionTx.Rollback(context.Background()) }()
	if _, err := repo.SetLibraryKindTx(ctx, selectionTx, repository.SetLibraryKindParams{
		ID: linkID, Kind: model.LibraryKindSite, Override: true,
	}); err != nil {
		t.Fatalf("stage library selection: %v", err)
	}

	completionPool, completionPID := oneConnectionPool(t, DSN(t))
	completionRepo := repository.NewPGXLinkRepository(completionPool)
	completionErr := make(chan error, 1)
	go func() {
		completionErr <- completeReadingSelection(completionRepo, context.Background(), attempt, nil, false, false)
	}()
	waitForBackendRowLock(t, pool, completionPID)
	if err := selectionTx.Commit(ctx); err != nil {
		t.Fatalf("commit library selection: %v", err)
	}

	select {
	case err := <-completionErr:
		if !errors.Is(err, repository.ErrLibrarySelectionChanged) {
			t.Fatalf("stale completion error = %v, want ErrLibrarySelectionChanged", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stale completion did not unblock after selection commit")
	}

	expectedSite := model.LibraryKindSite
	title := "Latest committed selection"
	_, err = repo.CompleteSiteParse(ctx, repository.CompleteSiteParseParams{
		Analysis: repository.UpdateLinkAnalysisParams{
			ID: linkID, ExpectedParseGeneration: attempt.Generation,
			ExpectedMetadataRevision: attempt.ExpectedMetadataRevision,
			Title:                    &title, Status: model.LinkStatusDone,
		},
		Classification:            repository.UpdateLibraryClassificationParams{ID: linkID, Kind: model.LibraryKindSite, Locked: true},
		ExpectedLibraryKind:       &expectedSite,
		ExpectedLibraryKindLocked: true,
		Site: repository.AggregateSiteParams{
			LinkID: linkID, IdentityKey: "v1:host:concurrent-library-selection.example",
			NormalizedURL: "https://concurrent-library-selection.example/", Name: title, EntryName: "Home",
		},
	})
	if err != nil {
		t.Fatalf("retry latest site completion: %v", err)
	}
	assertLibrarySelection(t, pool, linkID, model.LibraryKindSite, true)
}

func TestAutomaticRequeuePreservesLockedSelection(t *testing.T) {
	pool := StartPostgres(t)
	linkID, initialAttempt := insertPendingLinkAttempt(t, pool, "https://example.com/retry-library-selection")
	ctx := t.Context()
	repo := repository.NewPGXLinkRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	commands := dbLinkCommands(pool, repo, queue)

	if _, err := commands.SetLinkLibraryKind(ctx, service.SetLinkLibraryKindCommand{
		LinkID: linkID, Kind: model.LibraryKindSite, Override: true,
	}); err != nil {
		t.Fatalf("seed site selection: %v", err)
	}
	for _, command := range []service.RequeueLinkCommand{
		{LinkID: linkID},
		{LinkID: linkID, Capture: &service.LinkCapture{
			URL: "https://example.com/retry-library-selection", SourceKind: "rss",
			SourceKey: "https://example.com/retry-library-selection", Status: model.LinkStatusPending,
			RequestedLibraryKind: model.RequestedLibraryKindReading,
		}},
	} {
		if _, err := commands.RequeueLink(ctx, command); err != nil {
			t.Fatalf("RequeueLink(%#v): %v", command.Capture, err)
		}
		assertLibrarySelection(t, pool, linkID, model.LibraryKindSite, true)
	}
	current, err := repo.GetByID(ctx, linkID)
	if err != nil || current == nil {
		t.Fatalf("GetByID(after retries) = %#v, %v", current, err)
	}
	if current.ParseGeneration != initialAttempt.Generation+2 {
		t.Fatalf("parse generation = %d, want %d", current.ParseGeneration, initialAttempt.Generation+2)
	}
}

func completeReadingSelection(
	repo *repository.PGXLinkRepository,
	ctx context.Context,
	attempt model.ParseAttempt,
	expectedKind *model.LibraryKind,
	expectedLocked bool,
	locked bool,
) error {
	title := "Selection completion"
	_, err := repo.CompleteReadingParse(ctx, repository.CompleteReadingParseParams{
		Analysis: repository.UpdateLinkAnalysisParams{
			ID: attempt.LinkID, ExpectedParseGeneration: attempt.Generation,
			ExpectedMetadataRevision: attempt.ExpectedMetadataRevision,
			Title:                    &title, Status: model.LinkStatusDone,
		},
		Classification:            repository.UpdateLibraryClassificationParams{ID: attempt.LinkID, Kind: model.LibraryKindReading, Locked: locked},
		ExpectedLibraryKind:       expectedKind,
		ExpectedLibraryKindLocked: expectedLocked,
	})
	return err
}

func assertLibrarySelection(t *testing.T, pool *pgxpool.Pool, linkID uuid.UUID, wantKind model.LibraryKind, wantLocked bool) {
	t.Helper()
	var kind model.LibraryKind
	var locked bool
	if err := pool.QueryRow(t.Context(), `SELECT library_kind, library_kind_locked FROM links WHERE id=$1`, linkID).Scan(&kind, &locked); err != nil {
		t.Fatalf("read library selection: %v", err)
	}
	if kind != wantKind || locked != wantLocked {
		t.Fatalf("library selection = %s locked=%v, want %s/%v", kind, locked, wantKind, wantLocked)
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
		err := pool.QueryRow(t.Context(), `SELECT wait_event_type = 'Lock' FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&waiting)
		if err == nil && waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("backend %d did not block on the held link row", pid)
}
