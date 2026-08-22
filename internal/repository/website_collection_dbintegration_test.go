//go:build dbintegration

package repository

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWebsiteRecentListUsesInclusiveInstantCutoffAndStableOrder(t *testing.T) {
	dsn := os.Getenv("WEBTAG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WEBTAG_TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping database: %v", err)
	}

	// Keep the rows private to this transaction. The tag also makes the list
	// assertions independent of any fixture rows in the disposable database.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin recent-site transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	testTag := "dbintegration-" + uuid.NewString()
	requestNow := time.Date(2026, 8, 10, 2, 15, 30, 0, time.UTC)
	cutoff := requestNow.Add(-720 * time.Hour)
	boundaryLow := uuid.MustParse("10000000-0000-0000-0000-000000000001")
	boundaryHigh := uuid.MustParse("10000000-0000-0000-0000-000000000002")
	late := uuid.MustParse("10000000-0000-0000-0000-000000000003")
	early := uuid.MustParse("10000000-0000-0000-0000-000000000004")
	for _, item := range []struct {
		id   uuid.UUID
		name string
		at   time.Time
	}{
		{boundaryLow, "boundary-low", cutoff},
		{boundaryHigh, "boundary-high", cutoff},
		{late, "late", cutoff.Add(time.Millisecond)},
		{early, "early", cutoff.Add(-time.Millisecond)},
	} {
		_, err := tx.Exec(ctx, `INSERT INTO sites
	  (id, site_key, name, intro, first_collected_at, last_collected_at)
	VALUES ($1,$2,$3,'',$4,$4)`, item.id, "manual-recent:"+item.id.String(), item.name, item.at)
		if err != nil {
			t.Fatalf("seed %s: %v", item.name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO site_tags (site_id, tag, normalized_tag)
	VALUES ($1,$2,$2)`, item.id, testTag); err != nil {
			t.Fatalf("tag %s: %v", item.name, err)
		}
	}

	repo := NewPGXSiteRepository(tx)
	recent, total, err := repo.ListSites(ctx, SiteListFilter{
		View: "recent", RecentCutoff: &cutoff, Tags: []string{testTag}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("recent ListSites() error = %v", err)
	}
	want := []uuid.UUID{late, boundaryHigh, boundaryLow}
	if total != len(want) || len(recent) != len(want) {
		t.Fatalf("recent total=%d items=%#v", total, recent)
	}
	for i := range want {
		if recent[i].ID != want[i] {
			t.Fatalf("recent[%d]=%s want=%s", i, recent[i].ID, want[i])
		}
	}
	all, allTotal, err := repo.ListSites(ctx, SiteListFilter{View: "all", Tags: []string{testTag}, Limit: 10})
	if err != nil {
		t.Fatalf("all ListSites() error = %v", err)
	}
	if allTotal != 4 || len(all) != 4 {
		t.Fatalf("all total=%d items=%#v", allTotal, all)
	}
}

func TestWebsiteCollectionConcurrentSiteIdentity(t *testing.T) {
	dsn := os.Getenv("WEBTAG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WEBTAG_TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping database: %v", err)
	}

	identityKey := "v1:host:concurrent-" + uuid.NewString()
	ids := make(chan uuid.UUID, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := insertConcurrentSite(ctx, pool, identityKey, uuid.New())
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var first uuid.UUID
	for id := range ids {
		if first == uuid.Nil {
			first = id
		} else if id != first {
			t.Fatalf("concurrent upsert returned different site ids: %s and %s", first, id)
		}
	}
	if first == uuid.Nil {
		t.Fatal("concurrent upsert returned no site id")
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sites WHERE site_key = $1", identityKey)
	})
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sites WHERE site_key = $1", identityKey).Scan(&count); err != nil {
		t.Fatalf("count concurrent sites: %v", err)
	}
	if count != 1 {
		t.Fatalf("concurrent site count = %d, want 1", count)
	}

	testWebsiteAggregateConcurrency(t, ctx, pool)
}

func testWebsiteAggregateConcurrency(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	identityKey := "v1:host:aggregate-" + uuid.NewString()
	linkA, linkB := uuid.New(), uuid.New()
	for linkID, suffix := range map[uuid.UUID]string{linkA: "a", linkB: "b"} {
		if _, err := pool.Exec(ctx, `INSERT INTO links (id, url, source_key, status, first_collected_at)
VALUES ($1,$2,$2,'pending',NOW())`, linkID, "https://aggregate.example/"+suffix+"/"+uuid.NewString()); err != nil {
			t.Fatalf("seed aggregate link %s: %v", suffix, err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM sites WHERE site_key = $1", identityKey)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM links WHERE id = ANY($1::uuid[])", []uuid.UUID{linkA, linkB})
	})

	type aggregateOutcome struct {
		result SiteAggregateResult
		err    error
	}
	runAggregate := func(linkID uuid.UUID, name string) aggregateOutcome {
		repo := NewPGXLinkRepository(pool)
		result, err := repo.Aggregate(ctx, AggregateSiteParams{
			LinkID: linkID, IdentityKey: identityKey, NormalizedURL: "https://aggregate.example/" + linkID.String(),
			Name: "Aggregate Example", EntryName: name,
		})
		return aggregateOutcome{result: result, err: err}
	}

	results := make(chan aggregateOutcome, 2)
	var wg sync.WaitGroup
	for linkID, name := range map[uuid.UUID]string{linkA: "Entry A", linkB: "Entry B"} {
		wg.Add(1)
		go func(linkID uuid.UUID, name string) {
			defer wg.Done()
			results <- runAggregate(linkID, name)
		}(linkID, name)
	}
	wg.Wait()
	close(results)

	var siteID uuid.UUID
	for outcome := range results {
		if outcome.err != nil {
			t.Fatalf("concurrent repository aggregate: %v", outcome.err)
		}
		if siteID == uuid.Nil {
			siteID = outcome.result.SiteID
		} else if outcome.result.SiteID != siteID {
			t.Fatalf("concurrent aggregates returned different site ids: %s and %s", siteID, outcome.result.SiteID)
		}
	}
	if siteID == uuid.Nil {
		t.Fatal("concurrent repository aggregate returned no site")
	}
	assertAggregateCounts(t, ctx, pool, identityKey, siteID, 2)

	// A duplicate capture of the same Link must refresh its existing entry,
	// even when both refreshes race at the entry-level unique key.
	results = make(chan aggregateOutcome, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- runAggregate(linkA, "Entry A")
		}()
	}
	wg.Wait()
	close(results)
	var entryID uuid.UUID
	for outcome := range results {
		if outcome.err != nil {
			t.Fatalf("duplicate repository aggregate: %v", outcome.err)
		}
		if outcome.result.SiteID != siteID {
			t.Fatalf("duplicate aggregate site id = %s, want %s", outcome.result.SiteID, siteID)
		}
		if entryID == uuid.Nil {
			entryID = outcome.result.EntryID
		} else if outcome.result.EntryID != entryID {
			t.Fatalf("duplicate aggregate returned different entry ids: %s and %s", entryID, outcome.result.EntryID)
		}
	}
	assertAggregateCounts(t, ctx, pool, identityKey, siteID, 2)
}

func assertAggregateCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, identityKey string, siteID uuid.UUID, wantEntries int) {
	t.Helper()
	var sites, entries int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sites WHERE site_key = $1", identityKey).Scan(&sites); err != nil {
		t.Fatalf("count aggregate sites: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM site_entries WHERE site_id = $1", siteID).Scan(&entries); err != nil {
		t.Fatalf("count aggregate entries: %v", err)
	}
	if sites != 1 || entries != wantEntries {
		t.Fatalf("aggregate result sites=%d entries=%d, want sites=1 entries=%d", sites, entries, wantEntries)
	}
}

func insertConcurrentSite(ctx context.Context, pool *pgxpool.Pool, key string, candidateID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := pool.QueryRow(ctx, `INSERT INTO sites
	  (id, site_key, name, intro)
	VALUES ($1,$2,$3,'')
ON CONFLICT (site_key) DO UPDATE SET updated_at=now()
RETURNING id`, candidateID, key, "Concurrent Example").Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("concurrent site upsert: %w", err)
	}
	return id, nil
}
