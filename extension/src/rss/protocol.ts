/**
 * RSS discovery messages shared by the content script, background worker and
 * popup. Keeping the protocol here prevents each context from inventing its
 * own string literals and wire shapes.
 */

export const MSG_RSS_DISCOVERY_REPORT = 'webtag:rss-discovery-report'
export const MSG_RSS_DISCOVERY_GET = 'webtag:rss-discovery-get'
export const MSG_RSS_DISCOVERY_QUERY = 'webtag:rss-discovery-query'

export type FeedFormat = 'rss' | 'atom' | 'rdf'

export interface DiscoveredFeed {
  title: string
  url: string
  format: FeedFormat
}

export interface RssDiscoverySnapshot {
  pageUrl: string
  feeds: DiscoveredFeed[]
}

/** Content script -> background: publish the latest discovery for its tab. */
export interface RssDiscoveryReportMessage extends RssDiscoverySnapshot {
  type: typeof MSG_RSS_DISCOVERY_REPORT
}

/** Popup -> background: get discovery for the tab currently shown in popup. */
export interface RssDiscoveryGetMessage {
  type: typeof MSG_RSS_DISCOVERY_GET
  tabId: number
  pageUrl: string
}

/** Background -> content script: recompute discovery after a worker restart. */
export interface RssDiscoveryQueryMessage {
  type: typeof MSG_RSS_DISCOVERY_QUERY
}

export type RssDiscoveryError = 'restricted' | 'unavailable'

export type RssDiscoveryGetResponse =
  | { ok: true; state: RssDiscoverySnapshot }
  | { ok: false; error: RssDiscoveryError }

export function isRssDiscoveryGetResponse(
  value: unknown,
): value is RssDiscoveryGetResponse {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    return false
  }
  const response = value as Partial<RssDiscoveryGetResponse>
  if (response.ok === true) return isRssDiscoverySnapshot(response.state)
  return (
    response.ok === false &&
    (response.error === 'restricted' || response.error === 'unavailable')
  )
}

function isFeedFormat(value: unknown): value is FeedFormat {
  return value === 'rss' || value === 'atom' || value === 'rdf'
}

export function isDiscoveredFeed(value: unknown): value is DiscoveredFeed {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    return false
  }
  const feed = value as Partial<DiscoveredFeed>
  return (
    typeof feed.title === 'string' &&
    typeof feed.url === 'string' &&
    isFeedFormat(feed.format)
  )
}

export function isRssDiscoverySnapshot(
  value: unknown,
): value is RssDiscoverySnapshot {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    return false
  }
  const state = value as Partial<RssDiscoverySnapshot>
  return (
    typeof state.pageUrl === 'string' &&
    Array.isArray(state.feeds) &&
    state.feeds.every(isDiscoveredFeed)
  )
}

export function isWebFeedUrl(raw: string): boolean {
  try {
    const url = new URL(raw)
    return url.protocol === 'http:' || url.protocol === 'https:'
  } catch {
    return false
  }
}
