import { beforeEach, describe, expect, it } from 'vitest'
import {
  clearFeedScrollAnchor,
  feedScrollAnchorKey,
  feedScrollAnchorTarget,
  measureFeedItems,
  parseFeedScrollAnchor,
  pickFeedScrollAnchor,
  readFeedScrollAnchor,
  writeFeedScrollAnchor,
  type FeedItemMetric,
} from './feed-scroll-anchor'

// One scroller, cards 200px tall, the viewport starting 120px below the page
// top. The reader is 180px into the second card.
const VIEWPORT_TOP = 120
const SCROLLED_METRICS: FeedItemMetric[] = [
  { key: 'link:a', top: -260, bottom: -60 },
  { key: 'link:b', top: -60, bottom: 140 },
  { key: 'link:c', top: 140, bottom: 340 },
]

beforeEach(() => {
  window.sessionStorage.clear()
})

describe('feed scroll anchor geometry', () => {
  it('anchors on the topmost card still intersecting the viewport', () => {
    expect(pickFeedScrollAnchor(SCROLLED_METRICS, VIEWPORT_TOP, 940)).toEqual({
      itemKey: 'link:b',
      offset: -180,
      scrollTop: 940,
    })
  })

  it('ignores the card order in the DOM when choosing the anchor', () => {
    const shuffled = [SCROLLED_METRICS[2], SCROLLED_METRICS[0], SCROLLED_METRICS[1]]
    expect(pickFeedScrollAnchor(shuffled, VIEWPORT_TOP, 940)?.itemKey).toBe('link:b')
  })

  it('anchors on the first card when the scroller sits at the top', () => {
    const metrics: FeedItemMetric[] = [
      { key: 'link:a', top: 120, bottom: 320 },
      { key: 'link:b', top: 320, bottom: 520 },
    ]
    expect(pickFeedScrollAnchor(metrics, VIEWPORT_TOP, 0)).toEqual({
      itemKey: 'link:a',
      offset: 0,
      scrollTop: 0,
    })
  })

  it('produces no anchor for a collapsed or emptied scroller', () => {
    expect(pickFeedScrollAnchor([], VIEWPORT_TOP, 940)).toBeNull()
    expect(pickFeedScrollAnchor(
      [{ key: 'link:a', top: 0, bottom: 0 }, { key: 'link:b', top: 0, bottom: 0 }],
      0,
      0,
    )).toBeNull()
  })

  it('scrolls the anchored card back to its offset instead of reusing the old scrollTop', () => {
    // The resumed snapshot merged an extra card above the anchor, so the stored
    // scrollTop no longer points at the same card.
    const restored: FeedItemMetric[] = [
      { key: 'link:extra', top: 120, bottom: 320 },
      { key: 'link:a', top: 320, bottom: 520 },
      { key: 'link:b', top: 520, bottom: 720 },
    ]
    const target = feedScrollAnchorTarget(
      { itemKey: 'link:b', offset: -180, scrollTop: 940 },
      restored,
      VIEWPORT_TOP,
      0,
    )
    expect(target).toBe(580)
    expect(target).not.toBe(940)
  })

  it('falls back to the key own scrollTop when the anchored card left the snapshot', () => {
    const restored: FeedItemMetric[] = [
      { key: 'link:a', top: 120, bottom: 320 },
      { key: 'link:c', top: 320, bottom: 520 },
    ]
    expect(feedScrollAnchorTarget(
      { itemKey: 'link:b', offset: -180, scrollTop: 940 },
      restored,
      VIEWPORT_TOP,
      0,
    )).toBe(940)
  })

  it('never asks the scroller for a negative position', () => {
    expect(feedScrollAnchorTarget(
      { itemKey: 'link:a', offset: 300, scrollTop: 0 },
      [{ key: 'link:a', top: 120, bottom: 320 }],
      VIEWPORT_TOP,
      0,
    )).toBe(0)
    expect(feedScrollAnchorTarget(
      { itemKey: 'missing', offset: 0, scrollTop: 0 },
      [],
      VIEWPORT_TOP,
      0,
    )).toBe(0)
  })

  it('rounds fractional geometry to the nearest settable scroll position', () => {
    expect(feedScrollAnchorTarget(
      { itemKey: 'link:a', offset: -10.4, scrollTop: 0 },
      [{ key: 'link:a', top: 200.2, bottom: 400.2 }],
      VIEWPORT_TOP,
      0,
    )).toBe(91)
  })
})

describe('feed scroll anchor keys', () => {
  const filter = ['inbox', 'reading']

  it('separates identity, mode and source filter', () => {
    const base = feedScrollAnchorKey('ns-a', 'recommended', filter)
    expect(base).not.toBeNull()
    expect(feedScrollAnchorKey('ns-b', 'recommended', filter)).not.toBe(base)
    expect(feedScrollAnchorKey('ns-a', 'chronological', filter)).not.toBe(base)
    expect(feedScrollAnchorKey('ns-a', 'recommended', ['inbox'])).not.toBe(base)
    expect(feedScrollAnchorKey('ns-a', 'recommended', [])).not.toBe(base)
  })

  it('normalizes the source filter so order and duplicates cannot fork a key', () => {
    expect(feedScrollAnchorKey('ns-a', 'recommended', ['reading', 'inbox', 'reading']))
      .toBe(feedScrollAnchorKey('ns-a', 'recommended', filter))
  })

  it('does not key a position without an identity namespace', () => {
    expect(feedScrollAnchorKey(null, 'recommended', filter)).toBeNull()
    expect(feedScrollAnchorKey('', 'recommended', filter)).toBeNull()
  })

  it('keeps the position of an identity whose namespace needs escaping distinct', () => {
    expect(feedScrollAnchorKey('ns:a', 'recommended', filter))
      .not.toBe(feedScrollAnchorKey('ns', 'a:recommended', filter))
  })
})

describe('feed scroll anchor storage', () => {
  const key = feedScrollAnchorKey('ns-a', 'recommended', ['inbox', 'reading']) as string

  it('round-trips an anchor and clears only the key it is given', () => {
    const other = feedScrollAnchorKey('ns-a', 'chronological', ['inbox', 'reading']) as string
    writeFeedScrollAnchor(key, { itemKey: 'link:b', offset: -180, scrollTop: 940 })
    writeFeedScrollAnchor(other, { itemKey: 'link:z', offset: 12, scrollTop: 30 })

    expect(readFeedScrollAnchor(key)).toEqual({ itemKey: 'link:b', offset: -180, scrollTop: 940 })

    clearFeedScrollAnchor(key)
    expect(readFeedScrollAnchor(key)).toBeNull()
    expect(readFeedScrollAnchor(other)).toEqual({ itemKey: 'link:z', offset: 12, scrollTop: 30 })
  })

  it('reads nothing for a key that was never written', () => {
    expect(readFeedScrollAnchor(key)).toBeNull()
  })

  it('rejects stored payloads that cannot describe a position', () => {
    expect(parseFeedScrollAnchor({ version: 1, item_key: 'link:b', offset: -180, scroll_top: 940 }))
      .toEqual({ itemKey: 'link:b', offset: -180, scrollTop: 940 })
    expect(parseFeedScrollAnchor({ version: 2, item_key: 'link:b', offset: -180, scroll_top: 940 })).toBeNull()
    expect(parseFeedScrollAnchor({ version: 1, item_key: '', offset: -180, scroll_top: 940 })).toBeNull()
    expect(parseFeedScrollAnchor({ version: 1, item_key: 'link:b', offset: 'top', scroll_top: 940 })).toBeNull()
    expect(parseFeedScrollAnchor({ version: 1, item_key: 'link:b', offset: Number.NaN, scroll_top: 940 })).toBeNull()
    expect(parseFeedScrollAnchor({ version: 1, item_key: 'link:b', offset: -180, scroll_top: -1 })).toBeNull()
    expect(parseFeedScrollAnchor({ version: 1, item_key: 'link:b', offset: -180 })).toBeNull()
    expect(parseFeedScrollAnchor(null)).toBeNull()
  })

  it('treats an unparsable stored value as no position', () => {
    window.sessionStorage.setItem(key, '{oops')
    expect(readFeedScrollAnchor(key)).toBeNull()
  })
})

describe('measureFeedItems', () => {
  it('measures every keyed card in the scroller', () => {
    const scroller = document.createElement('div')
    scroller.innerHTML = `
      <ul>
        <li data-feed-item-key="link:a"></li>
        <li data-feed-item-key="link:b"></li>
        <li class="not-a-card"></li>
      </ul>
    `
    document.body.append(scroller)
    const tops: Record<string, number> = { 'link:a': -60, 'link:b': 140 }
    for (const card of scroller.querySelectorAll<HTMLElement>('[data-feed-item-key]')) {
      const top = tops[card.getAttribute('data-feed-item-key') as string]
      card.getBoundingClientRect = () => ({ top, bottom: top + 200 }) as DOMRect
    }

    expect(measureFeedItems(scroller)).toEqual([
      { key: 'link:a', top: -60, bottom: 140 },
      { key: 'link:b', top: 140, bottom: 340 },
    ])
    scroller.remove()
  })
})
