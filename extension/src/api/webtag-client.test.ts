import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  buildWebTagClientFromSettings,
  createWebTagClient,
} from './webtag-client'
import type { IngestRequest, Link } from './types'

type FetchImpl = (input: string, init?: RequestInit) => Promise<Response>

let fetchImpl: FetchImpl
const fetchSpy = vi.fn((input: unknown, init?: unknown) =>
  fetchImpl(input as string, init as RequestInit | undefined),
)

function makeResponse(opts: {
  status?: number
  bodyText: string
  headers?: Record<string, string>
}): Response {
  return new Response(opts.bodyText, {
    status: opts.status ?? 200,
    headers: opts.headers,
  })
}

function jsonResponse(
  status: number,
  value: unknown,
  headers?: Record<string, string>,
): Response {
  return makeResponse({ status, bodyText: JSON.stringify(value), headers })
}

function sessionIdentity(namespace = 'namespace-a') {
  return {
    client_data_namespace: namespace,
    representation_contract: 'v3' as const,
  }
}

function sessionResponse(namespace = 'namespace-a'): Response {
  return jsonResponse(200, sessionIdentity(namespace), {
    'X-WebTag-Data-Namespace': namespace,
  })
}

beforeEach(() => {
  fetchSpy.mockClear()
  fetchImpl = () => Promise.resolve(makeResponse({ bodyText: '' }))
  vi.stubGlobal('fetch', fetchSpy)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

function lastUrl(): string {
  const call = fetchSpy.mock.calls[fetchSpy.mock.calls.length - 1]
  return call[0] as string
}

function lastInit(): RequestInit {
  const call = fetchSpy.mock.calls[fetchSpy.mock.calls.length - 1]
  return (call[1] as RequestInit) ?? {}
}

function makeWireLink(overrides: Partial<Link> = {}): Link {
  return {
    id: 'link-1',
    url: 'https://example.com/article',
    title: 'Example article',
    summary: null,
    description: null,
    tags: [],
    content_type: 'article',
    status: 'done',
    domain: 'example.com',
    path_depth: 1,
    parent_id: null,
    parent_path: '/',
    fetcher_type: null,
    is_low_confidence: false,
    has_content: false,
    low_confidence_reason: null,
    error_category: null,
    error_msg: null,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    metadata_revision: 1,
    ...overrides,
  }
}

describe('WebTagClient construction', () => {
  it('normalizes baseURL and injects the Bearer token', async () => {
    fetchImpl = () => Promise.resolve(sessionResponse())
    const client = createWebTagClient({
      baseURL: 'http://localhost:8080///',
      token: 'secret',
    })

    await client.getSessionIdentity()

    expect(lastUrl()).toBe('http://localhost:8080/api/session')
    expect((lastInit().headers as Record<string, string>).Authorization).toBe(
      'Bearer secret',
    )
  })

  it('does not inject Authorization for a blank token', async () => {
    fetchImpl = () => Promise.resolve(sessionResponse())
    const client = createWebTagClient({
      baseURL: 'http://localhost:8080',
      token: '   ',
    })

    await client.getSessionIdentity()

    expect(
      (lastInit().headers as Record<string, string>).Authorization,
    ).toBeUndefined()
  })
})

describe('request construction', () => {
  it('ingest posts JSON with a stable Idempotency-Key', async () => {
    fetchImpl = () =>
      Promise.resolve(jsonResponse(200, { link_id: 'l1', status: 'pending' }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const body: IngestRequest = {
      sources: [{ kind: 'url', url: 'http://p' }],
      destination: 'library',
      requested_library_kind: 'auto',
    }

    const res = await client.ingest(body, { idempotencyKey: 'capture-key-1' })

    expect(res.ok).toBe(true)
    expect(lastUrl()).toBe('http://x/api/ingest')
    expect(lastInit().method).toBe('POST')
    expect((lastInit().headers as Record<string, string>)['Content-Type']).toBe(
      'application/json',
    )
    expect(
      (lastInit().headers as Record<string, string>)['Idempotency-Key'],
    ).toBe('capture-key-1')
    expect(lastInit().body).toBe(JSON.stringify(body))
  })

  it('getLink and saveLinkContent encode path ids', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, makeWireLink()))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await client.getLink('a b')

    expect(lastUrl()).toBe('http://x/api/links/a%20b')

    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          link_id: 'link/1',
          content: 'body',
          content_document: '# body',
          content_format: 'markdown',
          fetcher_type: 'browser_capture',
          content_source: 'fetched',
          content_revision: 1,
        }),
      )

    await client.saveLinkContent('link/1')

    expect(lastUrl()).toBe('http://x/api/links/link%2F1/content')
    expect(lastInit().method).toBe('POST')
  })

  it('capabilities and session identity use their dedicated current probes', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          library_kinds: true,
          site_library: true,
          site_management: true,
          reader: { inbox: true },
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await client.getCapabilities()

    expect(lastUrl()).toBe('http://x/api/capabilities')

    fetchImpl = () => Promise.resolve(sessionResponse('namespace-b'))

    await client.getSessionIdentity()

    expect(lastUrl()).toBe('http://x/api/session')
  })
})

describe('error normalization', () => {
  it('maps 401/403 to unauthorized and preserves error_code', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(401, {
          error: {
            code: 401,
            error_code: 'unauthorized',
            message: 'bad token',
          },
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 'wrong' })

    const unauthorized = await client.getSessionIdentity()

    expect(unauthorized.ok).toBe(false)
    if (!unauthorized.ok) {
      expect(unauthorized.error.kind).toBe('unauthorized')
      expect(unauthorized.error.status).toBe(401)
      expect(unauthorized.error.errorCode).toBe('unauthorized')
      expect(unauthorized.error.message).toBe('bad token')
    }

    fetchImpl = () => Promise.resolve(jsonResponse(403, {}))

    const forbidden = await client.getSessionIdentity()

    expect(forbidden.ok).toBe(false)
    if (!forbidden.ok) expect(forbidden.error.kind).toBe('unauthorized')
  })

  it('maps network failures and aborts to user-visible error kinds', async () => {
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    fetchImpl = () => Promise.reject(new TypeError('Failed to fetch'))
    const network = await client.testConnection()
    expect(network.ok).toBe(false)
    if (!network.ok) expect(network.error.kind).toBe('network-unreachable')

    fetchImpl = () => Promise.reject(new DOMException('aborted', 'AbortError'))
    const timeout = await client.testConnection()
    expect(timeout.ok).toBe(false)
    if (!timeout.ok) expect(timeout.error.kind).toBe('timeout')
  })

  it('times out when response body consumption hangs', async () => {
    vi.useFakeTimers()
    try {
      fetchImpl = (_input, init) => {
        const signal = init?.signal
        const stalledResponse = {
          ok: true,
          status: 200,
          text: () =>
            new Promise<string>((_resolve, reject) => {
              signal?.addEventListener('abort', () => {
                reject(
                  new DOMException('The operation was aborted.', 'AbortError'),
                )
              })
            }),
        }
        return Promise.resolve(stalledResponse as unknown as Response)
      }
      const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

      const resPromise = client.testConnection()
      await vi.advanceTimersByTimeAsync(8000)
      const res = await resPromise

      expect(res.ok).toBe(false)
      if (!res.ok) expect(res.error.kind).toBe('timeout')
    } finally {
      vi.useRealTimers()
    }
  })

  it('maps 429 with Retry-After to rate-limited', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(
          429,
          {
            error: {
              code: 429,
              error_code: 'rate_limit_exceeded',
              message: 'try later',
            },
          },
          { 'Retry-After': '12' },
        ),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.testConnection()

    expect(res.ok).toBe(false)
    if (!res.ok) {
      expect(res.error.kind).toBe('rate-limited')
      expect(res.error.status).toBe(429)
      expect(res.error.errorCode).toBe('rate_limit_exceeded')
      expect(res.error.retryAfterSeconds).toBe(12)
    }
  })

  it('maps other HTTP and parse failures without throwing', async () => {
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(500, {
          error: {
            code: 500,
            error_code: 'internal_error',
            message: 'boom',
          },
        }),
      )

    const server = await client.getSessionIdentity()

    expect(server.ok).toBe(false)
    if (!server.ok) {
      expect(server.error.kind).toBe('other')
      expect(server.error.status).toBe(500)
      expect(server.error.errorCode).toBe('internal_error')
    }

    fetchImpl = () =>
      Promise.resolve(
        makeResponse({ status: 200, bodyText: '<html>Login</html>' }),
      )

    await expect(client.getSessionIdentity()).resolves.toMatchObject({
      ok: false,
    })
  })
})

describe('capability and identity probes', () => {
  it('represents capabilities 404 as an older backend', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(404, {}))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await expect(client.getCapabilities()).resolves.toEqual({
      ok: true,
      data: null,
    })
  })

  it('fails closed on malformed capabilities', async () => {
    fetchImpl = () =>
      Promise.resolve(jsonResponse(200, { library_kinds: true }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.getCapabilities()

    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('reads session identity and rejects namespace marker mismatch', async () => {
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    fetchImpl = () => Promise.resolve(sessionResponse())

    await expect(client.getSessionIdentity()).resolves.toEqual({
      ok: true,
      data: sessionIdentity(),
    })

    fetchImpl = () => Promise.resolve(jsonResponse(200, sessionIdentity()))
    const missing = await client.getSessionIdentity()
    expect(missing.ok).toBe(false)
    if (!missing.ok) expect(missing.error.kind).toBe('identity-mismatch')

    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, sessionIdentity('namespace-a'), {
          'X-WebTag-Data-Namespace': 'namespace-b',
        }),
      )
    const mismatch = await client.getSessionIdentity()
    expect(mismatch.ok).toBe(false)
    if (!mismatch.ok) expect(mismatch.error.kind).toBe('identity-mismatch')
  })

  it('fails closed on malformed session identity', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          client_data_namespace: '',
          representation_contract: 'v3',
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.getSessionIdentity()

    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('testConnection reuses the authenticated session probe', async () => {
    fetchImpl = () => Promise.resolve(sessionResponse())
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.testConnection()

    expect(res).toEqual({ ok: true, data: true })
    expect(lastUrl()).toBe('http://x/api/session')
    expect(lastInit().method).toBe('GET')
  })

  it('reads active-only Reader pending count and treats 404 as unsupported', async () => {
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, { counts: { inbox: 7, inbox_expired: 4 } }),
      )

    await expect(client.getReaderPendingCount()).resolves.toEqual({
      ok: true,
      data: 7,
    })

    fetchImpl = () => Promise.resolve(jsonResponse(404, {}))

    await expect(client.getReaderPendingCount()).resolves.toEqual({
      ok: true,
      data: null,
    })
  })

  it('fails closed on malformed Reader pending count', async () => {
    fetchImpl = () =>
      Promise.resolve(jsonResponse(200, { counts: { inbox: -1 } }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.getReaderPendingCount()

    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })
})

describe('capture endpoints', () => {
  it('returns a full LinkResponse from getLink', async () => {
    const link = makeWireLink()
    fetchImpl = () => Promise.resolve(jsonResponse(200, link))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await expect(client.getLink('link-1')).resolves.toEqual({
      ok: true,
      data: link,
    })
  })

  it('fails closed on malformed LinkResponse', async () => {
    const { status: _status, ...linkWithoutStatus } = makeWireLink()
    fetchImpl = () => Promise.resolve(jsonResponse(200, linkWithoutStatus))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.getLink('link-1')

    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('accepts library and Inbox SubmitResponse variants', async () => {
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    fetchImpl = () =>
      Promise.resolve(jsonResponse(200, { link_id: 'l1', status: 'pending' }))
    await expect(client.ingest({ sources: [] })).resolves.toEqual({
      ok: true,
      data: { link_id: 'l1', status: 'pending' },
    })

    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          inbox_id: 'inbox-1',
          destination: 'inbox',
          status: 'pending',
        }),
      )
    await expect(client.ingest({ sources: [] })).resolves.toEqual({
      ok: true,
      data: {
        inbox_id: 'inbox-1',
        destination: 'inbox',
        status: 'pending',
      },
    })
  })

  it.each([
    [
      'simultaneous link_id and inbox_id',
      { link_id: 'l1', inbox_id: 'i1', status: 'done' },
    ],
    ['missing durable identity', { status: 'done' }],
    ['empty durable identity', { link_id: ' ', status: 'done' }],
    [
      'Inbox response with link_id',
      { link_id: 'l1', destination: 'inbox', status: 'done' },
    ],
    [
      'library response with inbox_id',
      { inbox_id: 'i1', destination: 'library', status: 'done' },
    ],
    ['invalid status', { link_id: 'l1', status: 'queued' }],
  ])('rejects invalid SubmitResponse: %s', async (_label, response) => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, response))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.ingest({ sources: [] })

    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('retries one ambiguous ingest failure with the same Idempotency-Key', async () => {
    let calls = 0
    fetchImpl = (_input, init) => {
      calls += 1
      if (calls === 1) return Promise.reject(new TypeError('Failed to fetch'))
      expect((init?.headers as Record<string, string>)['Idempotency-Key']).toBe(
        'capture-key-2',
      )
      return Promise.resolve(
        jsonResponse(200, { link_id: 'l1', status: 'done' }),
      )
    }
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const result = await client.ingest(
      { sources: [{ kind: 'browser_capture', url: 'https://example.com' }] },
      { idempotencyKey: 'capture-key-2' },
    )

    expect(result.ok).toBe(true)
    expect(calls).toBe(2)
  })

  it('does not retry explicit business errors', async () => {
    let calls = 0
    fetchImpl = () => {
      calls += 1
      return Promise.resolve(jsonResponse(422, { error: { code: 422 } }))
    }
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const result = await client.ingest({ sources: [] })

    expect(result.ok).toBe(false)
    expect(calls).toBe(1)
  })

  it('saves captured original content and validates the response', async () => {
    const response = {
      link_id: 'link/1',
      content: 'body',
      content_document: '# body',
      content_format: 'markdown' as const,
      fetcher_type: 'browser_capture',
      content_source: 'fetched' as const,
      content_revision: 1,
    }
    fetchImpl = () => Promise.resolve(jsonResponse(200, response))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await expect(client.saveLinkContent('link/1')).resolves.toEqual({
      ok: true,
      data: response,
    })

    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          link_id: 'link-1',
          content: 'body',
          fetcher_type: 'browser_capture',
        }),
      )
    const malformed = await client.saveLinkContent('link-1')
    expect(malformed.ok).toBe(false)
    if (!malformed.ok) expect(malformed.error.kind).toBe('other')
  })
})

describe('buildWebTagClientFromSettings', () => {
  it('returns null when backendUrl is blank', () => {
    expect(
      buildWebTagClientFromSettings({ backendUrl: '', accessToken: 't' }),
    ).toBeNull()
    expect(
      buildWebTagClientFromSettings({ backendUrl: '   ', accessToken: 't' }),
    ).toBeNull()
  })

  it('trims backendUrl and token', async () => {
    fetchImpl = () => Promise.resolve(sessionResponse())
    const client = buildWebTagClientFromSettings({
      backendUrl: '  http://localhost:8080  ',
      accessToken: '  secret  ',
    })

    expect(client).not.toBeNull()
    await client!.getSessionIdentity()

    expect(lastUrl()).toBe('http://localhost:8080/api/session')
    expect((lastInit().headers as Record<string, string>).Authorization).toBe(
      'Bearer secret',
    )
  })
})
