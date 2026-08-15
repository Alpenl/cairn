package service

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"webtag/internal/representation"
)

// defaultSnapshotCacheEntries bounds the number of variant/version snapshots
// retained by one installation. Representation versions advance over time, so
// an explicit LRU bound prevents old validators from growing the cache forever.
const defaultSnapshotCacheEntries = 1024

// snapshotLoadTimeout bounds one coalesced load. The loader no longer inherits
// the leader's cancellation (see Get), so it needs its own upper bound instead
// of relying on whichever caller happened to arrive first.
const snapshotLoadTimeout = 30 * time.Second

// SnapshotLoader loads a snapshot after a cache miss.
type SnapshotLoader[T any] func(context.Context) (T, error)

// SnapshotCache is an installation-wide TTL snapshot cache with singleflight
// miss coalescing and stale-on-error fallback. A key contains only the
// representation variant and optional version; Cairn has no tenant partition.
// Returned values must be treated as read-only.
type SnapshotCache[T any] struct {
	mu         sync.RWMutex
	ttl        time.Duration
	now        func() time.Time
	maxEntries int
	clone      func(T) T
	entries    map[snapshotKey]*snapshotEntry[T]
	group      singleflight.Group
	generation uint64
}

type snapshotKey struct {
	variant string
	version string
}

type snapshotEntry[T any] struct {
	value     T
	expiresAt time.Time
	lastUsed  time.Time
	loaded    bool
}

// NewSnapshotCache constructs an installation-wide cache. A nil clock uses
// time.Now. Slice and map values should provide a clone function so a loader
// cannot mutate a stored snapshot after returning it.
func NewSnapshotCache[T any](ttl time.Duration, now func() time.Time, clone func(T) T) *SnapshotCache[T] {
	if now == nil {
		now = time.Now
	}
	if clone == nil {
		clone = func(value T) T { return value }
	}
	return &SnapshotCache[T]{
		ttl:        ttl,
		now:        now,
		maxEntries: defaultSnapshotCacheEntries,
		clone:      clone,
		entries:    make(map[snapshotKey]*snapshotEntry[T]),
	}
}

// Get returns the cached variant for this installation. Unversioned reads may
// return a stale snapshot together with a loader error; versioned reads never
// do so because an old body must not be paired with a new strong validator.
func (c *SnapshotCache[T]) Get(ctx context.Context, variant string, loader SnapshotLoader[T]) (T, error) {
	key := newSnapshotKey(ctx, variant)
	if value, ok := c.cached(key); ok {
		return value, nil
	}

	// singleflight 只执行 leader 那一份闭包。若闭包直接沿用 leader 的 context，
	// leader 断连（浏览器跳转、移动端切后台、写超时）会把 context.Canceled 分发给
	// 每一个等待者，让 context 完全健康的并发请求一起失败。因此：
	//   1. 闭包用剥离取消的 context，leader 退出不再波及他人；
	//   2. 改用 DoChan，让每个调用方各自响应自己的 context。
	flightKey := key.variant + "\x00" + key.version
	shared := c.group.DoChan(flightKey, func() (any, error) {
		if value, ok := c.cached(key); ok {
			return value, nil
		}
		generation := c.currentGeneration()
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotLoadTimeout)
		defer cancel()
		loaded, loadErr := loader(loadCtx)
		if loadErr != nil {
			return nil, loadErr
		}
		fresh := c.clone(loaded)
		c.store(key, fresh, generation)
		return fresh, nil
	})

	var result any
	var err error
	select {
	case <-ctx.Done():
		// 只有本调用方走了。其他等待者继续由同一次 in-flight 加载服务。
		var zero T
		return zero, ctx.Err()
	case outcome := <-shared:
		result, err = outcome.Val, outcome.Err
	}
	if err != nil {
		if key.version == "" {
			if stale, ok := c.stale(key); ok {
				return stale, err
			}
		}
		var zero T
		return zero, err
	}

	value, _ := result.(T)
	return value, nil
}

func newSnapshotKey(ctx context.Context, variant string) snapshotKey {
	key := snapshotKey{variant: variant}
	version, ok := representation.FromContext(ctx)
	if !ok {
		return key
	}
	canonical, err := version.CanonicalJSON()
	if err == nil {
		key.version = string(canonical)
	}
	return key
}

// Invalidate advances the installation generation and removes every cached
// variant. The context remains in the interface because callers share the same
// invalidator contract as request-scoped persistence operations.
func (c *SnapshotCache[T]) Invalidate(context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	clear(c.entries)
}

func (c *SnapshotCache[T]) currentGeneration() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.generation
}

func (c *SnapshotCache[T]) cached(key snapshotKey) (T, bool) {
	if c.ttl <= 0 {
		var zero T
		return zero, false
	}

	c.mu.RLock()
	entry, ok := c.entries[key]
	if !ok || !entry.loaded || !c.now().Before(entry.expiresAt) {
		c.mu.RUnlock()
		var zero T
		return zero, false
	}
	value := entry.value
	c.mu.RUnlock()

	c.mu.Lock()
	if current, still := c.entries[key]; still {
		current.lastUsed = c.now()
	}
	c.mu.Unlock()
	return value, true
}

func (c *SnapshotCache[T]) stale(key snapshotKey) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok || !entry.loaded {
		var zero T
		return zero, false
	}
	return entry.value, true
}

func (c *SnapshotCache[T]) store(key snapshotKey, value T, generation uint64) {
	if c.ttl <= 0 {
		return
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation {
		return
	}
	if _, exists := c.entries[key]; !exists {
		c.evictIfFullLocked()
	}
	c.entries[key] = &snapshotEntry[T]{
		value:     value,
		expiresAt: now.Add(c.ttl),
		lastUsed:  now,
		loaded:    true,
	}
}

func (c *SnapshotCache[T]) evictIfFullLocked() {
	if len(c.entries) < c.maxEntries {
		return
	}
	var oldestKey snapshotKey
	var oldestAt time.Time
	first := true
	for key, entry := range c.entries {
		if first || entry.lastUsed.Before(oldestAt) {
			oldestKey, oldestAt, first = key, entry.lastUsed, false
		}
	}
	if !first {
		delete(c.entries, oldestKey)
	}
}
