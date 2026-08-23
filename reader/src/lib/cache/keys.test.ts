import { describe, expect, it } from 'vitest'

import {
  ANNOTATED_LINKS_CACHE_KEY,
  LINKS_CACHE_PREFIX,
  contentCacheKey,
  feedItemsFirstPageCacheKey,
  linkDetailCacheKey,
  linksFirstPageCacheKey,
  translationsKey,
} from './keys'

describe('ResourceStore cache keys', () => {
  it('builds exact first-page keys for normal and annotated link streams', () => {
    expect(
      linksFirstPageCacheKey({
        query: '?limit=30&library_kind=reading&status=done',
        selectionType: 'smart',
        selectionId: 'all',
        annotated: false,
      }),
    ).toBe('GET /api/links?limit=30&library_kind=reading&status=done#smart:all')

    expect(
      linksFirstPageCacheKey({
        query: '?limit=30',
        selectionType: 'smart',
        selectionId: 'annotated',
        annotated: true,
      }),
    ).toBe(ANNOTATED_LINKS_CACHE_KEY)
  })

  it('keeps point projections under the link-list prefix with encoded IDs', () => {
    expect(linkDetailCacheKey('L 1/2')).toBe(
      'GET /api/links/L%201%2F2?include_content=false',
    )
  })

  it('keeps saved content and translations out of library-wide link invalidation', () => {
    expect(contentCacheKey('L1', 7).startsWith(LINKS_CACHE_PREFIX)).toBe(false)
    expect(translationsKey('L1', 7).startsWith(LINKS_CACHE_PREFIX)).toBe(false)
  })

  it('uses separators that keep prefix-neighbor link IDs independent', () => {
    expect(contentCacheKey('L12', 7).startsWith(contentCacheKey('L1', 7))).toBe(false)
    expect(translationsKey('L12', 7).startsWith(translationsKey('L1', 7))).toBe(false)
  })

  it('centralizes feed item first-page key construction below hooks', () => {
    expect(feedItemsFirstPageCacheKey('?view=all&page=1')).toBe(
      'GET /api/feed-items?view=all&page=1',
    )
  })
})
