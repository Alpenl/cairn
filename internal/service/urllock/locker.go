package urllock

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/lockkey"
)

const advisoryUnlockTimeout = 5 * time.Second

// inProcessLockerShards must be a power of two so shardIndex can mask
// instead of mod. 256 is a sweet spot: under typical submit/ingest
// concurrency the false-sharing collision rate is well below 1%, and
// the total fixed memory footprint is ~6 KiB (one channel each).
const inProcessLockerShards = 256

// InProcessURLLocker serializes WithURL calls per-URL using a striped
// channel-based mutex array. It replaces the per-URL pg_advisory_lock
// for single-instance deployments — the previous implementation held a
// pool connection for the entire fn execution, doubling the per-batch
// connection footprint. The locker is inherently single-process; for a
// multi-instance deployment fall back to AdvisoryURLLocker, which still
// uses a Postgres advisory namespace and survives across replicas.
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

// AdvisoryLockClassSubmit scopes submit locks within the int64 advisory-lock
// namespace. Postgres advisory locks have a single shared int64 keyspace
// (or two-int variant); using the two-int form keeps unrelated subsystems
// (submit URL keys, future schema migrations, etc.) in
// separate 32-bit object-id namespaces. Object-id hash
// collisions are still possible; they only cause conservative serialization
// of unrelated work and do not compromise correctness.
const AdvisoryLockClassSubmit int32 = lockkey.ClassSubmit

// AdvisoryURLLocker is the multi-instance implementation used by production
// write paths that must serialize work across replicas.
// It uses pg_advisory_lock(classid, objid) two-int form to scope the
// advisory namespace by lock class.
type AdvisoryURLLocker struct {
	pool  *pgxpool.Pool
	class int32
	gate  *AdvisoryLockGate
}

// AdvisoryLockGate limits how many callbacks may hold session-level advisory
// lock connections from one pool at once. Submission callers share
// one gate in production; reserving part of the pool for callback queries and
// transactions prevents every connection becoming a lock holder waiting for
// another connection from the same exhausted pool.
type AdvisoryLockGate struct {
	slots chan struct{}
}

// NewAdvisoryLockGate constructs a process-local budget for session lock
// holders. maxHeld is clamped to one so callers cannot accidentally create a
// gate that blocks all work forever.
func NewAdvisoryLockGate(maxHeld int) *AdvisoryLockGate {
	if maxHeld < 1 {
		maxHeld = 1
	}
	return &AdvisoryLockGate{slots: make(chan struct{}, maxHeld)}
}

func (g *AdvisoryLockGate) acquire(ctx context.Context) error {
	if g == nil {
		return nil
	}
	select {
	case g.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *AdvisoryLockGate) release() {
	if g != nil {
		<-g.slots
	}
}

// NewAdvisoryURLLocker returns an AdvisoryURLLocker bound to the given
// lock class. class is the first int of the two-int advisory call; objid
// is derived from the URL hash. Picking distinct classes per subsystem
// prevents accidental cross-namespace collisions.
func NewAdvisoryURLLocker(pool *pgxpool.Pool, class int32) *AdvisoryURLLocker {
	return NewAdvisoryURLLockerWithGate(pool, class, NewAdvisoryLockGate(1))
}

// NewAdvisoryURLLockerWithGate binds a locker to a shared connection budget.
// All lockers using the same pgx pool should receive the same gate.
func NewAdvisoryURLLockerWithGate(pool *pgxpool.Pool, class int32, gate *AdvisoryLockGate) *AdvisoryURLLocker {
	return &AdvisoryURLLocker{pool: pool, class: class, gate: gate}
}

// WithURL is the single-key convenience form of WithURLs.
func (l *AdvisoryURLLocker) WithURL(ctx context.Context, rawURL string, fn func(context.Context) error) (err error) {
	return l.WithURLs(ctx, []string{rawURL}, fn)
}

// WithURLs acquires the installation-level URL lock set on one PostgreSQL session
// before fn opens any business transaction. Lock ids are deduplicated and
// sorted, giving single and batch writes one global ordering across processes.
// The callback runs only after the whole set is held. The dedicated session is
// never exposed to fn, so one pg_advisory_unlock_all call safely releases the
// complete set with an independent timeout even when the request is canceled.
func (l *AdvisoryURLLocker) WithURLs(ctx context.Context, rawURLs []string, fn func(context.Context) error) (err error) {
	if l == nil || l.pool == nil {
		return fn(ctx)
	}
	lockIDs := advisoryLockObjIDs(rawURLs)
	return l.withAdvisorySession(ctx, func(conn *pgxpool.Conn) error {
		for _, lockID := range lockIDs {
			if _, acquireErr := conn.Exec(ctx, "SELECT pg_advisory_lock($1, $2)", l.class, lockID); acquireErr != nil {
				return fmt.Errorf("acquire advisory lock %d: %w", lockID, acquireErr)
			}
		}
		return fn(ctx)
	})
}

func (l *AdvisoryURLLocker) withAdvisorySession(ctx context.Context, fn func(*pgxpool.Conn) error) (err error) {
	if err := l.gate.acquire(ctx); err != nil {
		return fmt.Errorf("wait for advisory lock connection budget: %w", err)
	}
	defer l.gate.release()

	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire advisory lock connection: %w", err)
	}

	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), advisoryUnlockTimeout)
		defer cancel()

		if _, unlockErr := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock_all()"); unlockErr == nil {
			conn.Release()
			return
		} else {
			// A failed unlock may leave this session holding one or more locks.
			// Close it before releasing pool ownership so shutdown drain cannot
			// observe zero acquisitions while the session still owns the set.
			rawConn := conn.Conn()
			closeErr := rawConn.Close(unlockCtx)
			conn.Release()
			err = errors.Join(
				err,
				fmt.Errorf("release advisory lock set: %w", unlockErr),
				closeErr,
			)
		}
	}()

	err = fn(conn)
	return err
}

func advisoryLockObjIDs(rawURLs []string) []int32 {
	unique := make(map[int32]struct{}, len(rawURLs))
	for _, rawURL := range rawURLs {
		unique[lockkey.URL(rawURL)] = struct{}{}
	}
	lockIDs := make([]int32, 0, len(unique))
	for lockID := range unique {
		lockIDs = append(lockIDs, lockID)
	}
	sort.Slice(lockIDs, func(i, j int) bool { return lockIDs[i] < lockIDs[j] })
	return lockIDs
}
