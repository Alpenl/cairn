import { afterEach, describe, expect, it, vi } from 'vitest'
import type { SessionIdentity } from './api/types'
import { negotiateLegacySessionUpgrade } from './legacy-session-upgrade'

const NAMESPACE_A = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
const NAMESPACE_B = 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'
const connection = {
  baseURL: 'http://localhost:8080',
  mode: 'installation-token' as const,
  installationToken: 'legacy-secret',
}
const bearerIdentity: SessionIdentity = {
  client_data_namespace: NAMESPACE_A,
  representation_contract: 'v3',
}

function identityResponse(namespace: string, status = 200): Response {
  return new Response(JSON.stringify({
    client_data_namespace: namespace,
    representation_contract: 'v3',
  }), {
    status,
    headers: { 'X-WebTag-Data-Namespace': namespace },
  })
}

function loginResponse(namespace: string): Response {
  return new Response(JSON.stringify({
    expires_at: '2030-01-01T00:00:00Z',
    client_data_namespace: namespace,
    representation_contract: 'v3',
  }), {
    status: 201,
    headers: { 'X-WebTag-Data-Namespace': namespace },
  })
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('negotiateLegacySessionUpgrade', () => {
  it('switches only after exchange and cookie identity both match the verified Bearer identity', async () => {
    const commitSession = vi.fn(async () => true)
    const fetchSpy = vi.fn(async (_url: string, init?: RequestInit) =>
      init?.method === 'POST' ? loginResponse(NAMESPACE_A) : identityResponse(NAMESPACE_A),
    )
    vi.stubGlobal('fetch', fetchSpy)

    await expect(negotiateLegacySessionUpgrade(connection, bearerIdentity, {
      commitSession,
    })).resolves.toEqual({
      kind: 'session',
    })
    expect(commitSession).toHaveBeenCalledOnce()
    expect(fetchSpy).toHaveBeenCalledTimes(2)
    expect(fetchSpy.mock.calls[0]?.[1]).toEqual(expect.objectContaining({
      method: 'POST',
      credentials: 'include',
    }))
    expect(fetchSpy.mock.calls[1]?.[1]).toEqual(expect.objectContaining({
      method: 'GET',
      credentials: 'include',
      headers: expect.objectContaining({ 'X-WebTag-Session': '1' }),
    }))
  })

  it.each([
    ['404', new Response('not found', { status: 404 }), 'bearer-compatibility'],
    ['401', new Response(JSON.stringify({ error: { message: 'invalid' } }), { status: 401 }), 'error'],
    ['429', new Response(JSON.stringify({ error: { message: 'limited' } }), { status: 429 }), 'error'],
    ['503', new Response(JSON.stringify({ error: { message: 'retry' } }), { status: 503 }), 'error'],
  ])('maps exchange %s to %s without cookie identity probing', async (_name, response, kind) => {
    const fetchSpy = vi.fn(async () => response.clone())
    vi.stubGlobal('fetch', fetchSpy)

    await expect(negotiateLegacySessionUpgrade(connection, bearerIdentity, {
      commitSession: async () => true,
    })).resolves.toMatchObject({ kind })
    expect(fetchSpy).toHaveBeenCalledTimes(1)
  })

  it('cleans up a malformed successful exchange', async () => {
    const fetchSpy = vi.fn(async (_url: string, init?: RequestInit) =>
      init?.method === 'POST'
        ? new Response('{not-json', { status: 201 })
        : new Response(null, { status: 204 }),
    )
    vi.stubGlobal('fetch', fetchSpy)

    await expect(negotiateLegacySessionUpgrade(connection, bearerIdentity, {
      commitSession: async () => true,
    })).resolves.toMatchObject({ kind: 'error' })
    expect(fetchSpy.mock.calls.map(([, init]) => init?.method)).toEqual(['POST', 'DELETE'])
  })

  it('stops on a network failure without treating it as Bearer compatibility', async () => {
    const fetchSpy = vi.fn(async () => {
      throw new TypeError('Failed to fetch')
    })
    vi.stubGlobal('fetch', fetchSpy)

    await expect(negotiateLegacySessionUpgrade(connection, bearerIdentity, {
      commitSession: async () => true,
    })).resolves.toMatchObject({
      kind: 'error',
      error: { kind: 'network-unreachable' },
    })
    expect(fetchSpy).toHaveBeenCalledTimes(1)
  })

  it('allows Bearer compatibility only when a successful exchange cookie receives 401', async () => {
    const fetchSpy = vi.fn(async (_url: string, init?: RequestInit) =>
      init?.method === 'POST'
        ? loginResponse(NAMESPACE_A)
        : new Response(JSON.stringify({ error: { message: 'cookie unavailable' } }), { status: 401 }),
    )
    vi.stubGlobal('fetch', fetchSpy)

    await expect(negotiateLegacySessionUpgrade(connection, bearerIdentity, {
      commitSession: async () => true,
    })).resolves.toEqual({
      kind: 'bearer-compatibility',
    })
    expect(fetchSpy.mock.calls.map(([, init]) => init?.method ?? 'GET')).toEqual([
      'POST',
      'GET',
      'DELETE',
    ])
  })

  it.each([
    ['exchange', NAMESPACE_B, NAMESPACE_A],
    ['cookie', NAMESPACE_A, NAMESPACE_B],
  ])('rejects and clears a cross-identity %s result', async (_surface, exchangeNamespace, cookieNamespace) => {
    const fetchSpy = vi.fn(async (_url: string, init?: RequestInit) => {
      if (init?.method === 'POST') return loginResponse(exchangeNamespace)
      if (init?.method === 'DELETE') return new Response(null, { status: 204 })
      return identityResponse(cookieNamespace)
    })
    vi.stubGlobal('fetch', fetchSpy)

    await expect(negotiateLegacySessionUpgrade(connection, bearerIdentity, {
      commitSession: async () => true,
    })).resolves.toMatchObject({
      kind: 'error',
      error: { kind: 'identity-mismatch' },
    })
    expect(fetchSpy).toHaveBeenCalledWith(
      'http://localhost:8080/api/session',
      expect.objectContaining({ method: 'DELETE', credentials: 'include' }),
    )
  })
})
