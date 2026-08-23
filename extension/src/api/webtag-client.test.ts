import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  buildWebTagClientFromSettings,
  createWebTagClient,
} from './webtag-client'
import type {
  CapabilitiesResponse,
  IngestRequest,
  Link,
  LinkContentResponse,
  SubmitResponse,
} from './types'

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

beforeEach(() => {
  fetchSpy.mockClear()
  fetchImpl = () => Promise.resolve(makeResponse({ bodyText: '' }))
  vi.stubGlobal('fetch', fetchSpy)
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

function lastUrl(): string {
  const call = fetchSpy.mock.calls[fetchSpy.mock.calls.length - 1]
  return call?.[0] as string
}

function lastInit(): RequestInit {
  const call = fetchSpy.mock.calls[fetchSpy.mock.calls.length - 1]
  return (call?.[1] as RequestInit | undefined) ?? {}
}

function capabilities(
  overrides: Partial<CapabilitiesResponse> = {},
): CapabilitiesResponse {
  return {
    library_kinds: true,
    site_library: true,
    site_management: true,
    site_advanced_management: true,
    archive_versions: [1, 2],
    reader_vnext: true,
    reader: {
      annotations: true,
      notes: true,
      inbox: true,
      todos: true,
      engagement: true,
      home: true,
      feed: true,
      ai: true,
      related_tags: true,
      activity: true,
      history: true,
      trash: true,
    },
    ...overrides,
  }
}

function link(overrides: Partial<Link> = {}): Link {
  return {
    id: '00000000-0000-0000-0000-000000000001',
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

function librarySubmit(
  overrides: Partial<SubmitResponse> = {},
): SubmitResponse {
  return {
    link_id: '00000000-0000-0000-0000-000000000001',
    destination: 'library',
    status: 'done',
    ...overrides,
  }
}

function inboxSubmit(overrides: Partial<SubmitResponse> = {}): SubmitResponse {
  return {
    inbox_id: '00000000-0000-0000-0000-000000000002',
    destination: 'inbox',
    status: 'pending',
    ...overrides,
  }
}

function linkContent(
  overrides: Partial<LinkContentResponse> = {},
): LinkContentResponse {
  return {
    link_id: '00000000-0000-0000-0000-000000000001',
    content: 'Captured text',
    content_document: 'Captured text',
    content_format: 'markdown',
    fetcher_type: 'browser_capture',
    content_source: 'fetched',
    content_revision: 2,
    ...overrides,
  }
}

describe('WebTagClient construction', () => {
  it('normalizes baseURL and injects Bearer authorization', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, capabilities()))
    const client = createWebTagClient({
      baseURL: 'http://localhost:8080///',
      token: ' secret ',
    })

    await client.getCapabilities()

    expect(lastUrl()).toBe('http://localhost:8080/api/capabilities')
    expect((lastInit().headers as Record<string, string>).Authorization).toBe(
      'Bearer secret',
    )
  })

  it('omits Authorization when the token is blank', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, capabilities()))
    const client = createWebTagClient({ baseURL: 'http://x', token: '   ' })

    await client.getCapabilities()

    expect(lastInit().headers).toEqual({})
  })
})

describe('current request construction', () => {
  it('getLink encodes the path parameter and validates a full LinkResponse', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, link()))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await expect(client.getLink('a b')).resolves.toEqual({
      ok: true,
      data: link(),
    })
    expect(lastUrl()).toBe('http://x/api/links/a%20b')
  })

  it('ingest posts JSON and preserves a caller supplied Idempotency-Key', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, librarySubmit()))
    const body: IngestRequest = {
      destination: 'library',
      sources: [
        {
          kind: 'browser_capture',
          url: 'https://example.com/article',
          title: 'Example',
          text: 'Captured text',
          html: '<article>Captured text</article>',
        },
      ],
    }
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const result = await client.ingest(body, { idempotencyKey: 'capture-1' })

    expect(result).toEqual({ ok: true, data: librarySubmit() })
    expect(lastUrl()).toBe('http://x/api/ingest')
    expect(lastInit()).toEqual(
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify(body),
      }),
    )
    expect(
      (lastInit().headers as Record<string, string>)['Idempotency-Key'],
    ).toBe('capture-1')
  })

  it('ingest retries one ambiguous network failure with the same key', async () => {
    fetchImpl = vi
      .fn<FetchImpl>()
      .mockReturnValueOnce(Promise.reject(new TypeError('Failed to fetch')))
      .mockReturnValueOnce(Promise.resolve(jsonResponse(200, inboxSubmit())))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const result = await client.ingest(
      { sources: [{ kind: 'browser_capture', url: 'https://x.test' }] },
      { idempotencyKey: 'capture-2' },
    )

    expect(result).toEqual({ ok: true, data: inboxSubmit() })
    expect(fetchSpy).toHaveBeenCalledTimes(2)
    expect(
      fetchSpy.mock.calls.map(
        (call) =>
          ((call[1] as RequestInit).headers as Record<string, string>)[
            'Idempotency-Key'
          ],
      ),
    ).toEqual(['capture-2', 'capture-2'])
  })

  it('saveLinkContent posts to the encoded content endpoint', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, linkContent()))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await expect(client.saveLinkContent('a b')).resolves.toEqual({
      ok: true,
      data: linkContent(),
    })
    expect(lastUrl()).toBe('http://x/api/links/a%20b/content')
    expect(lastInit().method).toBe('POST')
  })

  it('getReaderPendingCount reads only active Inbox count from Reader Home', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          counts: { inbox: 3, inbox_expired: 9 },
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await expect(client.getReaderPendingCount()).resolves.toEqual({
      ok: true,
      data: 3,
    })
    expect(lastUrl()).toBe('http://x/api/home')
  })
})

describe('error normalization', () => {
  it('maps 401 and 403 to unauthorized', async () => {
    const client = createWebTagClient({ baseURL: 'http://x', token: 'wrong' })
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(401, {
          error: { code: 401, error_code: 'unauthorized', message: 'bad' },
        }),
      )
    await expect(client.getCapabilities()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'unauthorized', status: 401 },
    })

    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(403, {
          error: { code: 403, error_code: 'forbidden', message: 'bad' },
        }),
      )
    await expect(client.getCapabilities()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'unauthorized', status: 403 },
    })
  })

  it('preserves 429 Retry-After details', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(
          429,
          {
            error: {
              code: 429,
              error_code: 'rate_limit_exceeded',
              message: 'slow down',
            },
          },
          { 'Retry-After': '12' },
        ),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await expect(client.getCapabilities()).resolves.toMatchObject({
      ok: false,
      error: {
        kind: 'rate-limited',
        status: 429,
        errorCode: 'rate_limit_exceeded',
        retryAfterSeconds: 12,
      },
    })
  })

  it('maps fetch TypeError to network-unreachable', async () => {
    fetchImpl = () => Promise.reject(new TypeError('Failed to fetch'))
    const client = createWebTagClient({ baseURL: 'http://nope', token: 't' })

    await expect(client.getCapabilities()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'network-unreachable' },
    })
  })

  it('times out while fetch is still pending', async () => {
    vi.useFakeTimers()
    fetchImpl = (_input, init) =>
      new Promise((_resolve, reject) => {
        init?.signal?.addEventListener('abort', () => {
          reject(new DOMException('Aborted', 'AbortError'))
        })
      })
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const resultPromise = client.getCapabilities()
    await vi.advanceTimersByTimeAsync(8000)

    await expect(resultPromise).resolves.toMatchObject({
      ok: false,
      error: { kind: 'timeout' },
    })
  })

  it('fails closed on non-JSON successful responses', async () => {
    fetchImpl = () =>
      Promise.resolve(makeResponse({ status: 200, bodyText: '<html />' }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await expect(client.getCapabilities()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'other' },
    })
  })
})

describe('runtime response guards', () => {
  it('accepts capabilities and treats 404 as an older backend', async () => {
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    fetchImpl = () => Promise.resolve(jsonResponse(200, capabilities()))
    await expect(client.getCapabilities()).resolves.toEqual({
      ok: true,
      data: capabilities(),
    })

    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(404, {
          error: { code: 404, error_code: 'not_found', message: 'missing' },
        }),
      )
    await expect(client.getCapabilities()).resolves.toEqual({
      ok: true,
      data: null,
    })
  })

  it('fails closed on malformed capabilities', async () => {
    fetchImpl = () =>
      Promise.resolve(jsonResponse(200, { library_kinds: 'true' }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await expect(client.getCapabilities()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'other' },
    })
  })

  it('requires the session identity namespace marker to match the body', async () => {
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(
          200,
          {
            client_data_namespace: 'namespace-a',
            representation_contract: 'v3',
          },
          { 'X-WebTag-Data-Namespace': 'namespace-a' },
        ),
      )

    await expect(client.getSessionIdentity()).resolves.toEqual({
      ok: true,
      data: {
        client_data_namespace: 'namespace-a',
        representation_contract: 'v3',
      },
    })

    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          client_data_namespace: 'namespace-a',
          representation_contract: 'v3',
        }),
      )
    await expect(client.getSessionIdentity()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'identity-mismatch' },
    })
  })

  it('fails closed on malformed Link and LinkContent payloads', async () => {
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    fetchImpl = () =>
      Promise.resolve(jsonResponse(200, { ...link(), tags: null }))
    await expect(client.getLink('l1')).resolves.toMatchObject({
      ok: false,
      error: { kind: 'other' },
    })

    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, { ...linkContent(), content_format: null }),
      )
    await expect(client.saveLinkContent('l1')).resolves.toMatchObject({
      ok: false,
      error: { kind: 'other' },
    })
  })

  it('accepts valid library and Inbox submit responses', async () => {
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    fetchImpl = () => Promise.resolve(jsonResponse(200, librarySubmit()))
    await expect(
      client.ingest({ sources: [{ kind: 'browser_capture' }] }),
    ).resolves.toEqual({ ok: true, data: librarySubmit() })

    fetchImpl = () => Promise.resolve(jsonResponse(200, inboxSubmit()))
    await expect(
      client.ingest({ sources: [{ kind: 'browser_capture' }] }),
    ).resolves.toEqual({ ok: true, data: inboxSubmit() })
  })

  it('rejects ambiguous submit identities', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          ...librarySubmit(),
          inbox_id: '00000000-0000-0000-0000-000000000002',
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await expect(
      client.ingest({ sources: [{ kind: 'browser_capture' }] }),
    ).resolves.toMatchObject({
      ok: false,
      error: { kind: 'other' },
    })
  })
})

describe('testConnection', () => {
  it('uses the capabilities probe and returns ok for current or older servers', async () => {
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    fetchImpl = () => Promise.resolve(jsonResponse(200, capabilities()))
    await expect(client.testConnection()).resolves.toEqual({
      ok: true,
      data: true,
    })
    expect(lastUrl()).toBe('http://x/api/capabilities')

    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(404, {
          error: { code: 404, error_code: 'not_found', message: 'missing' },
        }),
      )
    await expect(client.testConnection()).resolves.toEqual({
      ok: true,
      data: true,
    })
  })

  it('preserves user-visible error categories', async () => {
    const client = createWebTagClient({ baseURL: 'http://x', token: 'bad' })
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(401, {
          error: { code: 401, error_code: 'unauthorized', message: 'bad' },
        }),
      )
    await expect(client.testConnection()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'unauthorized' },
    })

    fetchImpl = () => Promise.reject(new TypeError('Failed to fetch'))
    await expect(client.testConnection()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'network-unreachable' },
    })

    fetchImpl = () => Promise.resolve(jsonResponse(200, { reader_vnext: true }))
    await expect(client.testConnection()).resolves.toMatchObject({
      ok: false,
      error: { kind: 'other' },
    })
  })
})

describe('buildWebTagClientFromSettings', () => {
  it('returns null for blank backend URLs', () => {
    expect(
      buildWebTagClientFromSettings({ backendUrl: '', accessToken: 't' }),
    ).toBeNull()
    expect(
      buildWebTagClientFromSettings({ backendUrl: '   ', accessToken: 't' }),
    ).toBeNull()
  })

  it('trims settings before constructing the client', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, capabilities()))
    const client = buildWebTagClientFromSettings({
      backendUrl: ' http://x/ ',
      accessToken: ' token ',
    })

    await client?.getCapabilities()

    expect(lastUrl()).toBe('http://x/api/capabilities')
    expect((lastInit().headers as Record<string, string>).Authorization).toBe(
      'Bearer token',
    )
  })
})
