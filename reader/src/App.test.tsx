import 'fake-indexeddb/auto'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import App from './App'
import {
  CACHE_SCHEMA_VERSION,
  createPersistedRecord,
  idbClear,
  idbGetAll,
  idbPut,
  resetDatabaseHandle,
} from './lib/cache/idb'
import { stopCachePersistence } from './lib/cache/bootstrap'
import { DOMAIN_SUMMARIES_CACHE_KEY, TAGS_CACHE_KEY } from './lib/cache/keys'
import { resourceStore } from './lib/cache/store'
import { derivePhysicalNamespace, readerIdentity } from './lib/identity'
import { resetUserDataDatabaseHandle } from './lib/legacy-user-data'
import { ownedDatabaseName, readOwnedStorage } from './lib/storage-ownership'
import { saveConnection } from './lib/settings'
import { enumerateAnnotatedLinkIds } from './lib/user-data/annotation-store'

const EMPTY_PAGE = JSON.stringify({ items: [], total: 0, page: 1, limit: 30 })
const TEST_NAMESPACE = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'

const ENABLED_READER_CAPABILITIES = {
  library_kinds: true,
  site_library: true,
  site_management: true,
  site_advanced_management: true,
  archive_versions: [2],
  reader_vnext: true,
  reader: {
    annotations: true,
    notes: true,
    inbox: true,
    todos: true,
    engagement: true,
    home: true,
    feed: true,
    ai: false,
    related_tags: true,
    activity: true,
    history: true,
    trash: true,
  },
}

const EMPTY_HOME = {
  today: '2026-08-10',
  summary: '空的 Reader',
  counts: {},
  continue_reading: [],
  recent_thoughts: [],
  todos: [],
  stale: false,
}

function identityResponse(namespace = TEST_NAMESPACE): Response {
  return new Response(
    JSON.stringify({
      client_data_namespace: namespace,
      representation_contract: 'v3',
    }),
    { status: 200, headers: { 'X-WebTag-Data-Namespace': namespace } },
  )
}

function sessionCreatedResponse(namespace = TEST_NAMESPACE): Response {
  return new Response(
    JSON.stringify({
      expires_at: '2030-01-01T00:00:00Z',
      client_data_namespace: namespace,
      representation_contract: 'v3',
    }),
    { status: 201, headers: { 'X-WebTag-Data-Namespace': namespace } },
  )
}

function mockFetchEmpty() {
  // 所有 GET（links/tags/tree）都返回安全空体；tags/tree 形状不符时 hook 退化为空，
  // 不影响主界面渲染。
  const fn = vi.fn(async (url: string) => {
      if (url.includes('/api/session')) return identityResponse()
      let body = EMPTY_PAGE
      if (url.includes('/api/tags')) body = '[]'
      else if (url.includes('/api/tree')) body = JSON.stringify({ nodes: [], total: 0 })
      return new Response(body, {
        status: 200,
        headers: {
          'Content-Type': 'application/json',
          'X-WebTag-Data-Namespace': TEST_NAMESPACE,
        },
      })
    })
  vi.stubGlobal('fetch', fn)
  return fn
}

async function waitForEmptyMainView(): Promise<void> {
  const lease = readerIdentity.activeLease
  if (!lease) throw new Error('expected an active Reader identity')
  await act(async () => {
    await enumerateAnnotatedLinkIds(lease)
  })
  await waitFor(() => {
    expect(screen.getByText('这个筛选下还没有链接')).toBeInTheDocument()
    for (const key of [TAGS_CACHE_KEY, DOMAIN_SUMMARIES_CACHE_KEY]) {
      const entry = resourceStore.peek(key)
      expect(entry.data !== undefined || entry.error !== null).toBe(true)
      expect(entry.revalidating).toBe(false)
    }
  })
}

afterEach(async () => {
  stopCachePersistence()
  await idbClear()
  resetDatabaseHandle()
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => resolve()
    request.onblocked = () => resolve()
  })
  localStorage.clear()
  vi.unstubAllGlobals()
})

/** 同源探测的应答桩：health 决定是不是 Cairn 后端，identity 决定是否免鉴权。 */
function mockProbe(opts: { health: boolean; open: boolean }) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string) => {
      if (url.includes('/health')) {
        return opts.health
          ? new Response(JSON.stringify({ status: 'ok', version: '1.2.3' }), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          : new Response('not found', { status: 404 })
      }
      if (url.includes('/api/session')) {
        return opts.open
          ? identityResponse()
          : new Response(JSON.stringify({ error: { message: 'unauthorized' } }), {
              status: 401,
              headers: { 'Content-Type': 'application/json' },
            })
      }
      if (url.includes('/api/links')) {
        return opts.open
          ? new Response(EMPTY_PAGE, {
              status: 200,
              headers: {
                'Content-Type': 'application/json',
                'X-WebTag-Data-Namespace': TEST_NAMESPACE,
              },
            })
          : new Response(JSON.stringify({ error: { message: 'unauthorized' } }), { status: 401 })
      }
      if (url.includes('/api/tags')) {
        return new Response('[]', {
          status: 200,
          headers: {
            'Content-Type': 'application/json',
            'X-WebTag-Data-Namespace': TEST_NAMESPACE,
          },
        })
      }
      return new Response(JSON.stringify({ nodes: [], total: 0 }), {
        status: 200,
        headers: {
          'Content-Type': 'application/json',
          'X-WebTag-Data-Namespace': TEST_NAMESPACE,
        },
      })
    }),
  )
}

describe('App smoke', () => {
  it('fails closed visibly when a historical token cannot be scrubbed', async () => {
    const fetchSpy = vi.fn()
    vi.stubGlobal('fetch', fetchSpy)
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: 'http://localhost:8080',
        mode: 'installation-token',
        installationToken: 'must-stay-unexposed',
      }),
    )
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('storage denied', 'SecurityError')
    })

    const { container } = render(<App />)

    expect(await screen.findByRole('alert')).toHaveTextContent(
      '无法安全读取或保存连接：无法写入连接存储',
    )
    expect(container.querySelector('.sidebar')).not.toBeInTheDocument()
    expect(fetchSpy).not.toHaveBeenCalled()
    expect(localStorage.getItem('webtag:reader:conn:v2')).toContain('must-stay-unexposed')
  })

  it('drops private A state and re-handshakes before hydrating B after a cookie replacement', async () => {
    const baseURL = 'http://localhost:8080'
    const namespaceA = TEST_NAMESPACE
    const namespaceB = 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'
    const physicalA = await derivePhysicalNamespace(baseURL, namespaceA)
    const physicalB = await derivePhysicalNamespace(baseURL, namespaceB)
    const probeKey = 'GET /api/links?identity-probe'
    await idbClear()
    await idbPut(createPersistedRecord(physicalA, probeKey, {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'A-on-disk' },
      updatedAt: 1,
      size: 20,
    }))
    await idbPut(createPersistedRecord(physicalB, probeKey, {
      schema: CACHE_SCHEMA_VERSION,
      data: { owner: 'B-on-disk' },
      updatedAt: 2,
      size: 20,
    }))

    let identityCalls = 0
    let resolveIdentityB!: (response: Response) => void
    const identityB = new Promise<Response>((resolve) => {
      resolveIdentityB = resolve
    })
    let resolveMismatchedLinks!: (response: Response) => void
    const mismatchedLinks = new Promise<Response>((resolve) => {
      resolveMismatchedLinks = resolve
    })
    let firstLinksRequest = true
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string): Promise<Response> => {
        if (url.endsWith('/api/session')) {
          identityCalls += 1
          return identityCalls === 1 ? Promise.resolve(identityResponse(namespaceA)) : identityB
        }
        if (url.includes('/api/links') && firstLinksRequest) {
          firstLinksRequest = false
          return mismatchedLinks
        }
        const namespace = identityCalls >= 2 ? namespaceB : namespaceA
        let body = EMPTY_PAGE
        if (url.includes('/api/tags')) body = '[]'
        else if (url.includes('/api/tree')) body = JSON.stringify({ nodes: [], total: 0 })
        return Promise.resolve(
          new Response(body, {
            status: 200,
            headers: {
              'Content-Type': 'application/json',
              'X-WebTag-Data-Namespace': namespace,
            },
          }),
        )
      }),
    )
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({ baseURL, mode: 'session', installationToken: '' }),
    )

    const { container } = render(<App />)
    await waitFor(() => expect(container.querySelector('.sidebar')).toBeInTheDocument())
    expect(resourceStore.activePhysicalNamespace).toBe(physicalA)
    expect(resourceStore.peek<{ owner: string }>(probeKey).data?.owner).toBe('A-on-disk')
    const leaseA = readerIdentity.activeLease
    expect(leaseA).not.toBeNull()

    await act(async () => {
      resolveMismatchedLinks(
        new Response(EMPTY_PAGE, {
          status: 200,
          headers: {
            'Content-Type': 'application/json',
            'X-WebTag-Data-Namespace': namespaceB,
          },
        }),
      )
    })
    await waitFor(() => expect(identityCalls).toBe(2))
    expect(container.querySelector('.sidebar')).not.toBeInTheDocument()
    expect(resourceStore.activePhysicalNamespace).toBeNull()
    expect(resourceStore.has(probeKey)).toBe(false)

    await act(async () => {
      resolveIdentityB(identityResponse(namespaceB))
    })
    await waitFor(() => expect(container.querySelector('.sidebar')).toBeInTheDocument())
    await waitForEmptyMainView()

    expect(resourceStore.activePhysicalNamespace).toBe(physicalB)
    expect(resourceStore.peek<{ owner: string }>(probeKey).data?.owner).toBe('B-on-disk')
    expect(leaseA?.capture('after cookie replacement').signal.aborted).toBe(true)
    expect(readerIdentity.activeLease?.context.localEpoch).toBeGreaterThan(
      leaseA?.context.localEpoch ?? 0,
    )
    expect(
      (await idbGetAll())
        .filter((record) => record.logicalKey === probeKey)
        .map((record) => record.namespace)
        .sort(),
    ).toEqual([physicalA, physicalB].sort())
  })

  it('aborts a pending identity handshake when the connection changes', async () => {
    let firstSignal: AbortSignal | undefined
    let resolveFirst!: (response: Response) => void
    const fetchSpy = vi.fn((url: string, init?: RequestInit): Promise<Response> => {
      if (url === 'http://localhost:8080/api/session') {
        firstSignal = init?.signal as AbortSignal
        return new Promise<Response>((resolve, reject) => {
          resolveFirst = resolve
          firstSignal?.addEventListener(
            'abort',
            () => reject(new DOMException('aborted', 'AbortError')),
            { once: true },
          )
        })
      }
      if (url === 'http://localhost:9090/api/session') {
        const secondSignal = init?.signal as AbortSignal
        return new Promise<Response>((_resolve, reject) => {
          secondSignal.addEventListener(
            'abort',
            () => reject(new DOMException('aborted', 'AbortError')),
            { once: true },
          )
        })
      }
      return Promise.resolve(new Response(null, { status: 404 }))
    })
    vi.stubGlobal('fetch', fetchSpy)
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: 'http://localhost:8080',
        mode: 'session',
        installationToken: '',
      }),
    )

    const { unmount } = render(<App />)
    await waitFor(() => expect(firstSignal).toBeDefined())

    await act(async () => {
      await saveConnection({
        baseURL: 'http://localhost:9090',
        mode: 'session',
        installationToken: '',
      })
    })
    await waitFor(() =>
      expect(fetchSpy).toHaveBeenCalledWith(
        'http://localhost:9090/api/session',
        expect.objectContaining({ cache: 'no-store' }),
      ),
    )
    const abortedOnSwitch = firstSignal?.aborted
    resolveFirst(identityResponse())
    unmount()

    expect(abortedOnSwitch).toBe(true)
  })

  it('returns to connection setup when a saved session is unauthorized', async () => {
    const fetchSpy = vi.fn(async (url: string) => {
      if (url === 'http://localhost:8080/api/session') {
        return new Response(
          JSON.stringify({ error: { message: 'unauthorized' } }),
          { status: 401, headers: { 'Content-Type': 'application/json' } },
        )
      }
      throw new Error(`private request must not start before identity: ${url}`)
    })
    vi.stubGlobal('fetch', fetchSpy)
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: 'http://localhost:8080',
        mode: 'session',
        installationToken: '',
      }),
    )

    const { container } = render(<App />)

    expect(await screen.findByLabelText('后端地址')).toHaveValue('http://localhost:8080')
    expect(screen.getByLabelText('安装令牌')).toHaveValue('')
    expect(screen.queryByText(/无法确认当前身份/)).not.toBeInTheDocument()
    expect(container.querySelector('.sidebar')).not.toBeInTheDocument()
    expect(fetchSpy).toHaveBeenCalledOnce()
    expect(fetchSpy).toHaveBeenCalledWith(
      'http://localhost:8080/api/session',
      expect.objectContaining({ cache: 'no-store' }),
    )
  })

  it('auto-upgrades a historical token to same-identity cookie mode before mounting private UI', async () => {
    const sessionAuthModes: string[] = []
    const fetchSpy = vi.fn(async (url: string, init?: RequestInit) => {
      const headers = (init?.headers ?? {}) as Record<string, string>
      if (url.endsWith('/api/session')) {
        if (init?.method === 'POST') {
          sessionAuthModes.push('exchange')
          expect(JSON.parse(String(init.body))).toEqual({ token: 'legacy-secret' })
          return sessionCreatedResponse()
        }
        if (headers.Authorization) {
          sessionAuthModes.push('bearer')
          return identityResponse()
        }
        if (headers['X-WebTag-Session']) {
          sessionAuthModes.push('cookie')
          return identityResponse()
        }
        return new Response(JSON.stringify({ error: { message: 'unauthorized' } }), { status: 401 })
      }
      expect(headers.Authorization).toBeUndefined()
      expect(headers['X-WebTag-Session']).toBe('1')
      let body = EMPTY_PAGE
      if (url.includes('/api/tags')) body = '[]'
      else if (url.includes('/api/tree')) body = JSON.stringify({ nodes: [], total: 0 })
      return new Response(body, {
        status: 200,
        headers: {
          'Content-Type': 'application/json',
          'X-WebTag-Data-Namespace': TEST_NAMESPACE,
        },
      })
    })
    vi.stubGlobal('fetch', fetchSpy)
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: 'http://localhost:8080',
        mode: 'installation-token',
        installationToken: 'legacy-secret',
      }),
    )

    const { container } = render(<App />)
    await waitFor(() => expect(container.querySelector('.sidebar')).toBeInTheDocument())
    await waitForEmptyMainView()

    expect(sessionAuthModes.slice(0, 3)).toEqual(['bearer', 'exchange', 'cookie'])
    expect(sessionAuthModes.filter((mode) => mode === 'exchange')).toHaveLength(1)
    expect(sessionAuthModes.filter((mode) => mode === 'cookie').length).toBeGreaterThanOrEqual(2)
    expect(JSON.parse(localStorage.getItem('webtag:reader:conn:v2') ?? '{}')).toMatchObject({
      baseURL: 'http://localhost:8080',
      mode: 'session',
      installationToken: '',
      revision: expect.any(String),
    })
    expect(localStorage.getItem('webtag:reader:conn:v2') ?? '').not.toContain('legacy-secret')
  })

  it('retries session exchange after a transient legacy-upgrade failure without mounting Bearer UI', async () => {
    let exchangeCalls = 0
    const fetchSpy = vi.fn(async (url: string, init?: RequestInit) => {
      const headers = (init?.headers ?? {}) as Record<string, string>
      if (url.endsWith('/api/session') && init?.method === 'POST') {
        exchangeCalls += 1
        return new Response(JSON.stringify({ error: { message: 'temporarily unavailable' } }), {
          status: 503,
          headers: { 'Content-Type': 'application/json' },
        })
      }
      if (url.endsWith('/api/session') && headers.Authorization) return identityResponse()
      throw new Error(`private request must not start during failed upgrade: ${url}`)
    })
    vi.stubGlobal('fetch', fetchSpy)
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: 'http://localhost:8080',
        mode: 'installation-token',
        installationToken: 'legacy-secret',
      }),
    )

    const { container } = render(<App />)
    expect(await screen.findByRole('alert')).toHaveTextContent('temporarily unavailable')
    expect(container.querySelector('.sidebar')).not.toBeInTheDocument()
    expect(exchangeCalls).toBe(1)
    expect(localStorage.getItem('webtag:reader:conn:v2') ?? '').not.toContain('legacy-secret')

    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    await waitFor(() => expect(exchangeCalls).toBe(2))
    expect(container.querySelector('.sidebar')).not.toBeInTheDocument()
  })

  it('continues the verified Bearer path only when legacy session exchange is explicitly unsupported', async () => {
    let exchangeCalls = 0
    const fetchSpy = vi.fn(async (url: string, init?: RequestInit) => {
      const headers = (init?.headers ?? {}) as Record<string, string>
      if (url.endsWith('/api/session') && init?.method === 'POST') {
        exchangeCalls += 1
        return new Response('not found', { status: 404 })
      }
      if (url.endsWith('/api/session')) {
        expect(headers.Authorization).toBe('Bearer legacy-secret')
        return identityResponse()
      }
      expect(headers.Authorization).toBe('Bearer legacy-secret')
      let body = EMPTY_PAGE
      if (url.includes('/api/tags')) body = '[]'
      else if (url.includes('/api/tree')) body = JSON.stringify({ nodes: [], total: 0 })
      return new Response(body, {
        status: 200,
        headers: {
          'Content-Type': 'application/json',
          'X-WebTag-Data-Namespace': TEST_NAMESPACE,
        },
      })
    })
    vi.stubGlobal('fetch', fetchSpy)
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: 'http://localhost:8080',
        mode: 'installation-token',
        installationToken: 'legacy-secret',
      }),
    )

    const { container } = render(<App />)
    await waitFor(() => expect(container.querySelector('.sidebar')).toBeInTheDocument())
    await waitForEmptyMainView()

    expect(exchangeCalls).toBe(1)
    expect(JSON.parse(localStorage.getItem('webtag:reader:conn:v2') ?? '{}')).toMatchObject({
      baseURL: 'http://localhost:8080',
      mode: 'installation-token',
      installationToken: '',
      revision: expect.any(String),
    })
  })

  it('uses the authoritative identity client for capability bootstrap', async () => {
    const previousURL = window.location.href
    const requests: string[] = []
    const jsonWithIdentity = (body: unknown) => new Response(JSON.stringify(body), {
      status: 200,
      headers: {
        'Content-Type': 'application/json',
        'X-WebTag-Data-Namespace': TEST_NAMESPACE,
      },
    })
    const fetchSpy = vi.fn(async (url: string) => {
      requests.push(url)
      if (url.endsWith('/api/session')) return identityResponse()
      if (url.endsWith('/api/capabilities')) return jsonWithIdentity(ENABLED_READER_CAPABILITIES)
      if (url.endsWith('/api/home')) return jsonWithIdentity(EMPTY_HOME)
      if (url.includes('/api/links')) return jsonWithIdentity(JSON.parse(EMPTY_PAGE))
      if (url.includes('/api/tags')) return jsonWithIdentity([])
      if (url.includes('/api/tree')) return jsonWithIdentity({ nodes: [], total: 0 })
      return jsonWithIdentity({})
    })
    vi.stubGlobal('fetch', fetchSpy)
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: 'http://localhost:8080',
        mode: 'session',
        installationToken: '',
      }),
    )
    window.history.replaceState({}, '', '/?surface=home')

    try {
      render(<App />)
      await waitFor(() => {
        expect(screen.getByRole('heading', { level: 1, name: '今天' })).toBeInTheDocument()
      })
      const identityIndex = requests.findIndex((url) => url.endsWith('/api/session'))
      const capabilitiesIndex = requests.findIndex((url) => url.endsWith('/api/capabilities'))
      expect(identityIndex).toBeGreaterThanOrEqual(0)
      expect(capabilitiesIndex).toBeGreaterThan(identityIndex)
    } finally {
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('refreshes runtime capabilities on lifecycle events with offline and in-flight dedupe', async () => {
    const previousURL = window.location.href
    const originalOnline = Object.getOwnPropertyDescriptor(window.navigator, 'onLine')
    let capabilityCalls = 0
    let resolveRefresh!: (response: Response) => void
    const pendingRefresh = new Promise<Response>((resolve) => {
      resolveRefresh = resolve
    })
    const jsonWithIdentity = (body: unknown, status = 200) => new Response(JSON.stringify(body), {
      status,
      headers: {
        'Content-Type': 'application/json',
        'X-WebTag-Data-Namespace': TEST_NAMESPACE,
      },
    })
    const fetchSpy = vi.fn(async (url: string) => {
      if (url.endsWith('/api/session')) return identityResponse()
      if (url.endsWith('/api/capabilities')) {
        capabilityCalls += 1
        if (capabilityCalls === 2) return pendingRefresh
        return jsonWithIdentity(ENABLED_READER_CAPABILITIES)
      }
      if (url.includes('/api/notes')) return jsonWithIdentity({ items: [], count: 0 })
      if (url.endsWith('/api/home')) return jsonWithIdentity(EMPTY_HOME)
      if (url.includes('/api/links')) return jsonWithIdentity(JSON.parse(EMPTY_PAGE))
      if (url.includes('/api/tags')) return jsonWithIdentity([])
      if (url.includes('/api/tree')) return jsonWithIdentity({ nodes: [], total: 0 })
      return jsonWithIdentity({})
    })
    vi.stubGlobal('fetch', fetchSpy)
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: 'http://localhost:8080',
        mode: 'session',
        installationToken: '',
      }),
    )
    window.history.replaceState({}, '', '/?view=notes')

    try {
      render(<App />)
      expect(await screen.findByText('还没有笔记')).toBeInTheDocument()
      expect(capabilityCalls).toBe(1)

      Object.defineProperty(window.navigator, 'onLine', { configurable: true, value: false })
      window.dispatchEvent(new Event('focus'))
      window.dispatchEvent(new Event('online'))
      await act(async () => { await Promise.resolve() })
      expect(capabilityCalls).toBe(1)

      Object.defineProperty(window.navigator, 'onLine', { configurable: true, value: true })
      window.dispatchEvent(new Event('focus'))
      await waitFor(() => expect(capabilityCalls).toBe(2))
      window.dispatchEvent(new Event('focus'))
      document.dispatchEvent(new Event('visibilitychange'))
      window.dispatchEvent(new Event('online'))
      await act(async () => { await Promise.resolve() })
      expect(capabilityCalls).toBe(2)

      await act(async () => {
        resolveRefresh(jsonWithIdentity({ error: { message: 'temporarily unavailable' } }, 503))
        await pendingRefresh
      })
      await waitFor(() => expect(window.location.search).toBe('?view=reading'))
      expect(screen.queryByText('还没有笔记')).not.toBeInTheDocument()

      window.dispatchEvent(new Event('online'))
      await waitFor(() => expect(capabilityCalls).toBe(3))
      await waitFor(() => expect(screen.getByRole('tab', { name: '笔记' })).toBeInTheDocument())
      expect(window.location.search).toBe('?view=reading')
    } finally {
      if (originalOnline) Object.defineProperty(window.navigator, 'onLine', originalOnline)
      else delete (window.navigator as unknown as Record<string, unknown>).onLine
      window.history.replaceState({}, '', previousURL)
    }
  })

  it('keeps legacy user data quarantined until the user selects a target identity', async () => {
    mockFetchEmpty()
    localStorage.setItem('webtag:pins:v1', '{"tags":["legacy-A"],"domains":[]}')
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: 'http://localhost:8080',
        mode: 'session',
        installationToken: '',
      }),
    )

    const { container } = render(<App />)

    await waitFor(() =>
      expect(screen.getByRole('dialog', { name: '未归属的本地数据' })).toBeInTheDocument(),
    )
    expect(container.querySelector('.sidebar')).not.toBeInTheDocument()
    expect(readOwnedStorage('pins')).toBeNull()

    fireEvent.click(screen.getByRole('button', { name: '保持隔离' }))
    await waitFor(() => expect(container.querySelector('.sidebar')).toBeInTheDocument())
    await waitForEmptyMainView()
    expect(readOwnedStorage('pins')).toBeNull()
  })

  it('imports legacy user data only after selecting the current identity', async () => {
    mockFetchEmpty()
    localStorage.setItem('webtag:pins:v1', '{"tags":["legacy-A"],"domains":[]}')
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: 'http://localhost:8080',
        mode: 'session',
        installationToken: '',
      }),
    )

    const { container } = render(<App />)
    await waitFor(() =>
      expect(screen.getByRole('dialog', { name: '未归属的本地数据' })).toBeInTheDocument(),
    )
    expect(readOwnedStorage('pins')).toBeNull()

    fireEvent.click(screen.getByRole('button', { name: '导入当前身份' }))
    await waitFor(() => expect(container.querySelector('.sidebar')).toBeInTheDocument())
    await waitForEmptyMainView()
    expect(JSON.parse(readOwnedStorage('pins') ?? '{}')).toEqual({
      tags: ['legacy-A'],
      domains: [],
    })
  })

  it('waits for identity-scoped cache hydration before mounting private UI', async () => {
    const fetchSpy = mockFetchEmpty()
    const originalOpen = indexedDB.open.bind(indexedDB)
    const openSpy = vi.spyOn(indexedDB, 'open').mockImplementation((name, version) => {
      if (name !== ownedDatabaseName('cacheDatabase')) {
        return version === undefined
          ? originalOpen(name)
          : originalOpen(name, version)
      }
      return {
        onsuccess: null,
        onerror: null,
        onupgradeneeded: null,
        onblocked: null,
      } as unknown as IDBOpenDBRequest
    })
    resetDatabaseHandle()
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: 'http://localhost:8080',
        mode: 'session',
        installationToken: '',
      }),
    )

    const { container } = render(<App />)
    await waitFor(() =>
      expect(fetchSpy).toHaveBeenCalledWith(
        'http://localhost:8080/api/session',
        expect.objectContaining({ cache: 'no-store' }),
      ),
    )
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(container.querySelector('.sidebar')).not.toBeInTheDocument()
    await waitFor(() => expect(container.querySelector('.sidebar')).toBeInTheDocument())
    await waitForEmptyMainView()

    openSpy.mockRestore()
    resetDatabaseHandle()
  })

  it('does not mount the private Reader subtree before identity is authoritative', async () => {
    const namespace = TEST_NAMESPACE
    let resolveIdentity!: (response: Response) => void
    const identityResponse = new Promise<Response>((resolve) => {
      resolveIdentity = resolve
    })
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        if (url.includes('/api/session')) return identityResponse
        let body = EMPTY_PAGE
        if (url.includes('/api/tags')) body = '[]'
        else if (url.includes('/api/tree')) body = JSON.stringify({ nodes: [], total: 0 })
        return new Response(body, {
          status: 200,
          headers: {
            'Content-Type': 'application/json',
            'X-WebTag-Data-Namespace': namespace,
          },
        })
      }),
    )
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: 'http://localhost:8080',
        mode: 'session',
        installationToken: '',
      }),
    )

    const { container } = render(<App />)
    expect(container.querySelector('.sidebar')).not.toBeInTheDocument()

    resolveIdentity(
      new Response(
        JSON.stringify({
          client_data_namespace: namespace,
          representation_contract: 'v3',
        }),
        { status: 200, headers: { 'X-WebTag-Data-Namespace': namespace } },
      ),
    )
    await waitFor(() => expect(container.querySelector('.sidebar')).toBeInTheDocument())
    await waitForEmptyMainView()
  })

  it('首跑无配置且同源不是 Cairn 后端时渲染连接引导', async () => {
    mockProbe({ health: false, open: false })
    render(<App />)
    await waitFor(() => expect(screen.getByText('连接到 Cairn 后端')).toBeInTheDocument())
  })

  // 后端 serve Reader 且开放访问时，用户不该被要求手抄他刚刚打开的那个地址。
  it('探到免鉴权的同源后端时跳过引导直接进主界面', async () => {
    mockProbe({ health: true, open: true })
    const { container } = render(<App />)
    await waitFor(() => expect(container.querySelector('.sidebar')).toBeInTheDocument())
    await waitForEmptyMainView()
    expect(screen.queryByText('连接到 Cairn 后端')).not.toBeInTheDocument()
    // 探测结果必须落盘，否则下次刷新又要重探一遍。
    expect(
      JSON.parse(localStorage.getItem('webtag:reader:conn:v2') || '{}'),
    ).toMatchObject({
      baseURL: window.location.origin,
      mode: 'installation-token',
      installationToken: '',
    })
  })

  // 需要鉴权时引导页照出，但地址已经填好，只需填写安装令牌。
  it('探到需鉴权的同源后端时预填地址并停在引导页', async () => {
    mockProbe({ health: true, open: false })
    render(<App />)
    await waitFor(() => expect(screen.getByText('连接到 Cairn 后端')).toBeInTheDocument())
    const input = screen.getByPlaceholderText('http://localhost:8080') as HTMLInputElement
    expect(input.value).toBe(window.location.origin)
    // 未通过鉴权探测时不得擅自落配置。
    expect(localStorage.getItem('webtag:reader:conn:v2')).toBeNull()
  })

  it('已配置渲染三栏主界面（侧栏 + 列表）', async () => {
    mockFetchEmpty()
    localStorage.setItem(
      'webtag:reader:conn:v2',
      JSON.stringify({
        baseURL: 'http://localhost:8080',
        mode: 'installation-token',
        installationToken: '',
      }),
    )
    const { container } = render(<App />)
    // 三栏外壳渲染（侧栏 + 列表面板存在）
    await waitFor(() => expect(container.querySelector('.sidebar')).toBeInTheDocument())
    expect(container.querySelector('.list-pane')).toBeInTheDocument()
    // 侧栏「全部链接」行存在（可能与列表标题同名，用 getAllByText）
    expect(screen.getAllByText('全部链接').length).toBeGreaterThan(0)
    // 列表空态最终出现
    await waitForEmptyMainView()
  })
})
