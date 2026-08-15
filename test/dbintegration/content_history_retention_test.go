package dbintegration

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/repository"
)

func TestContentHistoryRetentionConvergesAcrossInstallation(t *testing.T) {
	pool := StartPostgres(t)

	linkA := seedContentHistoryFixture(t, pool, "link-a", 31, 30)
	linkB := seedContentHistoryFixture(t, pool, "link-b", 31, 30)
	shortLink := seedContentHistoryFixture(t, pool, "link-short", 21, 20)
	// Legacy/manual current and future revision rows are not normal trigger
	// products, but retention must still leave every revision >= current intact.
	currentProtected := seedContentHistoryFixture(t, pool, "current-protected", 30, 31)
	cleanup := repository.NewPGXReaderContentHistoryCleanupRepository(pool)

	deleted, err := cleanup.CleanupContentHistoryBatch(t.Context(), 100)
	if err != nil {
		t.Fatalf("cleanup installation: %v", err)
	}
	if deleted != 26 {
		t.Fatalf("installation cleanup deleted = %d, want 26 across eligible links", deleted)
	}
	assertContentHistoryRevisions(t, pool, linkA, retentionExpectedRevisions(30))
	assertContentHistoryRevisions(t, pool, linkB, retentionExpectedRevisions(30))
	assertContentHistoryRevisions(t, pool, shortLink, integerRange(1, 20))
	assertContentHistoryRevisions(t, pool, currentProtected, append([]int64{1}, integerRange(10, 31)...))

	deleted, err = cleanup.CleanupContentHistoryBatch(t.Context(), 100)
	if err != nil {
		t.Fatalf("repeat installation cleanup: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("repeat installation cleanup deleted = %d, want idempotent zero", deleted)
	}
}

func TestContentHistoryCleanupConcurrentRestoreIsAtomic(t *testing.T) {
	pool := StartPostgres(t)
	cleanup := repository.NewPGXReaderContentHistoryCleanupRepository(pool)
	reader := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()

	for attempt := 0; attempt < 8; attempt++ {
		fixtureKey := fmt.Sprintf("concurrent-%d", attempt)
		linkID := seedContentHistoryFixture(t, pool, fixtureKey, 31, 30)
		historyID := contentHistoryIDForRevision(t, pool, linkID, 5)
		wantRestoredContent := contentHistorySnapshot(fixtureKey, 5)
		start := make(chan struct{})
		cleanupResult := make(chan contentHistoryCleanupResult, 1)
		restoreResult := make(chan contentHistoryRestoreResult, 1)

		go func() {
			<-start
			deleted, err := cleanup.CleanupContentHistoryBatch(ctx, 100)
			cleanupResult <- contentHistoryCleanupResult{deleted: deleted, err: err}
		}()
		go func() {
			<-start
			revision, err := reader.RestoreContentHistory(ctx, linkID, historyID, 31)
			restoreResult <- contentHistoryRestoreResult{revision: revision, err: err}
		}()
		close(start)

		cleanupOutcome := <-cleanupResult
		restoreOutcome := <-restoreResult
		if cleanupOutcome.err != nil {
			t.Fatalf("attempt %d cleanup error = %v", attempt, cleanupOutcome.err)
		}
		if cleanupOutcome.deleted < 9 || cleanupOutcome.deleted > 10 {
			t.Fatalf("attempt %d cleanup deleted = %d, want 9 or 10 around concurrent capture", attempt, cleanupOutcome.deleted)
		}

		var content, contentSource string
		var revision int64
		if err := pool.QueryRow(t.Context(), `
			SELECT content,content_source,content_revision
			FROM links
			WHERE id=$1`, linkID).Scan(&content, &contentSource, &revision); err != nil {
			t.Fatalf("attempt %d read link after race: %v", attempt, err)
		}
		switch {
		case restoreOutcome.err == nil:
			if restoreOutcome.revision != 32 || revision != 32 {
				t.Fatalf("attempt %d restored revisions result/db = %d/%d, want monotonic 32/32", attempt, restoreOutcome.revision, revision)
			}
			if content != wantRestoredContent || contentSource != "user" {
				t.Fatalf("attempt %d restored state = content %q source %q, want exact snapshot %q/user", attempt, content, contentSource, wantRestoredContent)
			}
		case errors.Is(restoreOutcome.err, repository.ErrRevisionConflict), errors.Is(restoreOutcome.err, repository.ErrNotFound):
			if revision != 31 || content != currentContent(fixtureKey) || contentSource != "fetched" {
				t.Fatalf("attempt %d rejected restore partially committed: revision=%d content=%q source=%q", attempt, revision, content, contentSource)
			}
		default:
			t.Fatalf("attempt %d restore error = %v, want success or stable conflict/not-found", attempt, restoreOutcome.err)
		}

		for cleanupPass := 0; cleanupPass < 20; cleanupPass++ {
			deleted, err := cleanup.CleanupContentHistoryBatch(ctx, 100)
			if err != nil {
				t.Fatalf("attempt %d convergence cleanup %d: %v", attempt, cleanupPass, err)
			}
			if deleted < 100 {
				break
			}
			if cleanupPass == 19 {
				t.Fatalf("attempt %d retention did not converge", attempt)
			}
		}
		if restoreOutcome.err == nil {
			assertContentHistoryRevisions(t, pool, linkID, append([]int64{1}, integerRange(12, 31)...))
		} else {
			assertContentHistoryRevisions(t, pool, linkID, retentionExpectedRevisions(30))
		}
	}
}

type contentHistoryCleanupResult struct {
	deleted int
	err     error
}

type contentHistoryRestoreResult struct {
	revision int64
	err      error
}

func seedContentHistoryFixture(
	t *testing.T,
	pool *pgxpool.Pool,
	key string,
	currentRevision int64,
	historyRevision int64,
) uuid.UUID {
	t.Helper()
	var linkID uuid.UUID
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO links (
			url,source_kind,source_key,status,content,content_document,
			content_format,content_source,content_revision,library_kind,
			library_kind_source,first_collected_at)
		VALUES ($1,'url',$1,'done',$2,$2,'plain','fetched',$3,'reading','user',NOW())
		RETURNING id`,
		"https://content-history.example/"+key+"/"+uuid.NewString(),
		currentContent(key),
		currentRevision,
	).Scan(&linkID); err != nil {
		t.Fatalf("seed content history link %s: %v", key, err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO reader_content_history (
			link_id,revision,content,content_document,content_format,content_source)
		SELECT $1,revision,$2 || revision::text,$2 || revision::text,'plain','user'
		FROM generate_series(1,$3::bigint) AS revision`,
		linkID,
		"snapshot-"+key+"-",
		historyRevision,
	); err != nil {
		t.Fatalf("seed content history snapshots %s: %v", key, err)
	}
	return linkID
}

func currentContent(key string) string {
	return "current-" + key
}

func contentHistorySnapshot(key string, revision int64) string {
	return fmt.Sprintf("snapshot-%s-%d", key, revision)
}

func contentHistoryIDForRevision(
	t *testing.T,
	pool *pgxpool.Pool,
	linkID uuid.UUID,
	revision int64,
) int64 {
	t.Helper()
	var historyID int64
	if err := pool.QueryRow(t.Context(), `
		SELECT id FROM reader_content_history
		WHERE link_id=$1 AND revision=$2`, linkID, revision).Scan(&historyID); err != nil {
		t.Fatalf("read content history revision %d: %v", revision, err)
	}
	return historyID
}

func assertContentHistoryRevisions(
	t *testing.T,
	pool *pgxpool.Pool,
	linkID uuid.UUID,
	want []int64,
) {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT revision
		FROM reader_content_history
		WHERE link_id=$1
		ORDER BY revision`, linkID)
	if err != nil {
		t.Fatalf("list content history revisions: %v", err)
	}
	defer rows.Close()
	got := make([]int64, 0, len(want))
	for rows.Next() {
		var revision int64
		if err := rows.Scan(&revision); err != nil {
			t.Fatalf("scan content history revision: %v", err)
		}
		got = append(got, revision)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate content history revisions: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("content history revisions = %v, want %v", got, want)
	}
}

func retentionExpectedRevisions(latestHistoryRevision int64) []int64 {
	return append([]int64{1}, integerRange(latestHistoryRevision-19, latestHistoryRevision)...)
}

func integerRange(first, last int64) []int64 {
	values := make([]int64, 0, last-first+1)
	for value := first; value <= last; value++ {
		values = append(values, value)
	}
	return values
}
