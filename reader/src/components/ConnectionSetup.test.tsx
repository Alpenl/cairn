import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ConnectionSetup } from './ConnectionSetup'

function mockFetch(fn: (url: string, init?: RequestInit) => Response) {
  vi.stubGlobal('fetch', vi.fn(fn))
}

const VALID_NAMESPACE = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'

function identityResponse(created = false): Response {
  return new Response(
    JSON.stringify({
      ...(created ? { expires_at: '2030-01-01T00:00:00Z' } : {}),
      client_data_namespace: VALID_NAMESPACE,
      representation_contract: 'v3',
    }),
    {
      status: created ? 201 : 200,
      headers: { 'X-WebTag-Data-Namespace': VALID_NAMESPACE },
    },
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  localStorage.clear()
})

describe('ConnectionSetup 测试连接', () => {
  it('连通成功显示「连接成功」', async () => {
    const fetchSpy = vi.fn((_url: string, init?: RequestInit) =>
      init?.method === 'POST'
        ? new Response('not found', { status: 404 })
        : identityResponse(),
    )
    vi.stubGlobal('fetch', fetchSpy)
    render(<ConnectionSetup onSaved={() => {}} />)
    const user = userEvent.setup()
    await user.type(screen.getByPlaceholderText('http://localhost:8080'), 'http://localhost:8080')
    await user.click(screen.getByRole('button', { name: /测试连接/ }))
    await waitFor(() => expect(screen.getByText('连接成功')).toBeInTheDocument())
    expect(fetchSpy).toHaveBeenCalledTimes(2)
    expect(fetchSpy.mock.calls.every(([url]) => String(url).endsWith('/api/session'))).toBe(true)
  })

  it('401 显示安装令牌无效', async () => {
    mockFetch(
      () => new Response(JSON.stringify({ error: { code: 401, message: 'x' } }), { status: 401 }),
    )
    render(<ConnectionSetup onSaved={() => {}} />)
    const user = userEvent.setup()
    await user.type(screen.getByPlaceholderText('http://localhost:8080'), 'http://localhost:8080')
    await user.click(screen.getByRole('button', { name: /测试连接/ }))
    await waitFor(() => expect(screen.getByText(/安装令牌无效/)).toBeInTheDocument())
  })

  it('网络不可达显示提示', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => {
        throw new TypeError('Failed to fetch')
      }),
    )
    render(<ConnectionSetup onSaved={() => {}} />)
    const user = userEvent.setup()
    await user.type(screen.getByPlaceholderText('http://localhost:8080'), 'http://localhost:8080')
    await user.click(screen.getByRole('button', { name: /测试连接/ }))
    await waitFor(() => expect(screen.getByText(/无法连接到该地址/)).toBeInTheDocument())
  })

  // 后端支持会话时首选会话模式：凭证换成 httpOnly cookie，前端一份都不留。
  it('后端支持会话时保存 session 模式且不带安装令牌', async () => {
    const onSaved = vi.fn()
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_url: string, init?: RequestInit) =>
        init?.method === 'POST' ? identityResponse(true) : identityResponse(),
      ),
    )
    render(<ConnectionSetup onSaved={onSaved} />)
    const user = userEvent.setup()
    await user.type(screen.getByPlaceholderText('http://localhost:8080'), 'http://localhost:8080')
    await user.type(screen.getByPlaceholderText('留空则不带鉴权头'), 'wtk_secret')
    await user.click(screen.getByRole('button', { name: '保存并进入' }))

    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(onSaved).toHaveBeenCalledWith({
      baseURL: 'http://localhost:8080',
      mode: 'session',
      installationToken: '',
    })
    const fetchSpy = vi.mocked(fetch)
    expect(fetchSpy.mock.calls.every(([url]) => String(url).endsWith('/api/session'))).toBe(true)
  })

  it('只测试 session 支持时在验证后清理临时 cookie', async () => {
    const methods: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET'
      methods.push(method)
      if (method === 'POST') return identityResponse(true)
      if (method === 'DELETE') return new Response(null, { status: 204 })
      return identityResponse()
    }))
    render(<ConnectionSetup onSaved={() => {}} />)
    const user = userEvent.setup()
    await user.type(screen.getByPlaceholderText('http://localhost:8080'), 'http://localhost:8080')
    await user.type(screen.getByPlaceholderText('留空则不带鉴权头'), 'wtk_secret')
    await user.click(screen.getByRole('button', { name: /测试连接/ }))

    await waitFor(() => expect(screen.getByText('连接成功')).toBeInTheDocument())
    expect(methods).toEqual(['POST', 'GET', 'DELETE'])
  })

  // POST session 可以未挂载，但 RF2A 的 GET identity 必须存在；Bearer 回退仍只探测 identity。
  it('会话创建端点不存在时直接发送安装令牌', async () => {
    const onSaved = vi.fn()
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_url: string, init?: RequestInit) =>
        init?.method === 'POST' ? new Response('not found', { status: 404 }) : identityResponse(),
      ),
    )
    render(<ConnectionSetup onSaved={onSaved} />)
    const user = userEvent.setup()
    // 组件只做 trim；末尾斜杠的规范化由 settings.saveConnection 唯一负责（App 调用）。
    await user.type(screen.getByPlaceholderText('http://localhost:8080'), '  http://localhost:8080  ')
    await user.type(screen.getByPlaceholderText('留空则不带鉴权头'), 'wtk_secret')
    await user.click(screen.getByRole('button', { name: '保存并进入' }))

    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(onSaved).toHaveBeenCalledWith({
      baseURL: 'http://localhost:8080',
      mode: 'installation-token',
      installationToken: 'wtk_secret',
    })
    const fetchSpy = vi.mocked(fetch)
    expect(fetchSpy.mock.calls.every(([url]) => String(url).endsWith('/api/session'))).toBe(true)
    expect(fetchSpy.mock.calls[1]?.[1]?.headers).toEqual(
      expect.objectContaining({ Authorization: 'Bearer wtk_secret' }),
    )
  })

  // 凭证本身不对时不该再直接发送同一令牌。
  it('会话登录 401 时直接报错，不回退', async () => {
    const onSaved = vi.fn()
    const fetchSpy = vi.fn(
      async () => new Response(JSON.stringify({ error: { code: 401 } }), { status: 401 }),
    )
    vi.stubGlobal('fetch', fetchSpy)
    render(<ConnectionSetup onSaved={onSaved} />)
    const user = userEvent.setup()
    await user.type(screen.getByPlaceholderText('http://localhost:8080'), 'http://localhost:8080')
    await user.click(screen.getByRole('button', { name: /测试连接/ }))

    await waitFor(() => expect(screen.getByText(/安装令牌无效/)).toBeInTheDocument())
    expect(fetchSpy).toHaveBeenCalledTimes(1)
  })

  it.each([
    ['429', () => new Response(JSON.stringify({ error: { code: 429 } }), { status: 429 }), /请求被限流/, 1],
    ['503', () => new Response(JSON.stringify({ error: { code: 503 } }), { status: 503 }), /HTTP 503/, 1],
    ['非法成功响应', () => new Response('{not-json', { status: 201 }), /SessionCreated/, 2],
  ])('会话协商遇到 %s 时停止且不做 Bearer 探测', async (
    _name,
    response,
    expectedError,
    expectedCalls,
  ) => {
    const onSaved = vi.fn()
    const fetchSpy = vi.fn(async () => response())
    vi.stubGlobal('fetch', fetchSpy)
    render(<ConnectionSetup onSaved={onSaved} />)
    const user = userEvent.setup()
    await user.type(screen.getByPlaceholderText('http://localhost:8080'), 'http://localhost:8080')
    await user.type(screen.getByPlaceholderText('留空则不带鉴权头'), 'must-not-persist')
    await user.click(screen.getByRole('button', { name: '保存并进入' }))

    await waitFor(() => expect(screen.getByText(expectedError)).toBeInTheDocument())
    expect(fetchSpy).toHaveBeenCalledTimes(expectedCalls)
    expect(onSaved).not.toHaveBeenCalled()
    expect(localStorage.getItem('webtag:reader:conn:v2') ?? '').not.toContain('must-not-persist')
  })

  it('会话创建成功但 cookie identity 为 401 时才允许 Bearer 兼容探测', async () => {
    const onSaved = vi.fn()
    const fetchSpy = vi.fn(async (_url: string, init?: RequestInit) => {
      if (init?.method === 'POST') return identityResponse(true)
      if (init?.headers && 'Authorization' in (init.headers as Record<string, string>)) return identityResponse()
      return new Response(JSON.stringify({ error: { code: 401 } }), { status: 401 })
    })
    vi.stubGlobal('fetch', fetchSpy)
    render(<ConnectionSetup onSaved={onSaved} />)
    const user = userEvent.setup()
    await user.type(screen.getByPlaceholderText('http://localhost:8080'), 'http://localhost:8080')
    await user.type(screen.getByPlaceholderText('留空则不带鉴权头'), 'compat-secret')
    await user.click(screen.getByRole('button', { name: '保存并进入' }))

    await waitFor(() => expect(onSaved).toHaveBeenCalledWith({
      baseURL: 'http://localhost:8080',
      mode: 'installation-token',
      installationToken: 'compat-secret',
    }))
    expect(fetchSpy.mock.calls.map(([, init]) => init?.method ?? 'GET')).toEqual([
      'POST',
      'GET',
      'DELETE',
      'GET',
    ])
  })

  it('session 持久化失败时清理 cookie 并显示错误', async () => {
    const onSaved = vi.fn(async () => {
      throw new Error('连接存储写入后校验失败')
    })
    const fetchSpy = vi.fn(async (_url: string, init?: RequestInit) => {
      if (init?.method === 'POST') return identityResponse(true)
      if (init?.method === 'DELETE') return new Response(null, { status: 204 })
      return identityResponse()
    })
    vi.stubGlobal('fetch', fetchSpy)
    render(<ConnectionSetup onSaved={onSaved} />)
    const user = userEvent.setup()
    await user.type(screen.getByPlaceholderText('http://localhost:8080'), 'http://localhost:8080')
    await user.type(screen.getByPlaceholderText('留空则不带鉴权头'), 'secret')
    await user.click(screen.getByRole('button', { name: '保存并进入' }))

    await waitFor(() => expect(screen.getByText('连接存储写入后校验失败')).toBeInTheDocument())
    expect(onSaved).toHaveBeenCalledOnce()
    expect(fetchSpy.mock.calls.map(([, init]) => init?.method ?? 'GET')).toEqual([
      'POST',
      'GET',
      'DELETE',
    ])
  })
})
