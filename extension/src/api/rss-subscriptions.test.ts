import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createWebTagClient } from './webtag-client'

const fetchSpy = vi.fn()

beforeEach(() => {
  fetchSpy.mockReset()
  vi.stubGlobal('fetch', fetchSpy)
})

afterEach(() => vi.unstubAllGlobals())

function json(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), { status })
}

describe('WebTagClient RSS subscriptions', () => {
  it('checks canonical feed_url responses through the subscriptions envelope', async () => {
    fetchSpy.mockResolvedValue(
      json({
        subscriptions: [
          {
            id: 'sub-1',
            feed_url: 'https://example.com/feed.xml',
            title: 'Feed',
          },
        ],
      }),
    )
    const client = createWebTagClient({
      baseURL: 'https://api.test',
      token: 't',
    })
    await expect(
      client.findSubscriptionByUrl('https://example.com/feed.xml'),
    ).resolves.toEqual({
      ok: true,
      data: { id: 'sub-1', url: 'https://example.com/feed.xml', title: 'Feed' },
    })
    expect(fetchSpy.mock.calls[0][0]).toBe(
      'https://api.test/api/subscriptions?url=https%3A%2F%2Fexample.com%2Ffeed.xml',
    )
  })

  it('returns null for an empty subscription list', async () => {
    fetchSpy.mockResolvedValue(json({ items: [] }))
    const client = createWebTagClient({
      baseURL: 'https://api.test',
      token: '',
    })
    await expect(
      client.findSubscriptionByUrl('https://none.test/feed'),
    ).resolves.toEqual({ ok: true, data: null })
  })

  it('subscribes with an explicit POST and accepts a subscription wrapper', async () => {
    fetchSpy.mockResolvedValue(
      json({
        subscription: {
          id: 'sub-2',
          feed_url: 'https://example.com/atom.xml',
          title: 'Atom',
        },
      }),
    )
    const client = createWebTagClient({
      baseURL: 'https://api.test',
      token: 'secret',
    })
    await expect(
      client.createSubscription('https://example.com/atom.xml'),
    ).resolves.toEqual({
      ok: true,
      data: { id: 'sub-2', url: 'https://example.com/atom.xml', title: 'Atom' },
    })
    expect(fetchSpy.mock.calls[0][1]).toEqual(
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ url: 'https://example.com/atom.xml' }),
      }),
    )
  })

  it('fails closed on malformed successful responses', async () => {
    fetchSpy.mockResolvedValue(
      json({ subscriptions: [{ title: 'missing id' }] }),
    )
    const client = createWebTagClient({
      baseURL: 'https://api.test',
      token: '',
    })
    const result = await client.findSubscriptionByUrl(
      'https://example.com/feed',
    )
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error.kind).toBe('other')
  })
})
