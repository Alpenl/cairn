import { beforeEach, describe, expect, it, vi } from 'vitest'
import { MSG_RSS_DISCOVERY_GET } from '@/rss/protocol'
import { getRssDiscovery } from './useRssDiscovery'

const runtimeSendMessage = chrome.runtime.sendMessage as unknown as ReturnType<
  typeof vi.fn<(...args: unknown[]) => Promise<unknown>>
>

beforeEach(() => {
  runtimeSendMessage.mockReset()
})

describe('getRssDiscovery', () => {
  it('uses the centralized GET message and returns a valid snapshot', async () => {
    runtimeSendMessage.mockResolvedValue({
      ok: true,
      state: { pageUrl: 'https://example.com', feeds: [] },
    })
    await expect(getRssDiscovery(3, 'https://example.com')).resolves.toEqual({
      ok: true,
      state: { pageUrl: 'https://example.com', feeds: [] },
    })
    expect(runtimeSendMessage).toHaveBeenCalledWith({
      type: MSG_RSS_DISCOVERY_GET,
      tabId: 3,
      pageUrl: 'https://example.com',
    })
  })

  it('fails closed for rejected or malformed background responses', async () => {
    runtimeSendMessage.mockResolvedValue({ ok: true })
    await expect(getRssDiscovery(1, 'https://example.com')).resolves.toEqual({
      ok: false,
      error: 'unavailable',
    })
    runtimeSendMessage.mockRejectedValue(new Error('closed'))
    await expect(getRssDiscovery(1, 'https://example.com')).resolves.toEqual({
      ok: false,
      error: 'unavailable',
    })
  })
})
