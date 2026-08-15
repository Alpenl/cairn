import { describe, expect, it, vi } from 'vitest'
import { probeSameOriginBackend } from './bootstrap'

const HEALTH_OK = JSON.stringify({ status: 'ok', version: '1.2.3' })
const VALID_NAMESPACE = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
const IDENTITY_OK = JSON.stringify({
  client_data_namespace: VALID_NAMESPACE,
  representation_contract: 'v3',
})

function json(body: string, status = 200, marker?: string): Response {
  return new Response(body, {
    status,
    headers: {
      'Content-Type': 'application/json',
      ...(marker ? { 'X-WebTag-Data-Namespace': marker } : {}),
    },
  })
}

/** 按 URL 分派的 fetch 桩。 */
function stub(routes: {
  health?: () => Response | Promise<Response>
  identity?: () => Response | Promise<Response>
}) {
  return vi.fn(async (url: string) => {
    if (url.includes('/health')) {
      if (!routes.health) throw new TypeError('network error')
      return routes.health()
    }
    if (!routes.identity) throw new TypeError('network error')
    return routes.identity()
  }) as unknown as typeof fetch
}

describe('probeSameOriginBackend', () => {
  it('同源是 Cairn 后端且免鉴权时报告 openAccess', async () => {
    const res = await probeSameOriginBackend(
      'http://localhost:8080',
      stub({
        health: () => json(HEALTH_OK),
        identity: () => json(IDENTITY_OK, 200, VALID_NAMESPACE),
      }),
    )
    expect(res).toEqual({ baseURL: 'http://localhost:8080', openAccess: true })
  })

  it('同源后端要求鉴权时给出地址但 openAccess=false', async () => {
    const res = await probeSameOriginBackend(
      'http://localhost:8080',
      stub({ health: () => json(HEALTH_OK), identity: () => json('{}', 401) }),
    )
    expect(res).toEqual({ baseURL: 'http://localhost:8080', openAccess: false })
  })

  // 只看 200 会把任何一台静态服务器都当成 Cairn。health 必须带 status+version。
  it('同源返回 200 但不是 Cairn health 时判为未探到', async () => {
    const res = await probeSameOriginBackend(
      'http://localhost:8080',
      stub({
        health: () => json(JSON.stringify({ hello: 'world' })),
        identity: () => json(IDENTITY_OK, 200, VALID_NAMESPACE),
      }),
    )
    expect(res.baseURL).toBeNull()
  })

  it('health 404（纯静态托管）时判为未探到', async () => {
    const res = await probeSameOriginBackend(
      'http://cdn.example.com',
      stub({
        health: () => json('{}', 404),
        identity: () => json(IDENTITY_OK, 200, VALID_NAMESPACE),
      }),
    )
    expect(res.baseURL).toBeNull()
  })

  it('health 请求整个失败时不抛异常，判为未探到', async () => {
    const res = await probeSameOriginBackend('http://localhost:8080', stub({}))
    expect(res.baseURL).toBeNull()
  })

  // identity 探测本身抖掉时保守判需鉴权：误判成开放会让用户对着空列表猜错在哪。
  it('鉴权探测失败时保守判为需要安装令牌', async () => {
    const res = await probeSameOriginBackend(
      'http://localhost:8080',
      stub({ health: () => json(HEALTH_OK) }),
    )
    expect(res).toEqual({ baseURL: 'http://localhost:8080', openAccess: false })
  })

  it('非 http(s) origin 直接判为未探到，不发请求', async () => {
    const fetchImpl = stub({
      health: () => json(HEALTH_OK),
      identity: () => json(IDENTITY_OK, 200, VALID_NAMESPACE),
    })
    const res = await probeSameOriginBackend('file://', fetchImpl)
    expect(res.baseURL).toBeNull()
    expect(fetchImpl).not.toHaveBeenCalled()
  })

  it('裁掉 origin 末尾斜杠，避免拼出 //health', async () => {
    const fetchImpl = stub({
      health: () => json(HEALTH_OK),
      identity: () => json(IDENTITY_OK, 200, VALID_NAMESPACE),
    })
    const res = await probeSameOriginBackend('http://localhost:8080/', fetchImpl)
    expect(res.baseURL).toBe('http://localhost:8080')
    expect((fetchImpl as unknown as ReturnType<typeof vi.fn>).mock.calls[0][0]).toBe(
      'http://localhost:8080/health',
    )
    expect((fetchImpl as unknown as ReturnType<typeof vi.fn>).mock.calls[1][0]).toBe(
      'http://localhost:8080/api/session',
    )
  })

  it('markerless identity never counts as open access and no private collection is probed', async () => {
    const fetchImpl = stub({
      health: () => json(HEALTH_OK),
      identity: () => json(IDENTITY_OK),
    })

    await expect(probeSameOriginBackend('http://localhost:8080', fetchImpl)).resolves.toEqual({
      baseURL: 'http://localhost:8080',
      openAccess: false,
    })
    const calls = (fetchImpl as unknown as ReturnType<typeof vi.fn>).mock.calls
    expect(calls.map(([url]) => url)).toEqual([
      'http://localhost:8080/health',
      'http://localhost:8080/api/session',
    ])
  })
})
