package concept

import (
	"strings"
	"sync"

	"github.com/google/uuid"
)

// AliasCache is a process-local map of lowercased alias → concept_id.
// The resolver consults it before every concept_alias SELECT; on a
// hit we skip the round-trip entirely, on a miss we fall through to
// the store and back-fill the result.
//
// Only positive entries are cached. A negative result (alias does not
// resolve yet) is not stored because the alias may be created moments
// later by another goroutine's UpsertAlias and we would otherwise
// have to invalidate on every write — defeating the cache's value.
// The cost of one repeated SELECT for a truly-missing alias is the
// same as the no-cache baseline, so this trade is free.
//
// The cache is safe for concurrent use. It has no size cap: the
// upstream alias table is bounded by the concept count (~5 aliases
// per concept) and a 100k-concept deployment would consume ~25 MB of
// heap here, well below the per-process budget. If that ceiling ever
// becomes a concern, swap the inner map for an LRU without touching
// callers.
type AliasCache struct {
	m sync.Map // key: string (lowercased alias), val: uuid.UUID
}

// NewAliasCache 返回一个空的进程内别名缓存。
func NewAliasCache() *AliasCache {
	return &AliasCache{}
}

// Get returns the cached concept_id for alias, if any. The lookup
// performs the same normalisation (trim + lowercase) the store does
// so callers can hand in raw surface tags without pre-processing.
// A nil receiver is a valid "no cache" sentinel and always misses —
// keeps the resolver's nil-cache branch a one-liner.
func (c *AliasCache) Get(alias string) (uuid.UUID, bool) {
	if c == nil {
		return uuid.Nil, false
	}
	key := normalizeAlias(alias)
	if key == "" {
		return uuid.Nil, false
	}
	v, ok := c.m.Load(key)
	if !ok {
		return uuid.Nil, false
	}
	id, _ := v.(uuid.UUID)
	return id, id != uuid.Nil
}

// Put stores alias → id. Empty alias or nil id is a noop so callers
// can blindly back-fill whatever the store returned without an extra
// guard at every site. LoadOrStore semantics mean a concurrent Put
// for the same alias keeps the first writer's value — acceptable
// here because both writers observed the same DB row.
func (c *AliasCache) Put(alias string, id uuid.UUID) {
	if c == nil || id == uuid.Nil {
		return
	}
	key := normalizeAlias(alias)
	if key == "" {
		return
	}
	c.m.LoadOrStore(key, id)
}

// InvalidateAll drops every cached entry. Called after a transactional
// concept merge re-points or deletes concept_alias rows; the merge tx
// can change the mapping for any of the loser's aliases (and FK cascade
// can drop arbitrary entries when the loser concept is removed), so a
// targeted invalidation would require enumerating every affected alias
// up-front. The full flush is O(N) over the cached entries — cheap
// next to the merge transaction itself, which is admin-triggered and
// runs at human cadence.
//
// Stale-window note: between the merge tx commit and this call, a
// concurrent Resolve may return a now-deleted loser concept_id. The
// downstream AttachLinkConcept is fail-soft (FK violation on the
// dropped concept is logged and swallowed) and the next Resolve after
// invalidation observes the correct winner, so the bounded staleness
// is tolerable.
func (c *AliasCache) InvalidateAll() {
	if c == nil {
		return
	}
	c.m.Range(func(k, _ any) bool {
		c.m.Delete(k)
		return true
	})
}

// AliasInvalidator is the slice of *AliasCache the merge admin service
// holds. Defining it as an interface lets the service take a nil-safe
// no-op stub in deployments that do not wire a cache (tests, opt-out
// configs) without dragging the concrete cache type into the admin
// service.
type AliasInvalidator interface {
	InvalidateAll()
}

func normalizeAlias(alias string) string {
	return strings.ToLower(strings.TrimSpace(alias))
}
