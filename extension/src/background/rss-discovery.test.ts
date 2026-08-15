import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  MSG_RSS_DISCOVERY_GET,
  MSG_RSS_DISCOVERY_QUERY,
  MSG_RSS_DISCOVERY_REPORT,
} from '@/rss/protocol'
import {
  RssDiscoveryController,
  setupRssDiscoveryMessaging,
} from './rss-discovery'

const tabsSendMessage = chrome.tabs.sendMessage as unknown as ReturnType<
  typeof vi.fn<(...args: unknown[]) => Promise<unknown>>
>

const feed = {
  title: 'Example feed',
  url: 'https://example.com/feed.xml',
  format: 'rss' as const,
}

beforeEach(() => {
  vi.mocked(chrome.action.setBadgeText).mockClear()
  vi.mocked(chrome.action.setBadgeBackgroundColor).mockClear()
  tabsSendMessage.mockReset()
  vi.mocked(chrome.runtime.onMessage.addListener).mockClear()
  vi.mocked(chrome.tabs.onUpdated.addListener).mockClear()
  vi.mocked(chrome.tabs.onRemoved.addListener).mockClear()
})

describe('RssDiscoveryController', () => {
  it('keeps state per tab and sets or clears the RSS badge', async () => {
    const controller = new RssDiscoveryController()
    controller.report(7, {
      type: MSG_RSS_DISCOVERY_REPORT,
      pageUrl: 'https://example.com',
      feeds: [feed],
    })
    await Promise.resolve()
    await Promise.resolve()
    expect(chrome.action.setBadgeText).toHaveBeenCalledWith({
      tabId: 7,
      text: 'RSS',
    })
    expect(await controller.get(7, 'https://example.com#section')).toEqual({
      ok: true,
      state: { pageUrl: 'https://example.com', feeds: [feed] },
    })

    controller.clear(7)
    await Promise.resolve()
    expect(chrome.action.setBadgeText).toHaveBeenLastCalledWith({
      tabId: 7,
      text: '',
    })
  })

  it('re-queries content when a restarted worker has no state', async () => {
    tabsSendMessage.mockResolvedValue({
      pageUrl: 'https://example.com',
      feeds: [feed],
    })
    const result = await new RssDiscoveryController().get(
      9,
      'https://example.com',
    )
    expect(tabsSendMessage).toHaveBeenCalledWith(9, {
      type: MSG_RSS_DISCOVERY_QUERY,
    })
    expect(result).toEqual({
      ok: true,
      state: { pageUrl: 'https://example.com', feeds: [feed] },
    })
  })
})

describe('setupRssDiscoveryMessaging', () => {
  it('routes reports by sender tab and asynchronously answers popup GET', async () => {
    const controller = new RssDiscoveryController()
    const reportSpy = vi.spyOn(controller, 'report')
    const getSpy = vi.spyOn(controller, 'get').mockResolvedValue({
      ok: true,
      state: { pageUrl: 'https://example.com', feeds: [feed] },
    })
    setupRssDiscoveryMessaging(controller)
    const listener = vi.mocked(chrome.runtime.onMessage.addListener).mock
      .calls[0][0]

    listener(
      {
        type: MSG_RSS_DISCOVERY_REPORT,
        pageUrl: 'https://example.com',
        feeds: [feed],
      },
      { tab: { id: 4 } } as chrome.runtime.MessageSender,
      vi.fn(),
    )
    expect(reportSpy).toHaveBeenCalledWith(
      4,
      expect.objectContaining({ feeds: [feed] }),
    )

    listener(
      {
        type: MSG_RSS_DISCOVERY_REPORT,
        pageUrl: 'https://old.example.com',
        feeds: [feed],
      },
      {
        tab: { id: 4, url: 'https://new.example.com' },
      } as chrome.runtime.MessageSender,
      vi.fn(),
    )
    expect(reportSpy).toHaveBeenCalledTimes(1)

    const sendResponse = vi.fn()
    const keepChannel = listener(
      {
        type: MSG_RSS_DISCOVERY_GET,
        tabId: 4,
        pageUrl: 'https://example.com',
      },
      {} as chrome.runtime.MessageSender,
      sendResponse,
    )
    expect(keepChannel).toBe(true)
    await Promise.resolve()
    expect(getSpy).toHaveBeenCalledWith(4, 'https://example.com')
    expect(sendResponse).toHaveBeenCalledWith(
      expect.objectContaining({ ok: true }),
    )
  })
})
