import { describe, expect, it } from 'vitest'
import { feedItemSourceTitle, itemPreview, safeHTTPURL } from './feed'
import type { FeedItem } from './api/types'

function item(over: Partial<FeedItem>): FeedItem {
  return {
    id: 'item-1',
    subscription_id: 'feed-1',
    title: 'Title',
    url: 'https://example.com/post',
    ...over,
  }
}

describe('feed presentation helpers', () => {
  it('treats summary/content as plain text, including legitimate angle brackets', () => {
    expect(itemPreview(item({ summary: 'Generic Vector<T>\n keeps <T> literal' }))).toBe(
      'Generic Vector<T> keeps <T> literal',
    )
  })

  it('only exposes absolute HTTP(S) article URLs', () => {
    expect(safeHTTPURL('https://example.com/post')).toBe('https://example.com/post')
    expect(safeHTTPURL('javascript:alert(1)')).toBeNull()
    expect(safeHTTPURL('')).toBeNull()
  })

  it('keeps the source label when an inactive subscription is absent from navigation', () => {
    const preserved = item({ subscription_title: 'Archived source' })

    expect(feedItemSourceTitle(preserved)).toBe('Archived source')
    expect(
      feedItemSourceTitle(preserved, {
        id: 'feed-1',
        feed_url: 'https://example.com/feed.xml',
        title: 'Current source',
      }),
    ).toBe('Current source')
  })
})
