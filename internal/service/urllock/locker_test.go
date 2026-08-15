package urllock

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestInProcessURLLockerSerializesSameURL pins the contract that a
// SubmitNew flow for the same URL is mutually exclusive — without this
// two concurrent /api/links submits for the same URL would race the
// GetByURL → INSERT path and one would lose to the unique constraint.
func TestInProcessURLLockerSerializesSameURL(t *testing.T) {
	t.Parallel()

	locker := NewInProcessURLLocker()
	const url = "https://example.com/x"
	var concurrent int32
	var maxConcurrent int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = locker.WithURL(context.Background(), url, func(_ context.Context) error {
				now := atomic.AddInt32(&concurrent, 1)
				for {
					prev := atomic.LoadInt32(&maxConcurrent)
					if now <= prev || atomic.CompareAndSwapInt32(&maxConcurrent, prev, now) {
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
				atomic.AddInt32(&concurrent, -1)
				return nil
			})
		}()
	}
	wg.Wait()
	if maxConcurrent != 1 {
		t.Fatalf("max concurrent fn calls for same URL = %d, want 1", maxConcurrent)
	}
}

// TestInProcessURLLockerAllowsDifferentURLsInParallel exercises the
// striping: different URLs that hash to different shards must run
// concurrently. With 256 shards two random URLs collide ~1/256 — pick
// inputs whose shards differ to stay deterministic.
func TestInProcessURLLockerAllowsDifferentURLsInParallel(t *testing.T) {
	t.Parallel()

	a := "https://example.com/a"
	b := "https://example.com/b"
	aShard := shardIndex(a)
	bShard := shardIndex(b)
	if aShard == bShard {
		t.Skipf("URLs collided on shard %d; pick fresh inputs", aShard)
	}

	locker := NewInProcessURLLocker()
	gateA := make(chan struct{})
	gateB := make(chan struct{})
	done := make(chan struct{}, 2)

	go func() {
		_ = locker.WithURL(context.Background(), a, func(_ context.Context) error {
			gateA <- struct{}{}
			<-gateB
			return nil
		})
		done <- struct{}{}
	}()
	go func() {
		<-gateA
		_ = locker.WithURL(context.Background(), b, func(_ context.Context) error {
			gateB <- struct{}{}
			return nil
		})
		done <- struct{}{}
	}()

	timeout := time.After(time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("different-URL fn calls did not complete in parallel")
		}
	}
}

func TestInProcessURLLockerWithURLsOrdersOverlappingSets(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	locker := NewInProcessURLLocker()
	sets := [][]string{
		{"https://example.com/order-a", "https://example.com/order-b"},
		{"https://example.com/order-b", "https://example.com/order-a"},
	}
	start := make(chan struct{})
	done := make(chan error, len(sets))
	for _, urls := range sets {
		urls := urls
		go func() {
			<-start
			done <- locker.WithURLs(ctx, urls, func(context.Context) error {
				time.Sleep(5 * time.Millisecond)
				return nil
			})
		}()
	}
	close(start)
	for range sets {
		if err := <-done; err != nil {
			t.Fatalf("WithURLs() error = %v", err)
		}
	}
}

func TestURLLockSetsAreSortedAndDeduplicated(t *testing.T) {
	t.Parallel()

	urls := []string{
		"https://example.com/z",
		"https://example.com/a",
		"https://example.com/z",
	}
	shards := inProcessShardIndexes(urls)
	for i := 1; i < len(shards); i++ {
		if shards[i-1] >= shards[i] {
			t.Fatalf("in-process shard indexes = %v, want strictly ascending unique values", shards)
		}
	}
	lockIDs := advisoryLockObjIDs(urls)
	for i := 1; i < len(lockIDs); i++ {
		if lockIDs[i-1] >= lockIDs[i] {
			t.Fatalf("advisory lock ids = %v, want strictly ascending unique values", lockIDs)
		}
	}
}

// TestInProcessURLLockerHonorsContextCancellation guards the cancel
// path: a caller blocked waiting on a busy shard returns ctx.Err()
// instead of waiting indefinitely once its context is cancelled.
func TestInProcessURLLockerHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	locker := NewInProcessURLLocker()
	const url = "https://example.com/cancel"
	hold := make(chan struct{})
	released := make(chan struct{})

	go func() {
		_ = locker.WithURL(context.Background(), url, func(_ context.Context) error {
			<-hold
			return nil
		})
		close(released)
	}()

	// Spin until the holder has actually entered the shard so the
	// second WithURL is guaranteed to park.
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := locker.WithURL(ctx, url, func(_ context.Context) error {
		t.Fatal("fn ran after ctx cancellation")
		return nil
	})
	if err == nil {
		t.Fatal("WithURL returned nil after ctx cancel, want ctx.Err()")
	}
	close(hold)
	<-released
}
