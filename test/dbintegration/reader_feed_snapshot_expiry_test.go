package dbintegration

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/repository"
)

// TestReaderFeedSnapshotExpirySweep pins the retention half of the feed
// snapshot contract: creating a snapshot reclaims expired ones, in bounded
// batches, without ever touching a snapshot a live cursor could still resolve.
//
// The 24 hour window itself is unchanged — the sweep predicate is the exact
// complement of the reader's, so the two assertions below (a still-valid cursor
// keeps resolving, an already-expired cursor keeps failing) hold before and
// after this change. What changes is only that the dead rows stop accumulating.
func TestReaderFeedSnapshotExpirySweep(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()

	// The batch cap is what makes this safe on the read path, so the fixture has
	// to be larger than one batch: a test that seeds fewer expired rows than the
	// cap cannot tell "bounded" from "unbounded".
	const expiredRows = 500
	const sweepBatch = 200
	// Rows must be wide enough that a sequential scan is genuinely more
	// expensive than the index — a table of 500 skinny rows fits in so few pages
	// that the planner is right to ignore any index, and the assertion below
	// would then be measuring the fixture rather than the query.
	if _, err := pool.Exec(ctx, `
		INSERT INTO reader_feed_snapshots (mode,items,created_at)
		SELECT 'recommended',
			jsonb_build_object('mode','recommended','items',
				jsonb_build_array(jsonb_build_object('key','expired-'||g,'pad',repeat(md5(g::text),80)))),
			NOW() - INTERVAL '24 hours' - (g || ' seconds')::interval
		FROM generate_series(1,$1) g`, expiredRows); err != nil {
		t.Fatalf("seed expired snapshots: %v", err)
	}
	// Two rows the sweep must never take: one comfortably inside the window and
	// one a minute inside the boundary.
	if _, err := pool.Exec(ctx, `
		INSERT INTO reader_feed_snapshots (mode,items,created_at) VALUES
			('recommended','{"mode":"recommended","items":[]}'::jsonb, NOW() - INTERVAL '1 hour'),
			('recommended','{"mode":"recommended","items":[]}'::jsonb, NOW() - INTERVAL '23 hours 59 minutes')`); err != nil {
		t.Fatalf("seed live snapshots: %v", err)
	}
	if _, err := pool.Exec(ctx, `VACUUM (ANALYZE) reader_feed_snapshots`); err != nil {
		t.Fatalf("vacuum analyze snapshots: %v", err)
	}

	liveIDs := readerFeedSnapshotIDs(t, pool, `created_at > NOW() - INTERVAL '24 hours'`)
	if len(liveIDs) != 2 {
		t.Fatalf("live snapshot fixture = %d rows, want 2", len(liveIDs))
	}
	expiredIDs := readerFeedSnapshotIDs(t, pool, `created_at <= NOW() - INTERVAL '24 hours'`)
	if len(expiredIDs) != expiredRows {
		t.Fatalf("expired snapshot fixture = %d rows, want %d", len(expiredIDs), expiredRows)
	}

	const expiryIndex = "idx_reader_feed_snapshots_expiry"
	// Only the batch selection is asserted. How the DELETE then reaches those
	// rows is a cost decision that legitimately depends on table size: on a
	// fixture this small a sequential scan really is the cheaper plan, so
	// demanding an index there would assert the fixture, not the query. The
	// scale-dependent half is handled by the statement's shape instead —
	// `id = ANY(ARRAY(...))` keeps a primary-key path available for the planner
	// to choose once the table is large enough for it to win.
	before := readIndexScanCounts(t, pool, expiryIndex)

	page, err := repo.ListFeed(ctx, "recommended", "", "", 10)
	if err != nil {
		t.Fatalf("ListFeed: %v", err)
	}
	if page.SnapshotID == "" {
		t.Fatal("ListFeed returned no snapshot id")
	}

	// Exactly one batch, and only from the expired side.
	remainingExpired := readerFeedSnapshotIDs(t, pool, `created_at <= NOW() - INTERVAL '24 hours'`)
	if len(remainingExpired) != expiredRows-sweepBatch {
		t.Fatalf("expired snapshots after one feed request = %d, want %d (one bounded batch reclaimed)",
			len(remainingExpired), expiredRows-sweepBatch)
	}
	// The oldest rows go first, so every survivor must be one of the newer ones.
	remaining := make(map[string]struct{}, len(remainingExpired))
	for _, id := range remainingExpired {
		remaining[id] = struct{}{}
	}
	for index, id := range expiredIDs {
		_, survived := remaining[id]
		// expiredIDs is ordered oldest first by the helper's ORDER BY.
		if wantSurvived := index >= sweepBatch; survived != wantSurvived {
			t.Fatalf("expired snapshot %d (id=%s) survived=%v, want %v: the sweep is not oldest-first",
				index, id, survived, wantSurvived)
		}
	}
	stillLive := readerFeedSnapshotIDs(t, pool, `created_at > NOW() - INTERVAL '24 hours'`)
	liveSet := make(map[string]struct{}, len(stillLive))
	for _, id := range stillLive {
		liveSet[id] = struct{}{}
	}
	for _, id := range liveIDs {
		if _, ok := liveSet[id]; !ok {
			t.Fatalf("sweep deleted unexpired snapshot %s", id)
		}
	}
	if _, ok := liveSet[page.SnapshotID]; !ok {
		t.Fatalf("snapshot %s created by this request is missing from the live set", page.SnapshotID)
	}

	after := awaitIndexScanProgress(t, pool, before, expiryIndex)
	if after[expiryIndex] <= before[expiryIndex] {
		t.Fatalf("%s scan count stayed at %d: the sweep picks its batch by scanning the whole snapshot table",
			expiryIndex, before[expiryIndex])
	}
	t.Logf("sweep index scans: %s %d -> %d", expiryIndex, before[expiryIndex], after[expiryIndex])

	// The 24 hour read contract is untouched on both sides.
	if _, err := repo.ListFeed(ctx, "recommended", page.SnapshotID, "", 10); err != nil {
		t.Fatalf("re-reading the snapshot this request created: %v", err)
	}
	survivingExpired := remainingExpired[len(remainingExpired)-1]
	_, err = repo.ListFeed(ctx, "recommended", survivingExpired, "", 10)
	if !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("ListFeed(expired snapshot) error = %v, want ErrNotFound", err)
	}
}

// readerFeedSnapshotIDs lists snapshot ids matching a predicate, oldest first.
// The predicate is a test-owned literal, never user input.
func readerFeedSnapshotIDs(t *testing.T, pool *pgxpool.Pool, predicate string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`SELECT id::text FROM reader_feed_snapshots WHERE `+predicate+` ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("list feed snapshots (%s): %v", predicate, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan feed snapshot id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate feed snapshot ids: %v", err)
	}
	return ids
}
