import {
  isCapabilitiesResponse,
  isErrorResponse,
  isJobResponse,
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
      equal(
        isJobResponse({
          id: 'job-1',
          link_id: 'link-1',
          status: 'done',
          error_category: null,
          error_msg: null,
          link,
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
          site_auto_classification: true,
          site_management: true,
          site_advanced_management: true,
          archive_versions: [1, 2],
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
            semantic: true,
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
        item_type: 'subscription',
        resource_key: 'link:link-1',
        action_key: 'subscription:feed-1',
        dedupe_key: 'url:https://example.com/article',
        section_id: 'subscription',
        actions: ['read', 'read_later', 'save', 'unsave', 'open'],
        title: 'Feed item',
        summary: 'Summary',
        url: 'https://example.com/article',
        link_id: 'link-1',
        inbox_id: null,
        feed_item_id: 'feed-1',
        read: false,
		read_later: false,
		saved: false,
		score: 60,
        score_contributions: {
          pending_confirmation: 0,
          saved_library: 0,
          subscription_recent: 40,
          unread: 20,
          read_later: 0,
          chronological_fallback: 0,
		},
		enabled_score_signals: ['subscription_recent', 'unread', 'read_later', 'chronological_fallback'],
        reason_code: 'subscription_recent',
        reason_params: { source: 'subscription' },
        reason_contribution: 40,
        reason_text: '订阅更新',
        published_at: null,
        event_at: '2026-08-08T00:00:00Z',
        created_at: '2026-08-08T00:00:00Z',
      }
      equal(
        isReaderFeedResponse({
          items: [feedItem],
          snapshot_id: 'snapshot-1',
          mode: 'recommended',
          capabilities: ['snapshot', 'cursor', 'dedupe', 'reason', 'source_filter', 'actions'],
          sections: [{
            id: 'subscription',
            source: 'subscription',
            label: '订阅',
            count: 1,
            capabilities: ['read', 'read_later', 'save', 'unsave', 'open'],
          }],
          sources: [{
            id: 'subscription',
            label: '订阅',
            enabled: true,
            count: 1,
            capabilities: ['read', 'read_later', 'save', 'unsave', 'open'],
          }],
        }),
        true,
      )
      equal(
        isReaderFeedResponse({
          items: [feedItem],
          snapshot_id: 'snapshot-off',
          mode: 'chronological',
          capabilities: [],
          sections: [],
          sources: [],
        }),
        true,
      )
      equal(
        isReaderFeedResponse({
          items: [{ ...feedItem, actions: ['unsupported'] }],
          snapshot_id: 'snapshot-invalid',
          mode: 'recommended',
        }),
        false,
      )
      const { score: _score, ...missingScore } = feedItem
      equal(
        isReaderFeedResponse({ items: [missingScore], snapshot_id: 'snapshot-missing-score', mode: 'recommended' }),
        false,
      )
      equal(
        isReaderFeedResponse({
          items: [{ ...feedItem, reason_code: 'saved_library', reason_params: { source: 'subscription' } }],
          snapshot_id: 'snapshot-mismatched-reason',
          mode: 'recommended',
        }),
        false,
      )
      equal(
        isReaderFeedResponse({
          items: [{ ...feedItem, reason_code: 'source_guess' }],
          snapshot_id: 'snapshot-unknown-reason',
          mode: 'recommended',
        }),
        false,
      )
      equal(
        isReaderFeedResponse({
          items: [{ ...feedItem, score_contributions: { subscription_recent: 40 } }],
          snapshot_id: 'snapshot-incomplete-contributions',
          mode: 'recommended',
        }),
        false,
      )
      equal(
        isReaderFeedResponse({
          items: [{ ...feedItem, score: 61 }],
          snapshot_id: 'snapshot-wrong-total',
          mode: 'recommended',
        }),
        false,
      )
      equal(
        isReaderFeedResponse({
          items: [{ ...feedItem, reason_contribution: 20 }],
          snapshot_id: 'snapshot-wrong-winner-contribution',
          mode: 'recommended',
        }),
        false,
      )
      equal(
        isReaderFeedResponse({
          items: [{ ...feedItem, reason_params: { source: 'subscription', guessed: true } }],
          snapshot_id: 'snapshot-ambiguous-params',
          mode: 'recommended',
        }),
        false,
      )
      equal(
        isReaderFeedResponse({
          items: [{ ...feedItem, actions: ['read', 'read'] }],
          snapshot_id: 'snapshot-duplicate-actions',
          mode: 'recommended',
        }),
        false,
      )
      equal(
        isReaderFeedResponse({
          items: [feedItem],
          snapshot_id: 'snapshot-invalid-capability',
          mode: 'recommended',
          capabilities: ['unsupported'],
        }),
        false,
      )
      equal(
        isReaderFeedResponse({
          items: [feedItem],
          snapshot_id: 'snapshot-duplicate-capabilities',
          mode: 'recommended',
          capabilities: ['snapshot', 'snapshot'],
        }),
        false,
      )
      equal(
        isReaderFeedResponse({
          items: [{ ...feedItem, item_type: 'inbox' }],
          snapshot_id: 'snapshot-invalid-union',
          mode: 'recommended',
        }),
        false,
      )
      equal(
        isReaderFeedResponse({
          items: [{ ...feedItem, event_at: undefined }],
          snapshot_id: 'snapshot-missing-event-at',
          mode: 'chronological',
        }),
        false,
      )
      equal(
        isReaderFeedResponse({
          items: [{
            key: 'link-legacy',
            source: 'reading',
            title: 'Legacy',
            summary: '',
            url: 'https://example.com/legacy',
            link_id: null,
            inbox_id: null,
            feed_item_id: null,
            read: false,
            read_later: false,
            saved: false,
            reason_code: 'legacy',
            reason_text: 'legacy',
            published_at: null,
            event_at: '2026-08-08T00:00:00Z',
            created_at: '2026-08-08T00:00:00Z',
          }],
          snapshot_id: 'snapshot-legacy',
          mode: 'recommended',
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
