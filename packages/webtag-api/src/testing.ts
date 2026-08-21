import {
  isCapabilitiesResponse,
  isErrorResponse,
  isLinkContentResponse,
  isLinkResponse,
  isPaginatedLinksResponse,
  isReaderFeedResponse,
  isSubmitResponse,
  isTagCountResponse,
} from './guards'
import {
  normalizeHttpError,
  normalizeThrownError,
  parseRetryAfter,
} from './errors'
import { buildLinksQuery, buildQueryString } from './query'

export interface SharedContractTestDriver {
  group: (name: string, run: () => void) => unknown
  test: (name: string, run: () => void) => unknown
  equal: (actual: unknown, expected: unknown) => void
}

function validLink(): Record<string, unknown> {
  return {
    id: 'link-1',
    url: 'https://example.com/article',
    title: 'Article',
    summary: null,
    description: null,
    tags: ['shared'],
    content_type: 'article',
    status: 'done',
    domain: 'example.com',
    path_depth: 1,
    parent_id: null,
    parent_path: null,
    fetcher_type: 'readability',
    has_content: true,
    is_low_confidence: false,
    low_confidence_reason: null,
    error_category: null,
    error_msg: null,
    created_at: '2026-08-08T00:00:00Z',
    updated_at: '2026-08-08T00:00:00Z',
    metadata_revision: 1,
  }
}

/** The same contract suite is invoked by Reader Vitest 3 and Extension Vitest 4. */
export function defineSharedApiContractTests(
  driver: SharedContractTestDriver,
): void {
  const { group, test, equal } = driver

  group('shared WebTag API contract', () => {
    test('validates the common generated response shapes', () => {
      const link = validLink()
      equal(isLinkResponse(link), true)
      equal(isLinkResponse({ ...link, library_kind: 'reading' }), true)
      equal(
        isPaginatedLinksResponse({
          items: [link],
          total: 1,
          page: 1,
          limit: 20,
        }),
        true,
      )
      equal(isSubmitResponse({ link_id: 'link-1', status: 'pending' }), true)
      equal(
        isSubmitResponse({
          inbox_id: 'inbox-1',
          destination: 'inbox',
          status: 'pending',
        }),
        true,
      )
      equal(isTagCountResponse({ tag: 'shared', count: 1 }), true)
      equal(
        isLinkContentResponse({
          link_id: 'link-1',
          content: 'body',
          content_format: 'plain',
          fetcher_type: 'readability',
          content_source: 'fetched',
          content_revision: 3,
        }),
        true,
      )
      equal(
        isCapabilitiesResponse({
          library_kinds: true,
          site_library: true,
          site_management: true,
          site_advanced_management: true,
          archive_versions: [2],
          reader_vnext: true,
          reader: {
            annotations: true,
            notes: true,
            inbox: true,
            todos: true,
            engagement: true,
            home: true,
            feed: true,
            ai: true,
            related_tags: true,
            activity: true,
            history: true,
            trash: true,
          },
        }),
        true,
      )
      const feedItem = {
        key: 'subscription:feed-1',
        source: 'subscription',
        resource_key: 'link:link-1',
        title: 'Feed item',
        summary: 'Summary',
        url: 'https://example.com/article',
        link_id: 'link-1',
        feed_item_id: 'feed-1',
        read: false,
        read_later: false,
        saved: false,
        event_at: '2026-08-08T00:00:00Z',
      }
      equal(
        isReaderFeedResponse({
          items: [feedItem],
          next_cursor: 'live-cursor-1',
          mode: 'recommended',
        }),
        true,
      )
      equal(
        isReaderFeedResponse({
          items: [feedItem],
          mode: 'chronological',
        }),
        true,
      )
      equal(
        isReaderFeedResponse({
          items: [{ ...feedItem, resource_key: undefined }],
          mode: 'recommended',
        }),
        false,
      )
      equal(
        isReaderFeedResponse({
          items: [{ ...feedItem, source: 'reading' }],
          mode: 'recommended',
        }),
        false,
      )
      equal(
        isReaderFeedResponse({
          items: [{ ...feedItem, event_at: undefined }],
          mode: 'chronological',
        }),
        false,
      )
      equal(
        isErrorResponse({
          error: { code: 409, message: 'stale', error_code: 'conflict' },
        }),
        true,
      )
    })

    test('rejects incomplete and invalid wire values', () => {
      equal(isLinkResponse({ ...validLink(), has_content: 'yes' }), false)
      equal(isLinkResponse({ ...validLink(), library_kind: 'archive' }), false)
      equal(
        isPaginatedLinksResponse({
          items: [validLink()],
          total: 1,
          page: 1,
          limit: '20',
        }),
        false,
      )
      equal(isSubmitResponse({ link_id: 'link-1', status: 'queued' }), false)
      equal(isSubmitResponse({ status: 'pending' }), false)
      equal(
        isSubmitResponse({
          inbox_id: 'inbox-1',
          destination: 'archive',
          status: 'pending',
        }),
        false,
      )
      equal(isTagCountResponse({ tag: 'shared', count: Number.NaN }), false)
      equal(isErrorResponse({ error: { code: '409', message: 'stale' } }), false)
    })

    test('normalizes authentication and throttling errors consistently', () => {
      const forbidden = {
        error: {
          code: 403,
          message: 'forbidden',
          error_code: 'unauthorized',
        },
      }
      equal(normalizeHttpError(403, forbidden).kind, 'unauthorized')
      const throttled = {
        error: {
          code: 429,
          message: 'too many requests',
          error_code: 'rate_limit_exceeded',
        },
      }
      equal(normalizeHttpError(429, throttled, 5).kind, 'rate-limited')
      equal(normalizeThrownError(new TypeError('offline'), 100).kind, 'network-unreachable')
      equal(normalizeThrownError({ name: 'AbortError' }, 100).kind, 'timeout')
      equal(parseRetryAfter('12'), 12)
      equal(parseRetryAfter('0'), undefined)
      equal(parseRetryAfter('0', { allowZero: true }), 0)
    })

    test('builds stable encoded queries without browser globals', () => {
      equal(
        buildQueryString({ q: 'two words', enabled: false, skip: undefined }),
        'q=two+words&enabled=false',
      )
      equal(
        buildLinksQuery({ q: '  article  ', page: 1, limit: 20 }),
        '?q=article&limit=20',
      )
      equal(
        buildLinksQuery({
          status: 'done',
          library_kind: 'reading',
          created_from: '2026-08-10T16:00:00.000Z',
          created_before: '2026-08-11T16:00:00.000Z',
        }),
        '?status=done&library_kind=reading&created_from=2026-08-10T16%3A00%3A00.000Z&created_before=2026-08-11T16%3A00%3A00.000Z',
      )
    })
  })
}
