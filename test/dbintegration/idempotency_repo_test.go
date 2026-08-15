// idempotency_repo_test.go exercises the Phase 13 (v4.0 M2) PG-backed
// idempotency store against a real PostgreSQL container. The fake-store unit
// tests in internal/middleware cover the middleware flow; this file is the
// only place that exercises the actual INSERT ... ON CONFLICT DO NOTHING
// acquire semantics + the multi-replica replay guarantee (two independent repo
// instances backed by the same table, modelling two API replicas).
package dbintegration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"webtag/internal/repository"
)

// TestIdempotencyRepo_AcquireStoreReplayAcrossReplicas is the headline
// multi-replica assertion: replica A acquires a key and stores its response;
// replica B (a SEPARATE repo instance over the same table) acquiring the same
// key does NOT win the insert and instead reads back A's stored response —
// proving the PG sink makes the same Idempotency-Key effective across replicas
// (the old in-memory LRU degraded to per-replica).
func TestIdempotencyRepo_AcquireStoreReplayAcrossReplicas(t *testing.T) {
	pool := StartPostgres(t)
	ctx := context.Background()

	replicaA := repository.NewPGXIdempotencyRepository(pool)
	replicaB := repository.NewPGXIdempotencyRepository(pool)

	key := "POST:/api/links:shared-key"
	now := time.Now()
	expires := now.Add(time.Hour)

	// Replica A acquires (first insert wins).
	acquiredA, existingA, err := replicaA.Acquire(ctx, key, "replica-a", now, expires)
	if err != nil {
		t.Fatalf("replicaA acquire: %v", err)
	}
	if !acquiredA || existingA == nil {
		t.Fatalf("replicaA acquire = (%v, %v), want owned claim", acquiredA, existingA)
	}

	// Before A stores its response, B tries to acquire: must NOT win, and the
	// existing row is still in-flight.
	acquiredB, existingB, err := replicaB.Acquire(ctx, key, "replica-b", now, expires)
	if err != nil {
		t.Fatalf("replicaB acquire (pre-store): %v", err)
	}
	if acquiredB {
		t.Fatal("replicaB won the acquire; want loser (A already holds the key)")
	}
	if existingB == nil || !existingB.InFlight {
		t.Fatalf("replicaB existing = %+v, want non-nil in-flight row", existingB)
	}

	// A finishes its handler and stores the response.
	body := []byte(`{"id":"abc","status":"pending"}`)
	if err := replicaA.Store(ctx, key, existingA.OwnerToken, existingA.Generation, 202, body, "application/json", expires); err != nil {
		t.Fatalf("replicaA store: %v", err)
	}

	// Now B acquires again: still loses, but the row is no longer in-flight and
	// carries A's response — B would replay it.
	acquiredB2, existingB2, err := replicaB.Acquire(ctx, key, "replica-b", now, expires)
	if err != nil {
		t.Fatalf("replicaB acquire (post-store): %v", err)
	}
	if acquiredB2 {
		t.Fatal("replicaB won the acquire after store; want loser")
	}
	if existingB2 == nil {
		t.Fatal("replicaB existing post-store = nil, want A's stored response")
	}
	if existingB2.InFlight {
		t.Fatal("replicaB existing still in-flight after A stored; want completed")
	}
	if existingB2.Status != 202 {
		t.Fatalf("replayed status = %d, want 202", existingB2.Status)
	}
	if string(existingB2.Body) != string(body) {
		t.Fatalf("replayed body = %q, want %q", existingB2.Body, body)
	}
	if existingB2.ContentType != "application/json" {
		t.Fatalf("replayed content-type = %q, want application/json", existingB2.ContentType)
	}
}

func TestIdempotencyRepo_UnknownResultRemainsFailClosedAcrossReplicas(t *testing.T) {
	pool := StartPostgres(t)
	ctx := context.Background()
	replicaA := repository.NewPGXIdempotencyRepository(pool)
	replicaB := repository.NewPGXIdempotencyRepository(pool)
	replicaC := repository.NewPGXIdempotencyRepository(pool)
	key := "POST:/api/links:unknown-result"
	initialNow := time.Now()

	acquired, first, err := replicaA.Acquire(ctx, key, "replica-a", initialNow, initialNow.Add(time.Second))
	if err != nil || !acquired || first == nil {
		t.Fatalf("replicaA acquire = (%v, %+v, %v), want owned claim", acquired, first, err)
	}
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := replicaA.Store(cancelledCtx, key, first.OwnerToken, first.Generation, 201, []byte(`{"id":"possibly-committed"}`), "application/json", initialNow.Add(time.Hour)); err == nil {
		t.Fatal("Store() with cancelled context succeeded; want deterministic finalize failure")
	}

	afterTTL := initialNow.Add(2 * time.Second)
	acquired, observedB, err := replicaB.Acquire(ctx, key, "replica-b", afterTTL, afterTTL.Add(time.Hour))
	if err != nil {
		t.Fatalf("replicaB acquire after TTL: %v", err)
	}
	assertUnknownClaimPreserved(t, acquired, observedB, first)

	if purged, purgeErr := replicaB.PurgeExpired(ctx, afterTTL); purgeErr != nil || purged != 0 {
		t.Fatalf("PurgeExpired() = (%d, %v), want (0, nil) for unknown claim", purged, purgeErr)
	}
	acquired, observedC, err := replicaC.Acquire(ctx, key, "replica-c", afterTTL.Add(time.Second), afterTTL.Add(time.Hour))
	if err != nil {
		t.Fatalf("replicaC acquire after purge: %v", err)
	}
	assertUnknownClaimPreserved(t, acquired, observedC, first)
}

func assertUnknownClaimPreserved(t *testing.T, acquired bool, observed, original *repository.IdempotencyRecord) {
	t.Helper()
	if acquired {
		t.Fatal("expired in-flight claim was reacquired; want permanent fail-closed state")
	}
	if observed == nil || original == nil {
		t.Fatalf("observed/original claim = (%+v, %+v), want both non-nil", observed, original)
	}
	if !observed.InFlight || observed.OwnerToken != original.OwnerToken || observed.Generation != original.Generation {
		t.Fatalf("observed claim = %+v, want preserved owner=%q generation=%d in-flight", observed, original.OwnerToken, original.Generation)
	}
}

func TestIdempotencyRepo_ConcurrentAcquireHasSingleOwner(t *testing.T) {
	pool := StartPostgres(t)
	ctx := context.Background()
	now := time.Now()
	expires := now.Add(time.Hour)
	key := "POST:/api/links:concurrent-key"

	type result struct {
		acquired bool
		record   *repository.IdempotencyRecord
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for index, repo := range []*repository.PGXIdempotencyRepository{
		repository.NewPGXIdempotencyRepository(pool),
		repository.NewPGXIdempotencyRepository(pool),
	} {
		wg.Add(1)
		go func(index int, repo *repository.PGXIdempotencyRepository) {
			defer wg.Done()
			<-start
			acquired, record, err := repo.Acquire(ctx, key, "replica-"+string(rune('a'+index)), now, expires)
			results <- result{acquired: acquired, record: record, err: err}
		}(index, repo)
	}
	close(start)
	wg.Wait()
	close(results)

	winners := 0
	for got := range results {
		if got.err != nil {
			t.Fatalf("concurrent acquire: %v", got.err)
		}
		if got.record == nil || !got.record.InFlight || got.record.Generation != 1 {
			t.Fatalf("concurrent claim = %+v, want generation 1 in-flight row", got.record)
		}
		if got.acquired {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent acquire winners = %d, want exactly 1", winners)
	}
}

func TestIdempotencyRepo_ExpiredTakeoverRejectsStaleOwner(t *testing.T) {
	pool := StartPostgres(t)
	ctx := context.Background()
	repo := repository.NewPGXIdempotencyRepository(pool)
	key := "POST:/api/links:takeover-key"
	initialNow := time.Now()

	acquired, first, err := repo.Acquire(ctx, key, "owner-a", initialNow, initialNow.Add(time.Second))
	if err != nil || !acquired || first == nil {
		t.Fatalf("first acquire = (%v, %+v, %v), want owned claim", acquired, first, err)
	}
	if err := repo.Store(ctx, key, first.OwnerToken, first.Generation, 200, []byte("first"), "text/plain", initialNow.Add(time.Second)); err != nil {
		t.Fatalf("first Store() error = %v", err)
	}
	takeoverNow := initialNow.Add(2 * time.Second)
	acquired, second, err := repo.Acquire(ctx, key, "owner-b", takeoverNow, takeoverNow.Add(time.Hour))
	if err != nil || !acquired || second == nil {
		t.Fatalf("takeover acquire = (%v, %+v, %v), want owned claim", acquired, second, err)
	}
	if second.Generation != first.Generation+1 || second.OwnerToken != "owner-b" {
		t.Fatalf("takeover claim = %+v, want owner-b generation %d", second, first.Generation+1)
	}

	if err := repo.Store(ctx, key, first.OwnerToken, first.Generation, 200, []byte("stale"), "text/plain", takeoverNow.Add(time.Hour)); !errors.Is(err, repository.ErrIdempotencyClaimLost) {
		t.Fatalf("stale Store() error = %v, want ErrIdempotencyClaimLost", err)
	}
	if err := repo.Delete(ctx, key, first.OwnerToken, first.Generation); !errors.Is(err, repository.ErrIdempotencyClaimLost) {
		t.Fatalf("stale Delete() error = %v, want ErrIdempotencyClaimLost", err)
	}
	if err := repo.Store(ctx, key, second.OwnerToken, second.Generation, 201, []byte("current"), "text/plain", takeoverNow.Add(time.Hour)); err != nil {
		t.Fatalf("current Store() error = %v", err)
	}
}

// TestIdempotencyRepo_DeleteAllowsReacquire deleting a key (the 5xx /
// not-cached path) lets a later request re-acquire it fresh.
func TestIdempotencyRepo_DeleteAllowsReacquire(t *testing.T) {
	pool := StartPostgres(t)
	ctx := context.Background()
	repo := repository.NewPGXIdempotencyRepository(pool)

	key := "POST:/fail:k"
	now := time.Now()
	expires := now.Add(time.Hour)

	acquired, claim, err := repo.Acquire(ctx, key, "owner-a", now, expires)
	if err != nil || !acquired || claim == nil {
		t.Fatalf("first acquire = (%v, %+v, %v), want owned claim", acquired, claim, err)
	}
	if err := repo.Delete(ctx, key, claim.OwnerToken, claim.Generation); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// After delete, the key is gone → a fresh acquire wins again.
	acquired, existing, err := repo.Acquire(ctx, key, "owner-b", now, expires)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if !acquired || existing == nil || existing.OwnerToken != "owner-b" {
		t.Fatalf("re-acquire = (%v, %+v), want owner-b claim", acquired, existing)
	}
}

// TestIdempotencyRepo_PurgeExpired removes only expired completed rows and
// preserves expired in-flight rows as durable unknown-result evidence.
func TestIdempotencyRepo_PurgeExpired(t *testing.T) {
	pool := StartPostgres(t)
	ctx := context.Background()
	repo := repository.NewPGXIdempotencyRepository(pool)

	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	acquired, completed, err := repo.Acquire(ctx, "expired-completed-key", "completed-owner", now, future)
	if err != nil || !acquired || completed == nil {
		t.Fatalf("acquire expired-completed-key: (%v, %+v, %v)", acquired, completed, err)
	}
	if err := repo.Store(ctx, "expired-completed-key", completed.OwnerToken, completed.Generation, 201, []byte("done"), "text/plain", past); err != nil {
		t.Fatalf("store expired-completed-key: %v", err)
	}
	if acquired, _, err := repo.Acquire(ctx, "expired-unknown-key", "unknown-owner", now, past); err != nil || !acquired {
		t.Fatalf("acquire expired-unknown-key: (%v, %v)", acquired, err)
	}
	if acquired, _, err := repo.Acquire(ctx, "live-key", "live-owner", now, future); err != nil || !acquired {
		t.Fatalf("acquire live-key: (%v, %v)", acquired, err)
	}

	n, err := repo.PurgeExpired(ctx, now)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d rows, want 1 (only the expired one)", n)
	}
	for _, preserved := range []string{"expired-unknown-key", "live-key"} {
		if acquired, existing, err := repo.Acquire(ctx, preserved, "other-owner", now, future); err != nil {
			t.Fatalf("re-acquire %s: %v", preserved, err)
		} else if acquired || existing == nil || !existing.InFlight {
			t.Fatalf("re-acquire %s = (%v, %+v), want preserved in-flight row", preserved, acquired, existing)
		}
	}
	if acquired, existing, err := repo.Acquire(ctx, "expired-completed-key", "new-owner", now, future); err != nil || !acquired || existing == nil {
		t.Fatalf("re-acquire purged completed key = (%v, %+v, %v), want new owned claim", acquired, existing, err)
	}
}
