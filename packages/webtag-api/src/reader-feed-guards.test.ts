import { describe, expect, it } from 'vitest'
import {
  isReaderFeedFeedbackResponse,
  isReaderFeedResponse,
} from './guards'
import type {
  ReaderFeedFeedbackResponse,
  ReaderFeedItemResponse,
} from './generated'

function feedItem(
  overrides: Partial<ReaderFeedItemResponse> = {},
): ReaderFeedItemResponse {
  return {
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
    event_at: '2026-08-10T01:00:00Z',
    ...overrides,
  }
}

describe('Reader feed response guards', () => {
  it('accepts valid recommended and chronological pages', () => {
    expect(
      isReaderFeedResponse({
        items: [feedItem()],
        next_cursor: 'live-cursor',
        mode: 'recommended',
      }),
    ).toBe(true)
    expect(
      isReaderFeedResponse({
        items: [feedItem()],
        mode: 'chronological',
      }),
    ).toBe(true)
  })

  it('rejects pages missing required item fields', () => {
    for (const field of ['resource_key', 'event_at']) {
      const item = { ...feedItem() } as Record<string, unknown>
      delete item[field]
      expect(
        isReaderFeedResponse({ items: [item], mode: 'recommended' }),
        field,
      ).toBe(false)
    }
  })

  it('rejects invalid enum values and pagination cursors', () => {
    expect(isReaderFeedResponse({ items: [], mode: 'ranking-v2' })).toBe(false)
    expect(
      isReaderFeedResponse({ items: [], mode: 'recommended', next_cursor: 1 }),
    ).toBe(false)
    expect(
      isReaderFeedResponse({
        items: [feedItem({ source: 'reading' })],
        mode: 'recommended',
      }),
    ).toBe(false)
  })

  it('rejects malformed nested item payloads', () => {
    expect(
      isReaderFeedResponse({
        items: [feedItem({ title: 7 as unknown as string })],
        mode: 'recommended',
      }),
    ).toBe(false)
    expect(
      isReaderFeedResponse({
        items: [feedItem({ read: 'false' as unknown as boolean })],
        mode: 'recommended',
      }),
    ).toBe(false)
  })

  it('accepts feedback with an optional visible link and rejects malformed payloads', () => {
    const response: ReaderFeedFeedbackResponse = {
      item_key: 'subscription:feed-1',
      action: 'save',
      link_id: 'link-1',
    }
    expect(isReaderFeedFeedbackResponse(response)).toBe(true)
    expect(isReaderFeedFeedbackResponse({ ...response, link_id: 7 })).toBe(
      false,
    )
    expect(isReaderFeedFeedbackResponse({ ...response, action: 'pin' })).toBe(
      false,
    )
  })
})
