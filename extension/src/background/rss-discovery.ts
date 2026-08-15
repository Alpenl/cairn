import {
  MSG_RSS_DISCOVERY_GET,
  MSG_RSS_DISCOVERY_QUERY,
  MSG_RSS_DISCOVERY_REPORT,
  type DiscoveredFeed,
  type RssDiscoveryGetMessage,
  type RssDiscoveryGetResponse,
  type RssDiscoveryReportMessage,
  type RssDiscoverySnapshot,
  isDiscoveredFeed,
  isRssDiscoverySnapshot,
  isWebFeedUrl,
} from '@/rss/protocol'

const BADGE_COLOR = '#b45309'

function sanitizeFeeds(feeds: unknown): DiscoveredFeed[] {
  if (!Array.isArray(feeds)) return []
  const byUrl = new Map<string, DiscoveredFeed>()
  for (const value of feeds) {
    if (!isDiscoveredFeed(value) || !isWebFeedUrl(value.url)) continue
    if (!byUrl.has(value.url)) byUrl.set(value.url, value)
  }
  return [...byUrl.values()]
}

function samePage(left: string, right: string): boolean {
  try {
    const a = new URL(left)
    const b = new URL(right)
    a.hash = ''
    b.hash = ''
    return a.href === b.href
  } catch {
    return left === right
  }
}

export class RssDiscoveryController {
  private readonly states = new Map<number, RssDiscoverySnapshot>()

  private async updateBadge(tabId: number, count: number): Promise<void> {
    try {
      if (count > 0) {
        await chrome.action.setBadgeBackgroundColor({
          tabId,
          color: BADGE_COLOR,
        })
      }
      await chrome.action.setBadgeText({ tabId, text: count > 0 ? 'RSS' : '' })
    } catch {
      // Tabs can disappear while async badge updates are in flight.
    }
  }

  report(tabId: number, message: RssDiscoveryReportMessage): void {
    const state: RssDiscoverySnapshot = {
      pageUrl: message.pageUrl,
      feeds: sanitizeFeeds(message.feeds),
    }
    this.states.set(tabId, state)
    void this.updateBadge(tabId, state.feeds.length)
  }

  clear(tabId: number): void {
    this.states.delete(tabId)
    void this.updateBadge(tabId, 0)
  }

  private async queryContentScript(
    tabId: number,
  ): Promise<RssDiscoverySnapshot | null> {
    try {
      const response: unknown = await chrome.tabs.sendMessage(tabId, {
        type: MSG_RSS_DISCOVERY_QUERY,
      })
      if (!isRssDiscoverySnapshot(response)) return null
      const state = {
        pageUrl: response.pageUrl,
        feeds: sanitizeFeeds(response.feeds),
      }
      this.states.set(tabId, state)
      await this.updateBadge(tabId, state.feeds.length)
      return state
    } catch {
      return null
    }
  }

  async get(tabId: number, pageUrl: string): Promise<RssDiscoveryGetResponse> {
    if (!isWebFeedUrl(pageUrl)) return { ok: false, error: 'restricted' }
    const cached = this.states.get(tabId)
    if (cached && samePage(cached.pageUrl, pageUrl)) {
      return { ok: true, state: cached }
    }
    const queried = await this.queryContentScript(tabId)
    if (!queried || !samePage(queried.pageUrl, pageUrl)) {
      return { ok: false, error: 'unavailable' }
    }
    return { ok: true, state: queried }
  }
}

export function setupRssDiscoveryMessaging(
  controller = new RssDiscoveryController(),
): RssDiscoveryController {
  chrome.runtime.onMessage.addListener(
    (message: unknown, sender, sendResponse) => {
      if (typeof message !== 'object' || message === null) return
      const type = (message as { type?: unknown }).type

      if (type === MSG_RSS_DISCOVERY_REPORT) {
        const tabId = sender.tab?.id
        const report = message as RssDiscoveryReportMessage
        if (
          tabId !== undefined &&
          typeof report.pageUrl === 'string' &&
          Array.isArray(report.feeds) &&
          (!sender.tab?.url || samePage(sender.tab.url, report.pageUrl))
        ) {
          controller.report(tabId, report)
        }
        return
      }

      if (type === MSG_RSS_DISCOVERY_GET) {
        const request = message as RssDiscoveryGetMessage
        if (
          Number.isInteger(request.tabId) &&
          typeof request.pageUrl === 'string'
        ) {
          void controller.get(request.tabId, request.pageUrl).then(sendResponse)
        } else {
          sendResponse({ ok: false, error: 'unavailable' })
        }
        return true
      }
    },
  )

  chrome.tabs.onUpdated.addListener((tabId, changeInfo) => {
    if (changeInfo.status === 'loading' || changeInfo.url)
      controller.clear(tabId)
  })
  chrome.tabs.onRemoved.addListener((tabId) => controller.clear(tabId))
  return controller
}
