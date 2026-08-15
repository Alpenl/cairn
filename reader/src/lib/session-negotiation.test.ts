import { afterEach, describe, expect, it, vi } from 'vitest'
import { negotiateSession } from './session-negotiation'

const NAMESPACE_A = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
const NAMESPACE_B = 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'
const BASE_URL = 'http://localhost:8080'

function sessionCreatedResponse(namespace = NAMESPACE_A): Response {
  return new Response(JSON.stringify({
    expires_at: '2030-01-01T00:00:00Z',
    client_data_namespace: namespace,
    representation_contract: 'v3',
  }), {
    status: 201,
    headers: { 'X-WebTag-Data-Namespace': namespace },
  })
}

function identityResponse(namespace = NAMESPACE_A): Response {
  return new Response(JSON.stringify({
    client_data_namespace: namespace,
    representation_contract: 'v3',
  }), {
    status: 200,
    headers: { 'X-WebTag-Data-Namespace': namespace },
  })
}

function deferred(): { promise: Promise<void>; resolve: () => void } {
  let resolve!: () => void
  const promise = new Promise<void>((promiseResolve) => {
    resolve = promiseResolve
  })
  return { promise, resolve }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('negotiateSession', () => {
  it('commits a verified cookie before returning the session outcome', async () => {
    const commit = vi.fn(async () => true)
    vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) =>
      init?.method === 'POST' ? sessionCreatedResponse() : identityResponse(),
    ))

    await expect(negotiateSession({
      baseURL: `${BASE_URL}/`,
      installationToken: ' secret ',
      commit,
    })).resolves.toMatchObject({ kind: 'session' })
    expect(commit).toHaveBeenCalledWith({
      client_data_namespace: NAMESPACE_A,
      representation_contract: 'v3',
    })
  })

  it('cleans up a malformed successful POST before returning an error', async () => {
    const methods: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) => {
      methods.push(init?.method ?? 'GET')
      return init?.method === 'POST'
        ? new Response('{not-json', { status: 201 })
        : new Response(null, { status: 204 })
    }))

    await expect(negotiateSession({
      baseURL: BASE_URL,
      installationToken: 'secret',
    })).resolves.toMatchObject({ kind: 'error' })
    expect(methods).toEqual(['POST', 'DELETE'])
  })

  it('cleans up when persistence fails after cookie verification', async () => {
    const methods: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET'
      methods.push(method)
      if (method === 'POST') return sessionCreatedResponse()
      if (method === 'DELETE') return new Response(null, { status: 204 })
      return identityResponse()
    }))

    await expect(negotiateSession({
      baseURL: BASE_URL,
      installationToken: 'secret',
      commit: async () => {
        throw new Error('连接存储写入后校验失败')
      },
    })).resolves.toMatchObject({
      kind: 'error',
      error: { message: '连接存储写入后校验失败' },
    })
    expect(methods).toEqual(['POST', 'GET', 'DELETE'])
  })

  it('cleans up when the durable revision was superseded', async () => {
    const methods: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET'
      methods.push(method)
      if (method === 'POST') return sessionCreatedResponse()
      if (method === 'DELETE') return new Response(null, { status: 204 })
      return identityResponse()
    }))

    await expect(negotiateSession({
      baseURL: BASE_URL,
      installationToken: 'secret',
      commit: async () => false,
    })).resolves.toMatchObject({
      kind: 'error',
      error: { kind: 'identity-mismatch' },
    })
    expect(methods).toEqual(['POST', 'GET', 'DELETE'])
  })

  it('cancels after a successful POST by deleting without probing private identity', async () => {
    const controller = new AbortController()
    const methods: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET'
      methods.push(method)
      if (method === 'POST') {
        controller.abort()
        return sessionCreatedResponse()
      }
      return new Response(null, { status: 204 })
    }))

    await expect(negotiateSession({
      baseURL: BASE_URL,
      installationToken: 'secret',
      signal: controller.signal,
    })).resolves.toMatchObject({ kind: 'error' })
    expect(methods).toEqual(['POST', 'DELETE'])
  })

  it('serializes a successor POST until predecessor cleanup DELETE completes', async () => {
    const deleteStarted = deferred()
    const releaseDelete = deferred()
    const events: string[] = []
    let posts = 0
    let gets = 0
    vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET'
      if (method === 'POST') {
        posts += 1
        events.push(`post-${posts}`)
        return sessionCreatedResponse()
      }
      if (method === 'GET') {
        gets += 1
        events.push(`get-${gets}`)
        return identityResponse(gets === 1 ? NAMESPACE_B : NAMESPACE_A)
      }
      events.push('delete-1-start')
      deleteStarted.resolve()
      await releaseDelete.promise
      events.push('delete-1-end')
      return new Response(null, { status: 204 })
    }))

    const predecessor = negotiateSession({
      baseURL: BASE_URL,
      installationToken: 'first',
      expectedClientDataNamespace: NAMESPACE_A,
    })
    await deleteStarted.promise
    const successor = negotiateSession({
      baseURL: `${BASE_URL}/different-path`,
      installationToken: 'second',
      expectedClientDataNamespace: NAMESPACE_A,
      commit: async () => true,
    })
    await Promise.resolve()
    expect(posts).toBe(1)

    releaseDelete.resolve()
    await expect(predecessor).resolves.toMatchObject({ kind: 'error' })
    await expect(successor).resolves.toMatchObject({ kind: 'session' })
    expect(events).toEqual([
      'post-1',
      'get-1',
      'delete-1-start',
      'delete-1-end',
      'post-2',
      'get-2',
    ])
  })
})
