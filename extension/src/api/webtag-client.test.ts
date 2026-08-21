/**
 * webtag-client.test.ts — WebTagClient 单元测试。
 *
 * 通过 vi.stubGlobal('fetch', ...) 模拟 native fetch（客户端已弃用 axios），
 * 验证三类行为：
 *
 * 一、请求构造与错误归一化
 *   - 成功响应归一化为 { ok: true, data }
 *   - 401/403 归一化为 unauthorized
 *   - fetch 抛 TypeError（网络层失败）归一化为 network-unreachable
 *   - AbortController 触发的 AbortError 归一化为 timeout
 *   - 真实超时路径（fake timers 驱动 setTimeout(DEFAULT_TIMEOUT) + AbortController）
 *   - 响应体挂起路径（headers 已回但 res.text() 永不 resolve）也归一化为 timeout，
 *     验证超时计时器覆盖响应体消费阶段而非在 fetch resolve 后立即清除
 *   - 其它非 2xx HTTP 错误归一化为 other，并透传后端 error_code
 *   - baseURL 注入与尾斜杠规整、Bearer 头注入
 *   - 各方法（getTree/getLinks/getTags/ingest/testConnection）的 URL 与参数
 *   - buildWebTagClientFromSettings：trim 地址/Token、空地址判未配置返回 null
 *
 * 二、generated wire 响应运行时校验（健壮性，遵循失败关闭原则）
 *   - getTree：底层 links 分页或任一 Link 残缺 → other 错误
 *   - getLinks：顶层 items 非数组、或 total/page/limit 缺失/非 number → other 错误；
 *     任一 Link 缺字段、tags 非 string[] 或 enum 非法 → 整体失败
 *   - getTags：响应非数组或任一 Tag 残缺 → 整体失败
 *   - ingest/refreshLink：持久身份与 status enum 严格校验
 *   - 200 携带后端错误体 → other 错误（不冒充合法数据）
 *   - 200 返回非 JSON（反代登录页）→ other 错误
 *   - 任何畸形响应都不抛异常
 *
 * 不依赖真实后端，所有请求由 stub 的 fetch 拦截。
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  buildWebTagClientFromSettings,
  createWebTagClient,
} from './webtag-client'
import type { IngestRequest, Link } from './types'

// ── fetch mock 基础设施 ──

/** 单次 fetch 调用的行为：要么 resolve（Response），要么 reject（错误）。 */
type FetchImpl = (input: string, init?: RequestInit) => Promise<Response>

let fetchImpl: FetchImpl
const fetchSpy = vi.fn((input: unknown, init?: unknown) =>
  fetchImpl(input as string, init as RequestInit | undefined),
)

/**
 * 构造一个最小可用的 Response 替身。
 * vitest（jsdom）下 Response 可用，这里直接用真实 Response 保证 .ok / .status /
 * .text() / .headers 行为正确。bodyText 原样作为响应体文本，headers 可选
 * （用于覆盖 429 的 Retry-After 解析路径）。
 */
function makeResponse(opts: {
  status?: number
  bodyText: string
  headers?: Record<string, string>
}): Response {
  const status = opts.status ?? 200
  return new Response(opts.bodyText, { status, headers: opts.headers })
}

/** 便捷：JSON 响应。可选 headers 透传给底层 Response。 */
function jsonResponse(
  status: number,
  value: unknown,
  headers?: Record<string, string>,
): Response {
  return makeResponse({ status, bodyText: JSON.stringify(value), headers })
}

beforeEach(() => {
  fetchSpy.mockClear()
  // 默认：空 body 的 200。
  fetchImpl = () => Promise.resolve(makeResponse({ bodyText: '' }))
  vi.stubGlobal('fetch', fetchSpy)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

/** 取 fetchSpy 最后一次调用的 URL。 */
function lastUrl(): string {
  const call = fetchSpy.mock.calls[fetchSpy.mock.calls.length - 1]
  return call[0] as string
}

/** 取 fetchSpy 最后一次调用的 init。 */
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

// ── 构造与注入 ──

describe('WebTagClient 构造与注入', () => {
  it('规整 baseURL 尾斜杠并注入 Bearer 头', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, []))
    const client = createWebTagClient({
      baseURL: 'http://localhost:8080///',
      token: 'secret',
    })
    await client.getTags()
    expect(lastUrl()).toBe('http://localhost:8080/api/tags')
    expect((lastInit().headers as Record<string, string>).Authorization).toBe(
      'Bearer secret',
    )
  })

  it('token 为空时不注入 Authorization 头', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, []))
    const client = createWebTagClient({
      baseURL: 'http://localhost:8080',
      token: '   ',
    })
    await client.getTags()
    expect(
      (lastInit().headers as Record<string, string>).Authorization,
    ).toBeUndefined()
  })
})

// ── 请求构造：URL 与参数 ──

describe('请求构造', () => {
  it('getTags 请求 GET /api/tags', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, []))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    await client.getTags()
    expect(lastUrl()).toBe('http://x/api/tags')
    expect(lastInit().method).toBe('GET')
  })

  it('getTree 透传 domain 查询参数', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, { items: [], total: 0, page: 0, limit: 100 }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    await client.getTree({ domain: 'arxiv.org' })
    expect(lastUrl()).toBe(
      'http://x/api/links?domain=arxiv.org&status=done&limit=100&after=',
    )
  })

  it('getLinks 透传分页与筛选参数', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, { items: [], total: 0, page: 1, limit: 20 }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    await client.getLinks({ tags: 'go,web', page: 2, limit: 50 })
    const url = new URL(lastUrl())
    expect(url.pathname).toBe('/api/links')
    expect(url.searchParams.get('tags')).toBe('go,web')
    expect(url.searchParams.get('page')).toBe('2')
    expect(url.searchParams.get('limit')).toBe('50')
  })

  it('getLinks 透传 status 状态集合参数', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, { items: [], total: 0, page: 1, limit: 20 }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    await client.getLinks({ status: 'pending,processing,failed', limit: 50 })
    const url = new URL(lastUrl())
    expect(url.pathname).toBe('/api/links')
    expect(url.searchParams.get('status')).toBe('pending,processing,failed')
    expect(url.searchParams.get('limit')).toBe('50')
  })

  it('getLinks 不传 status 时 URL 不带该参数（后端返回全部已保存状态）', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, { items: [], total: 0, page: 1, limit: 20 }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    await client.getLinks({ limit: 50 })
    const url = new URL(lastUrl())
    expect(url.searchParams.has('status')).toBe(false)
  })

  it('refreshLink 以 POST 请求 /api/links/{id}/refresh 并对 id 编码', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          link_id: 'l1',
          status: 'pending',
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.refreshLink('a b')
    expect(res.ok).toBe(true)
    expect(lastUrl()).toBe('http://x/api/links/a%20b/refresh')
    expect(lastInit().method).toBe('POST')
  })

  it('refreshLink 使用调用方提供的稳定 Idempotency-Key', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          link_id: 'l1',
          status: 'pending',
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await client.refreshLink('l1', { idempotencyKey: 'refresh-key-1' })

    expect(
      (lastInit().headers as Record<string, string>)['Idempotency-Key'],
    ).toBe('refresh-key-1')
  })

  it('URL-only ingest 以 POST + JSON body 显式提交 auto 意图', async () => {
    fetchImpl = () =>
      Promise.resolve(jsonResponse(200, { link_id: 'l1', status: 'pending' }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const body: IngestRequest = {
      sources: [{ kind: 'url', url: 'http://p' }],
      destination: 'library',
      requested_library_kind: 'auto',
    }
    const res = await client.ingest(body)
    expect(res.ok).toBe(true)
    expect(lastUrl()).toBe('http://x/api/ingest')
    expect(lastInit().method).toBe('POST')
    expect((lastInit().headers as Record<string, string>)['Content-Type']).toBe(
      'application/json',
    )
    expect(lastInit().body).toBe(JSON.stringify(body))
  })

  it('ingest 使用调用方提供的 Idempotency-Key', async () => {
    fetchImpl = () =>
      Promise.resolve(jsonResponse(200, { link_id: 'l1', status: 'done' }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await client.ingest(
      { sources: [{ kind: 'browser_capture', url: 'https://example.com' }] },
      { idempotencyKey: 'capture-key-1' },
    )

    expect(
      (lastInit().headers as Record<string, string>)['Idempotency-Key'],
    ).toBe('capture-key-1')
  })

  it('ingest 对网络失败只重试一次并复用同一个 Idempotency-Key', async () => {
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

  it('ingest 对无 HTTP status 的歧义异常也只重试一次并复用 key', async () => {
    let calls = 0
    fetchImpl = () => {
      calls += 1
      if (calls === 1) return Promise.reject(new Error('connection reset'))
      return Promise.resolve(
        jsonResponse(200, { link_id: 'l1', status: 'done' }),
      )
    }
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const result = await client.ingest(
      { sources: [{ kind: 'browser_capture', url: 'https://example.com' }] },
      { idempotencyKey: 'capture-key-no-status' },
    )

    expect(result.ok).toBe(true)
    expect(calls).toBe(2)
    expect(
      (lastInit().headers as Record<string, string>)['Idempotency-Key'],
    ).toBe('capture-key-no-status')
  })

  it('ingest 不对明确业务错误重试', async () => {
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
})

// ── 成功响应归一化 ──

describe('成功响应归一化', () => {
  it('getTags 成功返回 { ok: true, data }', async () => {
    const tags = [{ tag: 'go', count: 3 }]
    fetchImpl = () => Promise.resolve(jsonResponse(200, tags))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTags()
    expect(res.ok).toBe(true)
    if (res.ok) expect(res.data).toEqual(tags)
  })

  it('getTree 成功返回完整树', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [
            {
              id: 'n1',
              url: 'https://example.com/',
              title: 'Example',
              summary: null,
              description: null,
              tags: ['a'],
              content_type: 'homepage',
              status: 'done',
              domain: 'example.com',
              path_depth: 0,
              parent_id: null,
              parent_path: null,
              fetcher_type: null,
              is_low_confidence: false,
              has_content: false,
              low_confidence_reason: null,
              error_category: null,
              error_msg: null,
              created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-01-01T00:00:00Z',
              metadata_revision: 1,
            },
            {
              id: 'n2',
              url: 'https://example.com/post',
              title: 'Post',
              summary: null,
              description: null,
              tags: [],
              content_type: 'article',
              status: 'done',
              domain: 'example.com',
              path_depth: null,
              parent_id: null,
              parent_path: '/',
              fetcher_type: null,
              is_low_confidence: false,
              has_content: false,
              low_confidence_reason: null,
              error_category: null,
              error_msg: null,
              created_at: '2026-01-01T00:01:00Z',
              updated_at: '2026-01-01T00:01:00Z',
              metadata_revision: 1,
            },
          ],
          total: 2,
          page: 0,
          limit: 100,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTree()
    expect(res.ok).toBe(true)
    if (res.ok) {
      expect(res.data.total).toBe(2)
      expect(res.data.nodes[0].id).toBe('n1')
      expect(res.data.nodes[0].children[0].id).toBe('n2')
    }
  })

  it('ingest 成功返回 SubmitResponse', async () => {
    fetchImpl = () =>
      Promise.resolve(jsonResponse(200, { link_id: 'l1', status: 'pending' }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.ingest({ sources: [] })
    expect(res.ok).toBe(true)
    if (res.ok) expect(res.data.link_id).toBe('l1')
  })

  it('Inbox SubmitResponse 可以没有 link_id', async () => {
    const response = {
      inbox_id: 'inbox-1',
      destination: 'inbox',
      status: 'pending',
    }
    fetchImpl = () => Promise.resolve(jsonResponse(200, response))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const result = await client.ingest({ sources: [] })

    expect(result).toEqual({ ok: true, data: response })
  })

  it.each([
    [
      '同时返回 link_id 与 inbox_id',
      { link_id: 'l1', inbox_id: 'i1', status: 'done' },
    ],
    ['缺少 durable identity', { status: 'done' }],
    [
      'Inbox 响应返回 link_id',
      { link_id: 'l1', destination: 'inbox', status: 'done' },
    ],
    ['空 durable identity', { link_id: ' ', status: 'done' }],
    [
      'library 响应返回 inbox_id',
      { inbox_id: 'i1', destination: 'library', status: 'done' },
    ],
  ])('拒绝判别式不一致的 SubmitResponse：%s', async (_label, response) => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, response))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const result = await client.ingest({ sources: [] })

    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error.kind).toBe('other')
  })
})

// ── 错误归一化 ──

describe('错误归一化', () => {
  it('401 归一化为 unauthorized 并透传 error_code', async () => {
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
    const res = await client.getTags()
    expect(res.ok).toBe(false)
    if (!res.ok) {
      expect(res.error.kind).toBe('unauthorized')
      expect(res.error.status).toBe(401)
      expect(res.error.errorCode).toBe('unauthorized')
      expect(res.error.message).toBe('bad token')
    }
  })

  it('403 也归一化为 unauthorized', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(403, {}))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTags()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('unauthorized')
  })

  it('fetch 抛 TypeError 归一化为 network-unreachable', async () => {
    fetchImpl = () => Promise.reject(new TypeError('Failed to fetch'))
    const client = createWebTagClient({
      baseURL: 'http://unreachable',
      token: 't',
    })
    const res = await client.getTags()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('network-unreachable')
  })

  it('AbortError 归一化为 timeout', async () => {
    fetchImpl = () => Promise.reject(new DOMException('aborted', 'AbortError'))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTags()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('timeout')
  })

  it('普通 Error 名为 AbortError 也归一化为 timeout', async () => {
    fetchImpl = () => {
      const e = new Error('aborted')
      e.name = 'AbortError'
      return Promise.reject(e)
    }
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTags()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('timeout')
  })

  it('真实超时路径：fetch 永不 resolve，DEFAULT_TIMEOUT 后由 AbortController 触发 timeout', async () => {
    // 上面两个用例只模拟「fetch 直接 reject 一个 AbortError」，并未走过
    // client.request 内真实的 AbortController + setTimeout(DEFAULT_TIMEOUT)
    // + clearTimeout 路径。此用例用 fake timers 驱动真实计时器：
    // fetchImpl 返回一个永不 resolve 的 Promise，但监听 init.signal 的
    // abort 事件——计时器推进到 DEFAULT_TIMEOUT 时 controller.abort() 触发，
    // fetchImpl 据此 reject 一个 AbortError DOMException（与原生 fetch 在
    // signal 触发时的行为一致），归一化层据此得到 timeout。
    vi.useFakeTimers()
    try {
      fetchImpl = (_input, init) =>
        new Promise((_resolve, reject) => {
          const signal = init?.signal
          if (!signal) return
          signal.addEventListener('abort', () => {
            reject(new DOMException('The operation was aborted.', 'AbortError'))
          })
        })
      const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
      const resPromise = client.getTags()
      // 推进到 DEFAULT_TIMEOUT（8000ms），触发 setTimeout → controller.abort()。
      await vi.advanceTimersByTimeAsync(8000)
      const res = await resPromise
      expect(res.ok).toBe(false)
      if (!res.ok) expect(res.error.kind).toBe('timeout')
    } finally {
      vi.useRealTimers()
    }
  })

  it('响应体挂起路径：headers 已回但 res.text() 永不 resolve，DEFAULT_TIMEOUT 后归一化为 timeout', async () => {
    // H3 回归：超时计时器必须覆盖响应体消费阶段，而非在 await fetch() 后立即
    // clearTimeout。此用例模拟「后端/反代回了 headers，但 body 永远不来」：
    // fetch resolve 一个 Response，其 .text() 返回的 Promise 永不 resolve，
    // 但监听 init.signal 的 abort——计时器推进到 DEFAULT_TIMEOUT 时
    // controller.abort() 触发，body 读取据此 reject 一个 AbortError，
    // request() 的 catch 把它归一化为 timeout，而非无限挂起。
    vi.useFakeTimers()
    try {
      fetchImpl = (_input, init) => {
        const signal = init?.signal
        // 构造一个 .text() 永挂起、但响应 abort 信号的 Response 替身。
        const stalledResponse = {
          ok: true,
          status: 200,
          text: () =>
            new Promise<string>((_resolve, reject) => {
              if (!signal) return
              signal.addEventListener('abort', () => {
                reject(
                  new DOMException('The operation was aborted.', 'AbortError'),
                )
              })
            }),
        }
        return Promise.resolve(stalledResponse as unknown as Response)
      }
      const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
      const resPromise = client.getTags()
      // 推进到 DEFAULT_TIMEOUT（8000ms）：fetch 已 resolve（headers 已回），
      // 计时器到点 → controller.abort() → 挂起的 res.text() 抛 AbortError。
      await vi.advanceTimersByTimeAsync(8000)
      const res = await resPromise
      // 关键断言：响应体挂起必须以 timeout 终结，不能无限挂起或归为 other。
      expect(res.ok).toBe(false)
      if (!res.ok) expect(res.error.kind).toBe('timeout')
    } finally {
      vi.useRealTimers()
    }
  })

  it('响应体挂起路径同样覆盖 getLinks（loading 不会无限卡住）', async () => {
    // 库列表路径：body 挂起若不超时，UI loading spinner 会永远卡住。
    vi.useFakeTimers()
    try {
      fetchImpl = (_input, init) => {
        const signal = init?.signal
        const stalledResponse = {
          ok: true,
          status: 200,
          text: () =>
            new Promise<string>((_resolve, reject) => {
              if (!signal) return
              signal.addEventListener('abort', () => {
                reject(
                  new DOMException('The operation was aborted.', 'AbortError'),
                )
              })
            }),
        }
        return Promise.resolve(stalledResponse as unknown as Response)
      }
      const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
      const resPromise = client.getLinks()
      await vi.advanceTimersByTimeAsync(8000)
      const res = await resPromise
      expect(res.ok).toBe(false)
      if (!res.ok) expect(res.error.kind).toBe('timeout')
    } finally {
      vi.useRealTimers()
    }
  })

  it('500 归一化为 other 并保留 status 与 error_code', async () => {
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
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getLinks()
    expect(res.ok).toBe(false)
    if (!res.ok) {
      expect(res.error.kind).toBe('other')
      expect(res.error.status).toBe(500)
      expect(res.error.errorCode).toBe('internal_error')
    }
  })

  it('非 2xx 且响应体非 JSON 归一化为 other 并保留 status', async () => {
    fetchImpl = () =>
      Promise.resolve(
        makeResponse({ status: 502, bodyText: '<html>Bad Gateway</html>' }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTags()
    expect(res.ok).toBe(false)
    if (!res.ok) {
      expect(res.error.kind).toBe('other')
      expect(res.error.status).toBe(502)
    }
  })
})

// ── generated wire 响应运行时校验 ──

describe('运行时校验：getTree', () => {
  it('底层 /api/links 的 items 为 null 时失败关闭返回 other 错误', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, { items: null, total: 0, page: 0, limit: 100 }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTree()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('底层 /api/links 缺 items 字段时失败关闭返回 other 错误', async () => {
    fetchImpl = () =>
      Promise.resolve(jsonResponse(200, { total: 5, page: 0, limit: 100 }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTree()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('空 links 页可组成空树', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, { items: [], total: 0, page: 0, limit: 100 }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTree()
    expect(res.ok).toBe(true)
    if (res.ok) {
      expect(res.data.total).toBe(0)
      expect(res.data.nodes).toEqual([])
    }
  })

  it('Link.tags 为 null 时 getTree 透传 getLinks 的 fail-closed 错误', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [
            {
              id: 'n1',
              url: 'https://example.com/',
              title: 'Example',
              summary: null,
              description: null,
              tags: null,
              content_type: 'homepage',
              status: 'done',
              domain: 'example.com',
              path_depth: 0,
              parent_id: null,
              parent_path: null,
              fetcher_type: null,
              is_low_confidence: false,
              has_content: false,
              low_confidence_reason: null,
              error_category: null,
              error_msg: null,
              created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-01-01T00:00:00Z',
            },
          ],
          total: 1,
          page: 0,
          limit: 100,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTree()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('按 URL 推导父子关系', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [
            {
              id: 'root',
              url: 'https://example.com/',
              title: 'Example',
              summary: null,
              description: null,
              tags: [],
              content_type: 'homepage',
              status: 'done',
              domain: 'example.com',
              path_depth: 0,
              parent_id: null,
              parent_path: null,
              fetcher_type: null,
              is_low_confidence: false,
              has_content: false,
              low_confidence_reason: null,
              error_category: null,
              error_msg: null,
              created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-01-01T00:00:00Z',
              metadata_revision: 1,
            },
            {
              id: 'child',
              url: 'https://example.com/posts/a',
              title: 'Post A',
              summary: null,
              description: null,
              tags: [],
              content_type: 'article',
              status: 'done',
              domain: 'example.com',
              path_depth: 2,
              parent_id: null,
              parent_path: '/posts/',
              fetcher_type: null,
              is_low_confidence: false,
              has_content: false,
              low_confidence_reason: null,
              error_category: null,
              error_msg: null,
              created_at: '2026-01-01T00:01:00Z',
              updated_at: '2026-01-01T00:01:00Z',
              metadata_revision: 1,
            },
          ],
          total: 2,
          page: 0,
          limit: 100,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTree()
    expect(res.ok).toBe(true)
    if (res.ok) {
      expect(res.data.nodes[0].id).toBe('root')
      expect(res.data.nodes[0].children[0].url).toBe(
        'https://example.com/posts/',
      )
      expect(res.data.nodes[0].children[0].children[0].id).toBe('child')
    }
  })

  it('为缺失祖先生成虚拟 URL 层级且 total 只统计真实链接', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [
            {
              id: 'leaf',
              url: 'https://example.com/a/b/c',
              title: 'Leaf',
              summary: null,
              description: null,
              tags: [],
              content_type: 'article',
              status: 'done',
              domain: 'example.com',
              path_depth: 3,
              parent_id: null,
              parent_path: '/a/b/',
              fetcher_type: null,
              is_low_confidence: false,
              has_content: false,
              low_confidence_reason: null,
              error_category: null,
              error_msg: null,
              created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-01-01T00:00:00Z',
              metadata_revision: 1,
            },
          ],
          total: 1,
          page: 0,
          limit: 100,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTree()
    expect(res.ok).toBe(true)
    if (res.ok) {
      expect(res.data.total).toBe(1)
      const root = res.data.nodes[0]
      expect(root.virtual).toBe(true)
      expect(root.url).toBe('https://example.com/')
      expect(root.children[0].url).toBe('https://example.com/a/')
      expect(root.children[0].children[0].url).toBe('https://example.com/a/b/')
      expect(root.children[0].children[0].children[0].id).toBe('leaf')
    }
  })

  it('将无尾斜杠的真实链接匹配为路径祖先', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [
            {
              id: 'docs',
              url: 'https://example.com/docs',
              title: 'Docs',
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
            },
            {
              id: 'page',
              url: 'https://example.com/docs/page',
              title: 'Page',
              summary: null,
              description: null,
              tags: [],
              content_type: 'article',
              status: 'done',
              domain: 'example.com',
              path_depth: 2,
              parent_id: null,
              parent_path: '/docs/',
              fetcher_type: null,
              is_low_confidence: false,
              has_content: false,
              low_confidence_reason: null,
              error_category: null,
              error_msg: null,
              created_at: '2026-01-01T00:01:00Z',
              updated_at: '2026-01-01T00:01:00Z',
              metadata_revision: 1,
            },
          ],
          total: 2,
          page: 0,
          limit: 100,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTree()
    expect(res.ok).toBe(true)
    if (res.ok) {
      const root = res.data.nodes[0]
      expect(root.virtual).toBe(true)
      expect(root.children[0].id).toBe('docs')
      expect(root.children[0].children[0].id).toBe('page')
    }
  })

  it('响应体不是对象（数组）时返回 other 错误', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, [1, 2, 3]))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTree()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('200 携带后端错误体时返回 other 错误', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          error: { code: 500, error_code: 'oops', message: 'masked error' },
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTree()
    expect(res.ok).toBe(false)
    if (!res.ok) {
      expect(res.error.kind).toBe('other')
      expect(res.error.errorCode).toBe('oops')
    }
  })
})

describe('运行时校验：getLinks', () => {
  it('items 为 null（顶层必需字段非数组）时失败关闭返回 other 错误', async () => {
    // 失败关闭：items 是分页契约的硬要求，非数组绝不伪装成「空知识库」。
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, { items: null, total: 0, page: 1, limit: 20 }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getLinks()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('Link.tags 为 null 时整个分页响应失败关闭', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [{ ...makeWireLink(), tags: null }],
          total: 1,
          page: 1,
          limit: 20,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getLinks()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('完整 LinkResponse 可通过 getLinks public interface', async () => {
    const link = makeWireLink()
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, { items: [link], total: 1, page: 1, limit: 20 }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.getLinks()

    expect(res).toEqual({
      ok: true,
      data: { items: [link], total: 1, page: 1, limit: 20 },
    })
  })

  it('Link 缺 status 时整个分页响应失败关闭', async () => {
    const { status: _status, ...linkWithoutStatus } = makeWireLink()
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [linkWithoutStatus],
          total: 1,
          page: 1,
          limit: 20,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.getLinks()

    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('Link status 不在 generated enum 时整个分页响应失败关闭', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [{ ...makeWireLink(), status: 'queued' }],
          total: 1,
          page: 1,
          limit: 20,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.getLinks()

    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('Link 缺常驻 nullable 字段 error_msg 时整个分页响应失败关闭', async () => {
    const { error_msg: _errorMsg, ...linkWithoutErrorMessage } = makeWireLink()
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [linkWithoutErrorMessage],
          total: 1,
          page: 1,
          limit: 20,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.getLinks()

    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('total/page/limit 缺失（顶层必需数值字段）时失败关闭返回 other 错误', async () => {
    // 仅有 items、缺 total/page/limit：分页元数据不可信，必须报错而非返回空分页。
    fetchImpl = () => Promise.resolve(jsonResponse(200, { items: [] }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getLinks()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('total/page/limit 非 number（类型错）时失败关闭返回 other 错误', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [],
          total: 'lots',
          page: 1,
          limit: 20,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getLinks()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('total/page/limit 为小数时违反 integer 契约并失败关闭', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [],
          total: 1.5,
          page: 1,
          limit: 20,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.getLinks()

    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('Link.path_depth 为小数时违反 integer 契约并失败关闭', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [makeWireLink({ path_depth: 1.5 })],
          total: 1,
          page: 1,
          limit: 20,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.getLinks()

    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('next_cursor 为字符串时透传', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [],
          total: 0,
          page: 0,
          limit: 20,
          next_cursor: 'abc',
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getLinks()
    expect(res.ok).toBe(true)
    if (res.ok) expect(res.data.next_cursor).toBe('abc')
  })

  it('响应体不是对象时返回 other 错误', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, 'not an object'))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getLinks()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })
})

describe('运行时校验：getTags', () => {
  it('响应体不是数组（对象）时返回 other 错误', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, { tags: ['go'] }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTags()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('响应体为 null 时返回 other 错误', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, null))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTags()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('数组中任一残缺 Tag 都让整个响应失败关闭', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, [
          { tag: 'go', count: 3 },
          { tag: 'web' /* 无 count */ },
          { count: 5 /* 无 tag */ },
          null,
          'garbage',
        ]),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTags()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('Tag.count 为小数时违反 integer 契约并失败关闭', async () => {
    fetchImpl = () =>
      Promise.resolve(jsonResponse(200, [{ tag: 'go', count: 1.5 }]))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.getTags()

    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })
})

describe('运行时校验：ingest', () => {
  it('缺 link_id 字段时返回 other 错误', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, { status: 'pending' }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.ingest({ sources: [] })
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('缺 status 字段时返回 other 错误', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, { link_id: 'l1' }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.ingest({ sources: [] })
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('status 不在 generated Submit enum 时返回 other 错误', async () => {
    fetchImpl = () =>
      Promise.resolve(jsonResponse(200, { link_id: 'l1', status: 'queued' }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.ingest({ sources: [] })

    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('200 携带后端错误体时返回 other 错误', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          error: { code: 422, error_code: 'invalid_source', message: 'bad' },
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.ingest({ sources: [] })
    expect(res.ok).toBe(false)
    if (!res.ok) {
      expect(res.error.kind).toBe('other')
      expect(res.error.errorCode).toBe('invalid_source')
    }
  })
})

describe('运行时校验：saveLinkContent', () => {
  it('向编码后的链接原文路径发 POST 并返回完整快照', async () => {
    const response = {
      link_id: 'link/1',
      content: '公众号正文',
      content_document: '# 公众号正文',
      content_format: 'markdown' as const,
      fetcher_type: 'browser_capture',
      content_source: 'fetched' as const,
      // 后端保存正文后回传的代次。它是 required 字段，缺了要失败关闭。
      content_revision: 1,
    }
    fetchImpl = () => Promise.resolve(jsonResponse(200, response))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.saveLinkContent('link/1')

    expect(lastUrl()).toBe('http://x/api/links/link%2F1/content')
    expect(lastInit().method).toBe('POST')
    expect(res).toEqual({ ok: true, data: response })
  })

  it('响应缺少 content_format 时失败关闭', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          link_id: 'link-1',
          content: '公众号正文',
          fetcher_type: 'browser_capture',
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.saveLinkContent('link-1')

    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })
})

describe('运行时校验：refreshLink', () => {
  it('成功返回 SubmitResponse', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          link_id: 'l1',
          status: 'pending',
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.refreshLink('l1')
    expect(res.ok).toBe(true)
    if (res.ok) {
      expect(res.data.link_id).toBe('l1')
      expect(res.data.status).toBe('pending')
    }
  })

  it('缺 link_id / status 字段时返回 other 错误（复用 validateSubmitResponse）', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, { status: 'pending' }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.refreshLink('l1')
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('非法 status 通过共享 Submit guard 失败关闭', async () => {
    fetchImpl = () =>
      Promise.resolve(jsonResponse(200, { link_id: 'l1', status: 'queued' }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const res = await client.refreshLink('l1')

    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('401 时归一化为 unauthorized', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(401, {
          error: { code: 401, error_code: 'unauthorized', message: 'no' },
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.refreshLink('l1')
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('unauthorized')
  })

  it('429 冷却窗 + Retry-After：归一化为 rate-limited，带 errorCode 与秒数', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(
          429,
          {
            error: {
              code: 429,
              error_code: 'cooldown_active',
              message: 'refresh cooldown active',
            },
          },
          { 'Retry-After': '12' },
        ),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.refreshLink('l1')
    expect(res.ok).toBe(false)
    if (!res.ok) {
      expect(res.error.kind).toBe('rate-limited')
      expect(res.error.status).toBe(429)
      expect(res.error.errorCode).toBe('cooldown_active')
      expect(res.error.retryAfterSeconds).toBe(12)
    }
  })

  it('429 无 Retry-After 头：仍为 rate-limited 但不带 retryAfterSeconds', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(429, {
          error: {
            code: 429,
            error_code: 'rate_limit_exceeded',
            message: 'rate limit exceeded',
          },
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.refreshLink('l1')
    expect(res.ok).toBe(false)
    if (!res.ok) {
      expect(res.error.kind).toBe('rate-limited')
      expect(res.error.retryAfterSeconds).toBeUndefined()
    }
  })

  it('429 非整数 / 非法 Retry-After 头：忽略秒数（仍为 rate-limited）', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(
          429,
          {
            error: {
              code: 429,
              error_code: 'cooldown_active',
              message: 'cooldown',
            },
          },
          // HTTP-date 形式不解析；前导/异常文本一律忽略。
          { 'Retry-After': 'Wed, 21 Oct 2025 07:28:00 GMT' },
        ),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.refreshLink('l1')
    expect(res.ok).toBe(false)
    if (!res.ok) {
      expect(res.error.kind).toBe('rate-limited')
      expect(res.error.retryAfterSeconds).toBeUndefined()
    }
  })

  it('429 响应体非 JSON 但带 Retry-After：仍解析秒数', async () => {
    fetchImpl = () =>
      Promise.resolve(
        makeResponse({
          status: 429,
          bodyText: 'Too Many Requests',
          headers: { 'Retry-After': '5' },
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.refreshLink('l1')
    expect(res.ok).toBe(false)
    if (!res.ok) {
      expect(res.error.kind).toBe('rate-limited')
      expect(res.error.retryAfterSeconds).toBe(5)
    }
  })
})

describe('运行时校验：非 JSON 的 200 响应', () => {
  it('反代登录页（HTML 200）返回 other 错误且不抛异常', async () => {
    fetchImpl = () =>
      Promise.resolve(
        makeResponse({
          status: 200,
          bodyText: '<!DOCTYPE html><html><body>Login</body></html>',
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTree()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('空 body 的 200 对 getTags 返回 other 错误（非数组）', async () => {
    fetchImpl = () => Promise.resolve(makeResponse({ bodyText: '' }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.getTags()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })

  it('任何畸形响应都不抛异常', async () => {
    fetchImpl = () =>
      Promise.resolve(makeResponse({ bodyText: '{ broken json' }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    await expect(client.getTree()).resolves.toBeDefined()
    await expect(client.getLinks()).resolves.toBeDefined()
  })
})

// ── Reader capability / pending badge probes ──

describe('Reader compatibility probes', () => {
  it('capabilities 404 is represented as an older backend', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(404, {}))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await expect(client.getCapabilities()).resolves.toEqual({
      ok: true,
      data: null,
    })
  })

  it('capabilities 403 remains an authentication failure', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(403, {
          error: { code: 403, error_code: 'unauthorized', message: 'no' },
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    const result = await client.getCapabilities()

    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error.kind).toBe('unauthorized')
  })

  it('读取 authenticated session identity，并使用专用 endpoint', async () => {
    const response = {
      client_data_namespace: 'namespace-a',
      representation_contract: 'v3',
    }
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, response, {
          'X-WebTag-Data-Namespace': 'namespace-a',
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await expect(client.getSessionIdentity()).resolves.toEqual({
      ok: true,
      data: response,
    })
    expect(lastUrl()).toBe('http://x/api/session')
    expect(lastInit().method).toBe('GET')
  })

  it('session identity namespace marker 缺失或与 body 不一致时 fail closed', async () => {
    const response = {
      client_data_namespace: 'namespace-a',
      representation_contract: 'v3',
    }
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    fetchImpl = () => Promise.resolve(jsonResponse(200, response))
    const missing = await client.getSessionIdentity()
    expect(missing.ok).toBe(false)
    if (!missing.ok) expect(missing.error.kind).toBe('identity-mismatch')

    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, response, {
          'X-WebTag-Data-Namespace': 'namespace-b',
        }),
      )
    const mismatch = await client.getSessionIdentity()
    expect(mismatch.ok).toBe(false)
    if (!mismatch.ok) expect(mismatch.error.kind).toBe('identity-mismatch')
  })

  it('session identity 畸形或未授权响应 fail closed', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          client_data_namespace: '',
          representation_contract: 'v3',
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const malformed = await client.getSessionIdentity()
    expect(malformed.ok).toBe(false)
    if (!malformed.ok) expect(malformed.error.kind).toBe('other')

    fetchImpl = () => Promise.resolve(jsonResponse(401, {}))
    const unauthorized = await client.getSessionIdentity()
    expect(unauthorized.ok).toBe(false)
    if (!unauthorized.ok) expect(unauthorized.error.kind).toBe('unauthorized')
  })

  it('只读取 active-only 的 counts.inbox，不合并已过期计数', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, { counts: { inbox: 7, inbox_expired: 4 } }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await expect(client.getReaderPendingCount()).resolves.toEqual({
      ok: true,
      data: 7,
    })
  })

  it('Reader Home 404 clears compatibility state through a null count', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(404, {}))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })

    await expect(client.getReaderPendingCount()).resolves.toEqual({
      ok: true,
      data: null,
    })
  })
})

// ── testConnection ──

describe('testConnection', () => {
  it('后端可达且鉴权通过时返回 ok（探活复用 GET /api/tags）', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, []))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.testConnection()
    expect(res.ok).toBe(true)
    expect(lastUrl()).toBe('http://x/api/tags')
    expect(lastInit().method).toBe('GET')
  })

  it('Token 错误时返回 unauthorized 错误', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(401, {}))
    const client = createWebTagClient({ baseURL: 'http://x', token: 'wrong' })
    const res = await client.testConnection()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('unauthorized')
  })

  it('后端不可达时返回 network-unreachable 错误', async () => {
    fetchImpl = () => Promise.reject(new TypeError('Failed to fetch'))
    const client = createWebTagClient({ baseURL: 'http://nope', token: 't' })
    const res = await client.testConnection()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('network-unreachable')
  })

  it('后端返回畸形响应（非数组）时返回 other 错误', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, { unexpected: true }))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.testConnection()
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('other')
  })
})

// ── buildWebTagClientFromSettings ──

describe('buildWebTagClientFromSettings', () => {
  it('backendUrl 为空字符串时返回 null（未配置）', () => {
    expect(
      buildWebTagClientFromSettings({ backendUrl: '', accessToken: 't' }),
    ).toBeNull()
  })

  it('backendUrl 仅含空白时也视为未配置返回 null', () => {
    expect(
      buildWebTagClientFromSettings({ backendUrl: '   ', accessToken: 't' }),
    ).toBeNull()
  })

  it('backendUrl 非空时构造客户端并 trim 地址与 Token', async () => {
    fetchImpl = () => Promise.resolve(jsonResponse(200, []))
    const client = buildWebTagClientFromSettings({
      backendUrl: '  http://localhost:8080  ',
      accessToken: '  secret  ',
    })
    expect(client).not.toBeNull()
    await client!.getTags()
    // 地址已 trim（normalizeBaseURL 也会 trim），Token 已 trim 后注入 Bearer 头。
    expect(lastUrl()).toBe('http://localhost:8080/api/tags')
    expect((lastInit().headers as Record<string, string>).Authorization).toBe(
      'Bearer secret',
    )
  })
})

// ── findByUrl：精确已存检测 + feature-detect（v1.1） ──
describe('findByUrl — 精确已存检测与 feature-detect', () => {
  function linkItem(url: string, id = 'l1') {
    return makeWireLink({ id, url })
  }

  it('请求带 url 与 limit=2 查询参数', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, { items: [], total: 0, page: 1, limit: 2 }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    await client.findByUrl('https://a.com/p?q=1')
    const url = lastUrl()
    expect(url).toContain('/api/links?')
    expect(url).toContain('url=https%3A%2F%2Fa.com%2Fp%3Fq%3D1')
    expect(url).toContain('limit=2')
  })

  it('恰一条且 URL 一致 → supported:true 命中（link 非空）', async () => {
    const target = 'https://a.com/exact'
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [linkItem(target)],
          total: 1,
          page: 1,
          limit: 2,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.findByUrl(target)
    expect(res.ok).toBe(true)
    if (res.ok && res.data.supported) {
      expect(res.data.link?.url).toBe(target)
    } else {
      throw new Error('应判为 supported 且命中')
    }
  })

  it('带 # 锚点的页面：后端返回 %23 归一化 URL 仍判命中（回归：fragment false-negative）', async () => {
    // 后端 Go ParseRequestURI().String() 把 tab.url 中的 "#" 编码为 "%23"，
    // 严格 === 会把真实命中误判"未命中"——比较需接受该归一化变体。
    const browserUrl = 'https://a.com/docs#section-2'
    const backendUrl = 'https://a.com/docs%23section-2'
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [linkItem(backendUrl)],
          total: 1,
          page: 1,
          limit: 2,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.findByUrl(browserUrl)
    expect(res.ok).toBe(true)
    if (res.ok && res.data.supported) {
      expect(res.data.link?.url).toBe(backendUrl)
    } else {
      throw new Error('归一化变体应判为 supported 且命中')
    }
  })

  it('0 条 → supported:true 未命中（link 为 null）', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, { items: [], total: 0, page: 1, limit: 2 }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.findByUrl('https://a.com/missing')
    expect(res.ok).toBe(true)
    if (res.ok && res.data.supported) {
      expect(res.data.link).toBeNull()
    } else {
      throw new Error('应判为 supported 但未命中')
    }
  })

  it('返回多于 1 条（旧后端忽略 url 返回普通列表）→ supported:false', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [
            linkItem('https://a.com/1', 'l1'),
            linkItem('https://a.com/2', 'l2'),
          ],
          total: 99,
          page: 1,
          limit: 2,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.findByUrl('https://a.com/query')
    expect(res.ok).toBe(true)
    if (res.ok) expect(res.data.supported).toBe(false)
  })

  it('唯一一条但 URL 与查询不一致（旧后端恰好库里只 1 条）→ supported:false，不误报', async () => {
    fetchImpl = () =>
      Promise.resolve(
        jsonResponse(200, {
          items: [linkItem('https://other.com/unrelated')],
          total: 1,
          page: 1,
          limit: 2,
        }),
      )
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.findByUrl('https://a.com/query')
    expect(res.ok).toBe(true)
    // 关键：旧后端返回的不相关单条不得被误读为「已在库中」。
    if (res.ok) expect(res.data.supported).toBe(false)
  })

  it('底层请求失败时透传 error（调用方据此 fail-soft）', async () => {
    fetchImpl = () => Promise.reject(new TypeError('Failed to fetch'))
    const client = createWebTagClient({ baseURL: 'http://x', token: 't' })
    const res = await client.findByUrl('https://a.com/p')
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.error.kind).toBe('network-unreachable')
  })
})
