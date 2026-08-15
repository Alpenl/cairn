import {
  MSG_RSS_DISCOVERY_GET,
  type RssDiscoveryGetResponse,
  isRssDiscoveryGetResponse,
} from '@/rss/protocol'

/** Ask background for tab-scoped discovery, waking/re-querying content as needed. */
export async function getRssDiscovery(
  tabId: number,
  pageUrl: string,
): Promise<RssDiscoveryGetResponse> {
  try {
    const response: unknown = await chrome.runtime.sendMessage({
      type: MSG_RSS_DISCOVERY_GET,
      tabId,
      pageUrl,
    })
    return isRssDiscoveryGetResponse(response)
      ? response
      : { ok: false, error: 'unavailable' }
  } catch {
    return { ok: false, error: 'unavailable' }
  }
}
