import { describe, expect, it } from 'vitest'
import {
  isReaderFeedFeedbackResponse,
  isReaderFeedItemResponse,
  isReaderFeedResponse,
  isReaderInboxListItemResponse,
  isReaderInboxResponsePage,
} from './index'

const now = '2026-08-08T00:00:00Z'

function inboxListItem(): Record<string, unknown> {
  return {
    id: '00000000-0000-0000-0000-000000000001',
    url: 'https://example.com/capture',
    source_kind: 'browser_capture',
    title: null,
    preview: 'Captured summary',
    tags: ['shared'],
    status: 'pending',
    metadata_revision: 1,
    expired: false,
    updated_at: now,
  }
}

function feedItem(
  overrides: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    key: 'link:00000000-0000-0000-0000-000000000101',
    source: 'reading',
    resource_key: 'link:00000000-0000-0000-0000-000000000101',
    title: 'Captured article',
    summary: 'Captured summary',
    url: 'https://example.com/article',
    link_id: '00000000-0000-0000-0000-000000000101',
    inbox_id: null,
    feed_item_id: null,
    read: false,
    read_later: false,
    saved: false,
    event_at: now,
    ...overrides,
  }
}

describe('reader runtime guards', () => {
  it('validates Reader Inbox list pages from the public API export', () => {
    const item = inboxListItem()

    expect(isReaderInboxListItemResponse(item)).toBe(true)
    expect(
      isReaderInboxResponsePage({
        items: [item],
        next_cursor: 'live-cursor-1',
        active_count: 1,
        expired_count: 0,
      }),
    ).toBe(true)
    expect(isReaderInboxListItemResponse({ ...item, preview: undefined })).toBe(false)
    expect(
      isReaderInboxResponsePage({
        items: [item],
        next_cursor: 1,
        active_count: 1,
        expired_count: 0,
      }),
    ).toBe(false)
    expect(
      isReaderInboxResponsePage({
        items: [item],
        active_count: -1,
        expired_count: 0,
      }),
    ).toBe(false)
  })

  it('validates source-specific Reader Feed items and pages', () => {
    expect(isReaderFeedItemResponse(feedItem())).toBe(true)
    expect(
      isReaderFeedItemResponse(
        feedItem({
          key: 'inbox:00000000-0000-0000-0000-000000000201',
          source: 'inbox',
          resource_key: 'inbox:00000000-0000-0000-0000-000000000201',
          link_id: null,
          inbox_id: '00000000-0000-0000-0000-000000000201',
        }),
      ),
    ).toBe(true)
    expect(
      isReaderFeedItemResponse(
        feedItem({
          key: 'subscription:00000000-0000-0000-0000-000000000301',
          source: 'subscription',
          resource_key: 'subscription:00000000-0000-0000-0000-000000000301',
          link_id: null,
          feed_item_id: '00000000-0000-0000-0000-000000000301',
        }),
      ),
    ).toBe(true)
    expect(isReaderFeedItemResponse(feedItem({ link_id: null }))).toBe(false)
    expect(
      isReaderFeedItemResponse(feedItem({ source: 'subscription', inbox_id: 'inbox-1' })),
    ).toBe(false)
    expect(
      isReaderFeedResponse({
        items: [feedItem()],
        mode: 'recommended',
      }),
    ).toBe(true)
    expect(
      isReaderFeedResponse({
        items: [feedItem()],
        mode: 'chronological',
        next_cursor: 'live-cursor-2',
      }),
    ).toBe(true)
    expect(isReaderFeedResponse({ items: [], mode: 'ranking-v2' })).toBe(false)
    expect(
      isReaderFeedResponse({
        items: [feedItem()],
        mode: 'recommended',
        next_cursor: 1,
      }),
    ).toBe(false)
  })

  it('validates Reader Feed feedback responses', () => {
    expect(
      isReaderFeedFeedbackResponse({
        item_key: 'subscription:00000000-0000-0000-0000-000000000301',
        action: 'save',
        link_id: '00000000-0000-0000-0000-000000000101',
      }),
    ).toBe(true)
    expect(
      isReaderFeedFeedbackResponse({
        item_key: 'subscription:00000000-0000-0000-0000-000000000301',
        action: 'unsave',
      }),
    ).toBe(true)
    expect(
      isReaderFeedFeedbackResponse({
        item_key: 'subscription:00000000-0000-0000-0000-000000000301',
        action: 'archive',
      }),
    ).toBe(false)
    expect(
      isReaderFeedFeedbackResponse({
        item_key: 'subscription:00000000-0000-0000-0000-000000000301',
        action: 'hide',
        link_id: null,
      }),
    ).toBe(false)
  })
})
