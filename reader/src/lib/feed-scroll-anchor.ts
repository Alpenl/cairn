/**
 * Feed scroll position, expressed as a stable item anchor.
 *
 * A raw `scrollTop` is not a position in the Feed: the snapshot that gets
 * restored can differ in height from the one that was left (a confirmed Inbox
 * card disappears, a resumed page merges an extra item), so the same pixel
 * offset lands on a different card. The card key plus its offset inside the
 * viewport survives those changes; `scrollTop` is kept only as the fallback for
 * the case where the anchored card is no longer in the snapshot at all.
 */

import { isRecord } from './records'

export interface FeedScrollAnchor {
  /** Key of the first card still intersecting the viewport when the reader left. */
  readonly itemKey: string
  /** Signed distance from the viewport top to that card's top, in CSS pixels. */
  readonly offset: number
  /** Position of the scroller itself, used when `itemKey` is gone. */
  readonly scrollTop: number
}

/** One rendered card's vertical extent in viewport coordinates. */
export interface FeedItemMetric {
  readonly key: string
  readonly top: number
  readonly bottom: number
}

export const FEED_ITEM_KEY_ATTRIBUTE = 'data-feed-item-key'

const FEED_SCROLL_ANCHOR_PREFIX = 'webtag:reader:mixed-feed:v1'
const FEED_SCROLL_ANCHOR_VERSION = 1

function sessionStorageOrNull(): Storage | null {
  if (typeof window === 'undefined') return null
  try {
    return window.sessionStorage
  } catch {
    return null
  }
}

/**
 * Storage key for one Feed position.
 *
 * The tuple is identity namespace + mode + source filter, and the filter is
 * normalized here rather than by the caller so two callers cannot disagree
 * about what "the same filter" means. Without an identity namespace there is no
 * key at all: a position must never leak across identities.
 */
export function feedScrollAnchorKey(
  namespace: string | null | undefined,
  mode: string,
  sourceFilter: readonly string[],
): string | null {
  if (!namespace) return null
  const filter = [...new Set(sourceFilter)].sort().join(',') || 'none'
  return `${FEED_SCROLL_ANCHOR_PREFIX}:${encodeURIComponent(namespace)}:${encodeURIComponent(mode)}:${filter}:scroll`
}

export function parseFeedScrollAnchor(value: unknown): FeedScrollAnchor | null {
  if (!isRecord(value)) return null
  if (value.version !== FEED_SCROLL_ANCHOR_VERSION) return null
  const { item_key: itemKey, offset, scroll_top: scrollTop } = value
  if (typeof itemKey !== 'string' || itemKey.length === 0) return null
  if (typeof offset !== 'number' || !Number.isFinite(offset)) return null
  if (typeof scrollTop !== 'number' || !Number.isFinite(scrollTop) || scrollTop < 0) return null
  return { itemKey, offset, scrollTop }
}

export function readFeedScrollAnchor(key: string): FeedScrollAnchor | null {
  const storage = sessionStorageOrNull()
  if (!storage) return null
  try {
    const raw = storage.getItem(key)
    if (!raw) return null
    return parseFeedScrollAnchor(JSON.parse(raw))
  } catch {
    // A malformed or unavailable position is the same as never having one.
    return null
  }
}

export function writeFeedScrollAnchor(key: string, anchor: FeedScrollAnchor): void {
  const storage = sessionStorageOrNull()
  if (!storage) return
  try {
    storage.setItem(key, JSON.stringify({
      version: FEED_SCROLL_ANCHOR_VERSION,
      item_key: anchor.itemKey,
      offset: anchor.offset,
      scroll_top: anchor.scrollTop,
    }))
  } catch {
    // Session storage is a best-effort position cache.
  }
}

export function clearFeedScrollAnchor(key: string): void {
  const storage = sessionStorageOrNull()
  if (!storage) return
  try {
    storage.removeItem(key)
  } catch {
    // Session storage is a best-effort position cache.
  }
}

/**
 * The card the reader is actually looking at is the topmost one still
 * intersecting the viewport — a card scrolled fully past the top is gone from
 * view and must not win. A collapsed or detached scroller produces no
 * intersecting card, which is why this returns null instead of an anchor
 * pointing at nothing.
 */
export function pickFeedScrollAnchor(
  metrics: readonly FeedItemMetric[],
  viewportTop: number,
  scrollTop: number,
): FeedScrollAnchor | null {
  let best: FeedItemMetric | null = null
  for (const metric of metrics) {
    if (metric.bottom <= viewportTop) continue
    if (best === null || metric.top < best.top) best = metric
  }
  if (best === null) return null
  return { itemKey: best.key, offset: best.top - viewportTop, scrollTop }
}

/**
 * Scroller position that puts the anchored card back where it was. The measured
 * geometry is relative to the scroller's *current* position, so the delta is
 * applied to the current `scrollTop` rather than assuming the snapshot renders
 * at the same height it had before.
 */
export function feedScrollAnchorTarget(
  anchor: FeedScrollAnchor,
  metrics: readonly FeedItemMetric[],
  viewportTop: number,
  scrollTop: number,
): number {
  const metric = metrics.find((candidate) => candidate.key === anchor.itemKey)
  const target = metric
    ? scrollTop + (metric.top - viewportTop) - anchor.offset
    : anchor.scrollTop
  // Chromium quantizes Element.scrollTop; select the nearest settable offset
  // instead of letting a truncated fractional target drift past the tolerance.
  return Math.max(0, Math.round(target))
}

export function measureFeedItems(container: HTMLElement): FeedItemMetric[] {
  const metrics: FeedItemMetric[] = []
  for (const element of container.querySelectorAll(`[${FEED_ITEM_KEY_ATTRIBUTE}]`)) {
    const key = element.getAttribute(FEED_ITEM_KEY_ATTRIBUTE)
    if (!key) continue
    const rect = element.getBoundingClientRect()
    metrics.push({ key, top: rect.top, bottom: rect.bottom })
  }
  return metrics
}
