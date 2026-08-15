import {
  MSG_RSS_DISCOVERY_QUERY,
  MSG_RSS_DISCOVERY_REPORT,
  type DiscoveredFeed,
  type FeedFormat,
  type RssDiscoveryQueryMessage,
  type RssDiscoverySnapshot,
  isWebFeedUrl,
} from '@/rss/protocol'

const FEED_MIME_TYPES: Readonly<Record<string, FeedFormat>> = {
  'application/rss+xml': 'rss',
  'application/atom+xml': 'atom',
  'application/rdf+xml': 'rdf',
}

function normalizeTitle(value: string | null | undefined): string {
  return value?.replace(/\s+/g, ' ').trim().slice(0, 240) ?? ''
}

function resolveFeedUrl(href: string, pageUrl: string): string | null {
  try {
    const url = new URL(href, pageUrl)
    if (url.protocol !== 'http:' && url.protocol !== 'https:') return null
    url.hash = ''
    return url.href
  } catch {
    return null
  }
}

function formatFromRoot(document: Document): FeedFormat | null {
  const rootName = document.documentElement?.localName.toLowerCase()
  if (rootName === 'rss') return 'rss'
  if (rootName === 'feed') return 'atom'
  if (rootName === 'rdf') return 'rdf'
  return null
}

function directFeedTitle(document: Document): string {
  const root = document.documentElement
  if (!root) return ''

  const title = [...root.getElementsByTagName('*')].find((element) => {
    if (element.localName.toLowerCase() !== 'title') return false
    if (root.localName.toLowerCase() === 'feed')
      return element.parentElement === root
    return element.parentElement?.localName.toLowerCase() === 'channel'
  })
  return normalizeTitle(title?.textContent)
}

function fallbackTitle(document: Document, pageUrl: string): string {
  const documentTitle = normalizeTitle(document.title)
  if (documentTitle) return documentTitle
  try {
    return new URL(pageUrl).hostname
  } catch {
    return ''
  }
}

/**
 * Discover declared RSS/Atom/RDF feeds without fetching anything. Relative
 * hrefs are resolved against the actual page URL, fragments are removed, and
 * duplicate URLs collapse to the first declaration.
 */
export function discoverFeeds(
  document: Document,
  pageUrl: string,
): DiscoveredFeed[] {
  if (!isWebFeedUrl(pageUrl)) return []

  const directFormat = formatFromRoot(document)
  if (directFormat) {
    return [
      {
        title: directFeedTitle(document) || fallbackTitle(document, pageUrl),
        url: resolveFeedUrl(pageUrl, pageUrl) ?? pageUrl,
        format: directFormat,
      },
    ]
  }

  const byUrl = new Map<string, DiscoveredFeed>()
  const pageTitle = fallbackTitle(document, pageUrl)
  for (const link of document.querySelectorAll<HTMLLinkElement>(
    'link[rel][href]',
  )) {
    const rels = link.rel.toLowerCase().split(/\s+/).filter(Boolean)
    if (!rels.includes('alternate')) continue

    const format = FEED_MIME_TYPES[link.type.trim().toLowerCase()]
    if (!format) continue
    const url = resolveFeedUrl(link.getAttribute('href') ?? '', pageUrl)
    if (!url || byUrl.has(url)) continue

    byUrl.set(url, {
      title: normalizeTitle(link.title) || pageTitle,
      url,
      format,
    })
  }
  return [...byUrl.values()]
}

function snapshotsEqual(
  left: RssDiscoverySnapshot | null,
  right: RssDiscoverySnapshot,
): boolean {
  return left !== null && JSON.stringify(left) === JSON.stringify(right)
}

/** Start discovery reporting in a manifest-injected content script. */
export function setupRssDiscovery(): void {
  if (window.__webtagRssDiscoveryInit) return
  window.__webtagRssDiscoveryInit = true

  let latest: RssDiscoverySnapshot | null = null
  let refreshTimer: ReturnType<typeof setTimeout> | undefined

  const snapshot = (): RssDiscoverySnapshot => ({
    pageUrl: window.location.href,
    feeds: discoverFeeds(document, window.location.href),
  })

  const report = (force = false): RssDiscoverySnapshot => {
    const next = snapshot()
    if (!force && snapshotsEqual(latest, next)) return next
    latest = next
    const message = { type: MSG_RSS_DISCOVERY_REPORT, ...next } as const
    void chrome.runtime.sendMessage(message).catch(() => {
      // The worker may be restarting or the extension may have been reloaded.
    })
    return next
  }

  const scheduleReport = (): void => {
    if (refreshTimer) clearTimeout(refreshTimer)
    refreshTimer = setTimeout(() => report(), 100)
  }

  chrome.runtime.onMessage.addListener(
    (message: unknown, _sender, sendResponse) => {
      if (
        typeof message === 'object' &&
        message !== null &&
        (message as RssDiscoveryQueryMessage).type === MSG_RSS_DISCOVERY_QUERY
      ) {
        sendResponse(report(true))
      }
    },
  )

  const startObserving = (): void => {
    report(true)
    const target = document.head ?? document.documentElement
    if (target) {
      new MutationObserver(scheduleReport).observe(target, {
        childList: true,
        subtree: true,
        attributes: true,
        attributeFilter: ['href', 'rel', 'type', 'title'],
      })
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', startObserving, {
      once: true,
    })
  } else {
    startObserving()
  }
  window.addEventListener('pageshow', () => report(true))
  window.addEventListener('popstate', scheduleReport)
}
