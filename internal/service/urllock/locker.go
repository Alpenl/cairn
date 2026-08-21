package urllock

import (
	"context"
	"hash/fnv"
	"sort"
)

// inProcessLockerShards must be a power of two so shardIndex can mask
// instead of mod. 256 is a sweet spot: under typical submit/ingest
// concurrency the false-sharing collision rate is well below 1%, and
// the total fixed memory footprint is ~6 KiB (one channel each).
const inProcessLockerShards = 256

// InProcessURLLocker serializes WithURL calls per-URL using a striped
// channel-based mutex array. It replaces the per-URL pg_advisory_lock
// for single-instance deployments — the previous implementation held a
// pool connection for the entire fn execution, doubling the per-batch
// connection footprint. The locker is intentionally single-process.
type InProcessURLLocker struct {
	shards [inProcessLockerShards]chan struct{}
}

// NewInProcessURLLocker 构造内存版 URL 串行化锁，全部分片在构造时一次性初始化。
func NewInProcessURLLocker() *InProcessURLLocker {
	var l InProcessURLLocker
	for i := range l.shards {
		l.shards[i] = make(chan struct{}, 1)
	}
	return &l
}

// WithURL is the single-key convenience form of WithURLs.
func (l *InProcessURLLocker) WithURL(ctx context.Context, rawURL string, fn func(context.Context) error) error {
	return l.WithURLs(ctx, []string{rawURL}, fn)
}

// WithURLs serializes fn against every URL in the set. Shards are deduplicated
// and acquired in ascending order so overlapping batches cannot deadlock when
// callers supply URLs in different orders. Waiting for each shard respects ctx;
// already-acquired shards are released in reverse order on every exit path.
func (l *InProcessURLLocker) WithURLs(ctx context.Context, rawURLs []string, fn func(context.Context) error) error {
	if l == nil {
		return fn(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	indexes := inProcessShardIndexes(rawURLs)
	acquired := make([]uint32, 0, len(indexes))
	defer func() {
		for i := len(acquired) - 1; i >= 0; i-- {
			<-l.shards[acquired[i]]
		}
	}()

	for _, index := range indexes {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case l.shards[index] <- struct{}{}:
			acquired = append(acquired, index)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fn(ctx)
}

func inProcessShardIndexes(rawURLs []string) []uint32 {
	unique := make(map[uint32]struct{}, len(rawURLs))
	for _, rawURL := range rawURLs {
		unique[shardIndex(rawURL)] = struct{}{}
	}
	indexes := make([]uint32, 0, len(unique))
	for index := range unique {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	return indexes
}

func shardIndex(rawURL string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(rawURL))
	return h.Sum32() & (inProcessLockerShards - 1)
}
