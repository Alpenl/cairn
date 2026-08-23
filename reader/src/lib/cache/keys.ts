/**
 * ResourceStore key builders.
 *
 * This module is intentionally below hooks, persistence and invalidation:
 * it owns stable cache-key strings but does not import React, ResourceStore,
 * storage, fetch, or Reader client code.
 */

/** Stable subscription key used to wake the indexed view after link-cache invalidation. */
export const LINKS_CACHE_PREFIX = 'GET /api/links'
export const ANNOTATED_LINKS_CACHE_KEY = `${LINKS_CACHE_PREFIX}#smart:annotated`

export interface LinksFirstPageCacheKeyInput {
  readonly query: string
  readonly selectionType: string
  readonly selectionId: string
  readonly annotated: boolean
}

/**
 * Cache key for the first page of one link stream.
 *
 * The query captures the server request while the selection suffix keeps
 * streams with equivalent query parameters but different UI identities apart.
 * Annotated links have no list request, but still need a stable key for
 * pagination and optimistic state reset.
 */
export function linksFirstPageCacheKey(input: LinksFirstPageCacheKeyInput): string {
  if (input.annotated) return ANNOTATED_LINKS_CACHE_KEY
  return `${LINKS_CACHE_PREFIX}${input.query}#${input.selectionType}:${input.selectionId}`
}

export function linkDetailCacheKey(linkId: string): string {
  return `${LINKS_CACHE_PREFIX}/${encodeURIComponent(linkId)}?include_content=false`
}

export const TAGS_CACHE_PREFIX = 'GET /api/tags'
export const TAGS_CACHE_KEY = `${TAGS_CACHE_PREFIX}?library_kind=reading`

export const DOMAIN_SUMMARIES_CACHE_PREFIX = 'GET /api/tree?view=domains'
export const DOMAIN_SUMMARIES_CACHE_KEY = `${DOMAIN_SUMMARIES_CACHE_PREFIX}&library_kind=reading`

export const SUBSCRIPTIONS_CACHE_KEY = 'GET /api/subscriptions'

export const FEED_ITEMS_CACHE_PREFIX = 'GET /api/feed-items'

export function feedItemsFirstPageCacheKey(query: string): string {
  return `${FEED_ITEMS_CACHE_PREFIX}${query}`
}

/**
 * Saved-content cache prefix.
 *
 * It intentionally does not live under `GET /api/links`. Library writes
 * invalidate that broad list prefix; saved article bodies are partitioned by
 * content revision and should not be evicted by unrelated list writes.
 */
export const CONTENT_CACHE_PREFIX = 'GET content:/api/links'

export function contentCacheKey(linkId: string, revision: number | undefined): string {
  return `${CONTENT_CACHE_PREFIX}/${encodeURIComponent(linkId)}?rev=${revision ?? 0}`
}

/**
 * Translation cache prefix.
 *
 * Like saved content, translations used to sit below `GET /api/links` and were
 * therefore evicted by every library-level write. Keep them in their own
 * namespace so invalidation can target the changed link generation.
 */
export const TRANSLATIONS_CACHE_PREFIX = 'GET translations:/api/links'

export function cacheRevisionOrNull(value: unknown): number | null {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0
    ? value
    : null
}

/** Cache key for one saved-content translation generation. */
export function translationsKey(
  linkId: string,
  contentRevision?: number | null,
): string {
  const revision = cacheRevisionOrNull(contentRevision)
  return `${TRANSLATIONS_CACHE_PREFIX}/${encodeURIComponent(linkId)}/translations?rev=${revision ?? 'unverified'}`
}
