import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { err, ok } from '../lib/api/result'
import type { IdentityBoundReaderClient } from '../lib/api/client'
import type { ReaderActivityResponse } from '../lib/api/types'
import { IdentityLease } from '../lib/identity'
import { resourceStore } from '../lib/cache/store'
import { makeLink } from '../test/fixtures'
import {
  compareReaderActivityLastAtDesc,
  normalizeReaderActivityPayload,
  useReaderActivity,
} from './useReaderActivity'

let lease: IdentityLease
let testNumber = 0

function makeClient(
  getReaderActivity?: IdentityBoundReaderClient['getReaderActivity'],
): IdentityBoundReaderClient {
  return {
    identityLease: lease,
    isIdentityCurrent: vi.fn(() => true),
    ...(getReaderActivity ? { getReaderActivity } : {}),
  } as unknown as IdentityBoundReaderClient
}

beforeEach(() => {
  testNumber += 1
  lease = new IdentityLease({
    serverClientDataNamespace: `activity-server-${testNumber}`,
    physicalNamespace: `activity-physical-${testNumber}`,
    localEpoch: testNumber,
  })
  resourceStore.activateIdentity(lease)
})

afterEach(() => {
  resourceStore.deactivateIdentity(lease)
})

describe('useReaderActivity', () => {
  it('旧后端没有 activity 能力时返回本地降级标记', () => {
    const { result } = renderHook(() => useReaderActivity(makeClient()))

    expect(result.current.source).toBe('local')
    expect(result.current.degraded).toBe(true)
    expect(result.current.tagLastAt.size).toBe(0)
    expect(result.current.domainLastAt.size).toBe(0)
  })

  it('server activity 优先提供标签和域名最近时间', async () => {
    const response: ReaderActivityResponse = {
      kind: 'all',
      tags: [
        { tag: 'server-tag', last_at: '2026-08-10T02:00:00Z' },
        { tag: 'server-tag', last_at: '2026-08-10T04:00:00Z' },
        { tag: 'same-time-z', last_at: '2026-08-10T05:00:00Z' },
        { tag: 'same-time-a', last_at: '2026-08-10T05:00:00Z' },
        { tag: 'invalid-tag', last_at: 'not-a-timestamp' },
      ],
      domains: [
        { domain: 'server.example', last_at: '2026-08-10T03:00:00Z' },
        { domain: 'empty.example', last_at: '' },
      ],
    }
    const getReaderActivity = vi.fn(async () => ok(response))
    const { result } = renderHook(() => useReaderActivity(makeClient(getReaderActivity)))

    await waitFor(() => expect(result.current.source).toBe('server'))
    expect(result.current.tagLastAt.get('server-tag')).toBe('2026-08-10T04:00:00Z')
    expect(result.current.domainLastAt.get('server.example')).toBe('2026-08-10T03:00:00Z')
    expect(result.current.tagLastAt.has('invalid-tag')).toBe(false)
    expect(result.current.domainLastAt.has('empty.example')).toBe(false)
    expect([...result.current.tagLastAt.keys()]).toEqual(['same-time-a', 'same-time-z', 'server-tag'])
    expect(result.current.degraded).toBe(false)
    expect(getReaderActivity).toHaveBeenCalledWith(100, { kind: 'all' })
  })

  it('旧后端 created_at 可转换为 lastAt，且重复事件取最新时间', () => {
    const normalized = normalizeReaderActivityPayload({
      tags: [
        { tag: 'legacy', created_at: '2026-08-10T01:00:00Z' },
        { tag: 'legacy', last_at: '2026-08-10T03:00:00Z', created_at: '2026-08-10T02:00:00Z' },
      ],
      domains: [{ domain: 'legacy.example', created_at: '2026-08-10T02:00:00Z' }],
    })

    expect(normalized).toEqual({
      tags: [
        { tag: 'legacy', last_at: '2026-08-10T01:00:00Z' },
        { tag: 'legacy', last_at: '2026-08-10T03:00:00Z' },
      ],
      domains: [{ domain: 'legacy.example', last_at: '2026-08-10T02:00:00Z' }],
    })
  })

  it('请求失败时使用传入链接的 created_at，并按时间和名称稳定排序', async () => {
    const links = [
      makeLink({ id: 'old', tags: ['zeta'], domain: 'z.example', created_at: '2026-08-10T01:00:00Z' }),
      makeLink({ id: 'new', tags: ['alpha'], domain: 'a.example', created_at: '2026-08-10T02:00:00Z' }),
    ]
    const getReaderActivity = vi.fn(async () => err<ReaderActivityResponse>({
      kind: 'other',
      status: 404,
      message: 'old backend',
    }))
    const { result } = renderHook(() => useReaderActivity(makeClient(getReaderActivity), links))

    await waitFor(() => expect(getReaderActivity).toHaveBeenCalled())
    expect([...result.current.tagLastAt.keys()]).toEqual(['alpha', 'zeta'])
    expect([...result.current.domainLastAt.keys()]).toEqual(['a.example', 'z.example'])
    expect(result.current.degraded).toBe(true)
  })

  it('本地 created_at fallback 按实际时间取最新，而不是按带时区字符串比较', () => {
    const links = [
      makeLink({ id: 'offset-older', tags: ['shared'], created_at: '2026-08-10T03:00:00+01:00' }),
      makeLink({ id: 'utc-newer', tags: ['shared'], created_at: '2026-08-10T02:30:00Z' }),
    ]
    const { result } = renderHook(() => useReaderActivity(makeClient(), links))

    expect(result.current.tagLastAt.get('shared')).toBe('2026-08-10T02:30:00Z')
  })

  it('服务端返回不可用时间戳时回到 created_at 本地投影', async () => {
    const links = [
      makeLink({ id: 'local-fallback', tags: ['local-only'], created_at: '2026-08-10T02:00:00Z' }),
    ]
    const getReaderActivity = vi.fn(async () => ok<ReaderActivityResponse>({
      kind: 'all',
      tags: [{ tag: 'local-only', last_at: 'not-a-timestamp' }],
      domains: [],
    }))
    const { result } = renderHook(() => useReaderActivity(makeClient(getReaderActivity), links))

    await waitFor(() => expect(getReaderActivity).toHaveBeenCalled())
    expect(result.current.source).toBe('local')
    expect(result.current.degraded).toBe(true)
    expect(result.current.tagLastAt.get('local-only')).toBe('2026-08-10T02:00:00Z')
  })

  it('activity 请求失败时由组件保留本地 lastAt 近似', async () => {
    const getReaderActivity = vi.fn(async () => err<ReaderActivityResponse>({
      kind: 'other',
      status: 404,
      message: 'old backend',
    }))
    const { result } = renderHook(() => useReaderActivity(makeClient(getReaderActivity)))

    await waitFor(() => expect(getReaderActivity).toHaveBeenCalled())
    expect(result.current.source).toBe('local')
    expect(result.current.degraded).toBe(true)
    expect(result.current.error?.status).toBe(404)
  })

  it('等价时区时间按相同时间处理，非法值稳定落后', () => {
    expect(compareReaderActivityLastAtDesc(
      '2026-08-10T03:00:00+01:00',
      '2026-08-10T02:00:00Z',
    )).toBe(0)
    expect(compareReaderActivityLastAtDesc('2026-08-10T02:00:00Z', 'invalid')).toBeLessThan(0)
    expect(compareReaderActivityLastAtDesc('invalid-a', 'invalid-b')).toBeLessThan(0)
  })

  it('过期显式 client 不读取活跃身份的同 namespace 缓存', async () => {
    const getActiveActivity = vi.fn(async () => ok<ReaderActivityResponse>({
      kind: 'all',
      tags: [{ tag: 'active-only', last_at: '2026-08-10T00:00:00Z' }],
      domains: [],
    }))
    const active = renderHook(() => useReaderActivity(makeClient(getActiveActivity)))
    await waitFor(() => expect(active.result.current.source).toBe('server'))
    active.unmount()

    const staleLease = new IdentityLease({
      serverClientDataNamespace: 'stale-client',
      physicalNamespace: lease.context.physicalNamespace,
      localEpoch: lease.context.localEpoch + 100,
    })
    const getStaleActivity = vi.fn(async () => ok<ReaderActivityResponse>({ kind: 'all', tags: [], domains: [] }))
    const staleClient = {
      identityLease: staleLease,
      isIdentityCurrent: vi.fn(() => true),
      getReaderActivity: getStaleActivity,
    } as unknown as IdentityBoundReaderClient
    const stale = renderHook(() => useReaderActivity(staleClient))

    expect(stale.result.current.source).toBe('local')
    expect(stale.result.current.tagLastAt.has('active-only')).toBe(false)
    expect(getStaleActivity).not.toHaveBeenCalled()
  })

  it('完整标签浏览逐页累积第 101 项并按 kind 缓存', async () => {
    const firstTags = Array.from({ length: 100 }, (_, index) => ({
      tag: `tag-${String(index).padStart(3, '0')}`,
      last_at: `2026-08-10T${String(23 - Math.floor(index / 60)).padStart(2, '0')}:${String(59 - (index % 60)).padStart(2, '0')}:00Z`,
    }))
    const getReaderActivity = vi.fn(async (
      _limit: number,
      options?: { kind?: string; after?: string },
    ) => {
      if (options?.kind !== 'tag') {
        return err<ReaderActivityResponse>({ kind: 'other', status: 500, message: 'wrong kind' })
      }
      if (!options.after) {
        return ok({ kind: 'tag' as const, tags: firstTags, domains: [], next_cursor: 'tag-cursor-1' })
      }
      return ok({
        kind: 'tag' as const,
        tags: [{ tag: 'tag-100', last_at: '2026-08-09T00:00:00Z' }],
        domains: [],
      })
    }) as IdentityBoundReaderClient['getReaderActivity']

    const { result } = renderHook(() => useReaderActivity(
      makeClient(getReaderActivity),
      [],
      { kind: 'tag', allPages: true },
    ))

    await waitFor(() => expect(result.current.tagLastAt.has('tag-100')).toBe(true))
    expect(result.current.tagLastAt.size).toBe(101)
    expect(getReaderActivity).toHaveBeenNthCalledWith(1, 100, { kind: 'tag' })
    expect(getReaderActivity).toHaveBeenNthCalledWith(2, 100, { kind: 'tag', after: 'tag-cursor-1' })
  })

  it('首屏缓存已存在时，完整消费者在同 identity/kind 条目上继续分页', async () => {
    const getReaderActivity = vi.fn(async (
      _limit: number,
      options?: { kind?: string; after?: string },
    ) => options?.after
      ? ok({
          kind: 'tag' as const,
          tags: [{ tag: 'continued', last_at: '2026-08-10T01:00:00Z' }],
          domains: [],
        })
      : ok({
          kind: 'tag' as const,
          tags: [{ tag: 'first', last_at: '2026-08-10T02:00:00Z' }],
          domains: [],
          next_cursor: 'shared-tag-cursor',
        })) as IdentityBoundReaderClient['getReaderActivity']
    const client = makeClient(getReaderActivity)
    const firstPage = renderHook(() => useReaderActivity(client, [], { kind: 'tag' }))
    await waitFor(() => expect(firstPage.result.current.tagLastAt.has('first')).toBe(true))

    const complete = renderHook(() => useReaderActivity(client, [], { kind: 'tag', allPages: true }))
    await waitFor(() => expect(complete.result.current.tagLastAt.has('continued')).toBe(true))

    expect(complete.result.current.tagLastAt.size).toBe(2)
    expect(getReaderActivity).toHaveBeenLastCalledWith(100, {
      kind: 'tag',
      after: 'shared-tag-cursor',
    })
  })

  it('tag 下一页失败不覆盖已加载页，domain 查询使用独立 cache', async () => {
    const getReaderActivity = vi.fn(async (
      _limit: number,
      options?: { kind?: string; after?: string },
    ) => {
      if (options?.kind === 'domain') {
        return ok({
          kind: 'domain' as const,
          tags: [],
          domains: [{ domain: 'domain.example', last_at: '2026-08-10T03:00:00Z' }],
        })
      }
      if (options?.after) {
        return err<ReaderActivityResponse>({ kind: 'other', status: 422, message: 'invalid cursor' })
      }
      return ok({
        kind: 'tag' as const,
        tags: [{ tag: 'retained-tag', last_at: '2026-08-10T04:00:00Z' }],
        domains: [],
        next_cursor: 'bad-on-domain',
      })
    }) as IdentityBoundReaderClient['getReaderActivity']

    const tag = renderHook(() => useReaderActivity(
      makeClient(getReaderActivity),
      [],
      { kind: 'tag', allPages: true },
    ))
    await waitFor(() => expect(tag.result.current.error?.status).toBe(422))
    expect(tag.result.current.source).toBe('server')
    expect(tag.result.current.tagLastAt.get('retained-tag')).toBe('2026-08-10T04:00:00Z')

    const domain = renderHook(() => useReaderActivity(
      makeClient(getReaderActivity),
      [],
      { kind: 'domain' },
    ))
    await waitFor(() => expect(domain.result.current.domainLastAt.has('domain.example')).toBe(true))
    expect(domain.result.current.tagLastAt.has('retained-tag')).toBe(false)
  })

  it('缺少分页 kind 标记的旧后端响应只作为本地 created_at 降级', async () => {
    const links = [makeLink({
      id: 'legacy-pagination-fallback',
      tags: ['local-authority-label'],
      created_at: '2026-08-10T05:00:00Z',
    })]
    const getReaderActivity = vi.fn(async () => ok({
      tags: [{ tag: 'legacy-server-row', last_at: '2026-08-10T06:00:00Z' }],
      domains: [],
    })) as IdentityBoundReaderClient['getReaderActivity']

    const { result } = renderHook(() => useReaderActivity(makeClient(getReaderActivity), links))

    await waitFor(() => expect(getReaderActivity).toHaveBeenCalled())
    expect(result.current.source).toBe('local')
    expect(result.current.degraded).toBe(true)
    expect(result.current.tagLastAt.has('legacy-server-row')).toBe(false)
    expect(result.current.tagLastAt.get('local-authority-label')).toBe('2026-08-10T05:00:00Z')
  })
})
