import { describe, expect, it } from 'vitest'

import type { IdentityContext } from '../identity'
import {
  ANNOTATED_LINKS_CACHE_KEY,
  CONTENT_CACHE_PREFIX,
  DOMAIN_SUMMARIES_CACHE_PREFIX,
  FEED_ITEMS_CACHE_PREFIX,
  LINKS_CACHE_PREFIX,
  PERSISTED_CACHE_PREFIXES,
  READER_ACTIVITY_CACHE_PREFIX,
  READER_RELATED_TAGS_CACHE_PREFIX,
  SITES_CACHE_PREFIX,
  SUBSCRIPTIONS_CACHE_KEY,
  TAGS_CACHE_PREFIX,
  TRANSLATIONS_CACHE_PREFIX,
  contentCacheKey,
  feedItemsCacheKey,
  identityScopedCacheKey,
  linkContentInvalidationPrefix,
  linkDetailCacheKey,
  linkTranslationsInvalidationPrefix,
  linksFirstPageCacheKey,
  readerActivityCacheKey,
  readerIdentityCacheSuffix,
  readerRelatedTagsCacheKey,
  sitesCacheKey,
  sitesPageCacheKey,
  translationsKey,
} from './keys'

const IDENTITY: IdentityContext = {
  serverClientDataNamespace: 'server one',
  physicalNamespace: 'physical/two',
  localEpoch: 3,
}

describe('reader cache keys', () => {
  it('keeps link list and point projection keys in the library prefix', () => {
    expect(LINKS_CACHE_PREFIX).toBe('GET /api/links')
    expect(linkDetailCacheKey('L/1?')).toBe(
      'GET /api/links/L%2F1%3F?include_content=false',
    )
    expect(linksFirstPageCacheKey(
      { type: 'tag', id: 'read later' },
      { library_kind: 'reading', status: 'done', limit: 30, after: '', tags: 'read later' },
    )).toBe(
      'GET /api/links?tags=read+later&status=done&library_kind=reading&after=&limit=30#tag:read later',
    )
    expect(linksFirstPageCacheKey(
      { type: 'smart', id: 'annotated' },
      { library_kind: 'reading', status: 'done', limit: 30, after: '' },
    )).toBe(ANNOTATED_LINKS_CACHE_KEY)
  })

  it('keeps saved content outside the link-list invalidation prefix', () => {
    expect(CONTENT_CACHE_PREFIX).toBe('GET content:/api/links')
    expect(contentCacheKey('L/1?', undefined)).toBe(
      'GET content:/api/links/L%2F1%3F?rev=0',
    )
    expect(contentCacheKey('L1', 7).startsWith(linkContentInvalidationPrefix('L1'))).toBe(true)
    expect(contentCacheKey('L12', 7).startsWith(linkContentInvalidationPrefix('L1'))).toBe(false)
    expect(contentCacheKey('L1', 7).startsWith(LINKS_CACHE_PREFIX)).toBe(false)
  })

  it('keeps translations outside the link-list invalidation prefix', () => {
    expect(TRANSLATIONS_CACHE_PREFIX).toBe('GET translations:/api/links')
    expect(translationsKey('L/1?', 7)).toBe(
      'GET translations:/api/links/L%2F1%3F/translations?rev=7',
    )
    expect(translationsKey('L1', null)).toBe(
      'GET translations:/api/links/L1/translations?rev=unverified',
    )
    expect(translationsKey('L1').startsWith(linkTranslationsInvalidationPrefix('L1'))).toBe(true)
    expect(translationsKey('L12').startsWith(linkTranslationsInvalidationPrefix('L1'))).toBe(false)
    expect(translationsKey('L1', 7).startsWith(LINKS_CACHE_PREFIX)).toBe(false)
  })

  it('builds aggregate, feed, and site keys without changing their wire shape', () => {
    expect(TAGS_CACHE_PREFIX).toBe('GET /api/tags')
    expect(DOMAIN_SUMMARIES_CACHE_PREFIX).toBe('GET /api/tree?view=domains')
    expect(SUBSCRIPTIONS_CACHE_KEY).toBe('GET /api/subscriptions')
    expect(feedItemsCacheKey({ view: 'unread', q: ' term ', page: 1, limit: 30 })).toBe(
      'GET /api/feed-items?view=unread&q=term&limit=30',
    )
    expect(FEED_ITEMS_CACHE_PREFIX).toBe('GET /api/feed-items')
    expect(sitesCacheKey({ view: 'recent', tags: ' tag ', recentCutoff: ' 2026-01 ', page: 2, limit: 30 })).toBe(
      'GET /api/sites?view=recent&tags=tag&recent_cutoff=2026-01&page=2&limit=30',
    )
    expect(sitesPageCacheKey({ view: 'all', page: 1, limit: 30 }, 4)).toBe(
      'GET /api/sites?view=all&limit=30#capability=4',
    )
    expect(SITES_CACHE_PREFIX).toBe('GET /api/sites')
  })

  it('builds identity-scoped reader helper keys with the existing suffix partition', () => {
    expect(readerIdentityCacheSuffix(IDENTITY)).toBe('server%20one:physical%2Ftwo:3')
    expect(readerIdentityCacheSuffix(null)).toBe('unscoped')
    expect(identityScopedCacheKey('GET /api/sites', IDENTITY)).toBe(
      'GET /api/sites#server%20one:physical%2Ftwo:3',
    )
    expect(readerRelatedTagsCacheKey('L/1?', IDENTITY, 12)).toBe(
      `${READER_RELATED_TAGS_CACHE_PREFIX}?link_id=L%2F1%3F&limit=12#server%20one:physical%2Ftwo:3`,
    )
    expect(readerActivityCacheKey('domain', IDENTITY, 100)).toBe(
      `${READER_ACTIVITY_CACHE_PREFIX}?kind=domain&limit=100#server%20one:physical%2Ftwo:3`,
    )
  })

  it('lists only the prefixes that are durable across reloads', () => {
    expect(PERSISTED_CACHE_PREFIXES).toEqual([
      LINKS_CACHE_PREFIX,
      CONTENT_CACHE_PREFIX,
      TRANSLATIONS_CACHE_PREFIX,
      TAGS_CACHE_PREFIX,
      DOMAIN_SUMMARIES_CACHE_PREFIX,
      SUBSCRIPTIONS_CACHE_KEY,
      SITES_CACHE_PREFIX,
    ])
  })
})
