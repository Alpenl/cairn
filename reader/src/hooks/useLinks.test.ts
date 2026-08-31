import 'fake-indexeddb/auto'

import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook, waitFor } from '@testing-library/react'
import { localDayCreatedRange, useLinks, type Selection } from './useLinks'
import { linkDetailCacheKey } from './useAnnotatedLinks'
import type { IdentityBoundReaderClient, ReaderClient } from '../lib/api/client'
import type { ListLinksParams } from '../lib/api/client'
import { err, type ApiResult } from '@webtag/api'
import type { LinkResponse, PaginatedLinksResponse } from '../lib/api/types'
import { ownedDatabaseName } from '../lib/storage-ownership'
import { invalidateLibrary } from '../lib/cache/invalidate'
import { resourceStore } from '../lib/cache/store'
import { makeLink } from '../test/fixtures'
import { IdentityLease, readerIdentity } from '../lib/identity'
import { commitAnnotationOperation } from '../lib/user-data/annotation-store'
import type { SavedContentAnnotationAddDraft } from '../lib/user-data/annotation-types'
import { resetUserDataDatabaseHandle } from '../lib/user-data/idb'

function bindClient(
  client: ReaderClient,
  lease = readerIdentity.activeLease!,
): IdentityBoundReaderClient {
  const current = (client as { isIdentityCurrent?: () => boolean }).isIdentityCurrent
  Object.defineProperty(client, 'identityLease', {
    configurable: true,
    value: lease,
  })
  if (!current) {
    Object.defineProperty(client, 'isIdentityCurrent', {
      configurable: true,
      value: () => true,
    })
  }
  Object.defineProperty(client, 'captureIdentity', {
    configurable: true,
    value: (logicalKey: string) => {
      if (current && !current.call(client)) return null
      const ownership = lease.captureOwnership(logicalKey)
      return lease.isOwnershipCurrent(ownership) ? ownership : null
    },
  })
  return client as IdentityBoundReaderClient
}

function useTestLinks(client: ReaderClient, selection: Selection) {
  const lease = readerIdentity.activeLease
  if (!lease) throw new Error('test identity lease is not active')
  return useLinks(bindClient(client, lease), selection)
}

function makeReadingLink(over: Partial<LinkResponse> = {}): LinkResponse {
  return makeLink({ library_kind: 'reading', ...over })
}

/**
 * 构造一页响应。
 *
 * 后端契约（dto.PaginatedLinksResponse）：游标模式下满页**必发** next_cursor，
 * 短于 limit 则省略；total / page 恒为 0（游标模式刻意不算 COUNT）。
 * fixture 必须照这个来——否则「还有没有下一页」在测试里永远是 false，
 * 一整类翻页用例会在"什么都没发生"的前提下通过。
 */
function page(items = [makeLink()], limit = 30): ApiResult<PaginatedLinksResponse> {
  const data: PaginatedLinksResponse = { items, total: 0, page: 0, limit }
  if (items.length >= limit) data.next_cursor = 'cursor-' + items.length
  return { ok: true, data }
}

function legacyPage(items: LinkResponse[], total: number, limit = 30): ApiResult<PaginatedLinksResponse> {
  return { ok: true, data: { items, total, page: 1, limit } }
}

/** 构造仅实现 getLinks 的假客户端，记录调用参数。 */
function fakeClient(
  impl: (p: ListLinksParams) => Promise<ApiResult<PaginatedLinksResponse>>,
  isIdentityCurrent: () => boolean = () => true,
) {
  const calls: ListLinksParams[] = []
  const client = {
    getLinks: vi.fn(async (p: ListLinksParams = {}) => {
      calls.push(p)
      return impl(p)
    }),
    isIdentityCurrent: vi.fn(isIdentityCurrent),
  } as unknown as ReaderClient
  return { client, calls }
}

function annotationDraft(id: string): SavedContentAnnotationAddDraft {
  return {
    id,
    blockKey: 'content',
    start: 0,
    end: 4,
    text: 'text',
    note: '',
    source: 'self',
    createdAt: 100,
    updatedAt: 100,
  }
}

async function indexAnnotatedLink(linkId: string): Promise<void> {
  const result = await commitAnnotationOperation(readerIdentity.activeLease!, {
    kind: 'add',
    opId: `op-${linkId}`,
    linkId,
    target: { kind: 'saved-content', contentRevision: 1 },
    draft: annotationDraft(`annotation-${linkId}`),
  })
  expect(result).toMatchObject({ ok: true, value: { status: 'committed' } })
}

afterEach(async () => {
  resetUserDataDatabaseHandle()
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(ownedDatabaseName('userDataDatabase'))
    request.onsuccess = () => resolve()
    request.onerror = () => reject(
      request.error ?? new Error('Failed to delete the user-data test database'),
    )
    request.onblocked = () => reject(new Error('User-data test database deletion was blocked'))
  })
})

describe('localDayCreatedRange', () => {
  const cases = [
    {
      name: 'UTC+8 natural day',
      timezone: 'Asia/Shanghai',
      now: '2026-03-08T12:00:00Z',
      from: '2026-03-07T16:00:00.000Z',
      before: '2026-03-08T16:00:00.000Z',
      hours: 24,
    },
    {
      name: 'UTC-7 natural day',
      timezone: 'America/Phoenix',
      now: '2026-03-08T12:00:00Z',
      from: '2026-03-08T07:00:00.000Z',
      before: '2026-03-09T07:00:00.000Z',
      hours: 24,
    },
    {
      name: 'DST spring-forward day',
      timezone: 'America/Los_Angeles',
      now: '2026-03-08T12:00:00Z',
      from: '2026-03-08T08:00:00.000Z',
      before: '2026-03-09T07:00:00.000Z',
      hours: 23,
    },
    {
      name: 'DST fall-back day',
      timezone: 'America/Los_Angeles',
      now: '2026-11-01T12:00:00Z',
      from: '2026-11-01T07:00:00.000Z',
      before: '2026-11-02T08:00:00.000Z',
      hours: 25,
    },
  ]

  it.each(cases)('maps $name through adjacent browser-local midnights', ({
    timezone,
    now,
    from,
    before,
    hours,
  }) => {
    const originalTimezone = process.env.TZ
    try {
      process.env.TZ = timezone
      const range = localDayCreatedRange(new Date(now))
      expect(range).toEqual({ created_from: from, created_before: before })
      expect((Date.parse(range.created_before) - Date.parse(range.created_from)) / 3_600_000)
        .toBe(hours)
    } finally {
      if (originalTimezone === undefined) delete process.env.TZ
      else process.env.TZ = originalTimezone
    }
  })
})

describe('useLinks 视图→参数映射', () => {
  it('all 视图显式限定已完成的 reading 链接', async () => {
    const { client, calls } = fakeClient(async () => page())
    renderHook(() => useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }))
    await waitFor(() => expect(calls.length).toBeGreaterThan(0))

    expect(calls[0]).toEqual({
      library_kind: 'reading',
      status: 'done',
      limit: 30,
      after: '',
    })
  })

  it('tag 视图带 tags 参数', async () => {
    const { client, calls } = fakeClient(async () => page())
    const sel: Selection = { type: 'tag', id: 'LLM', name: '#LLM' }
    renderHook(() => useTestLinks(client, sel))
    await waitFor(() => expect(calls.length).toBeGreaterThan(0))
    expect(calls[0]).toEqual({
      library_kind: 'reading',
      status: 'done',
      limit: 30,
      after: '',
      tags: 'LLM',
    })
  })

  it('domain 视图带 domain 参数', async () => {
    const { client, calls } = fakeClient(async () => page())
    renderHook(() => useTestLinks(client, { type: 'domain', id: 'x.com', name: 'x.com' }))
    await waitFor(() => expect(calls.length).toBeGreaterThan(0))
    expect(calls[0]).toEqual({
      library_kind: 'reading',
      status: 'done',
      limit: 30,
      after: '',
      domain: 'x.com',
    })
  })

  it('today 视图把浏览器本地自然日交给服务端过滤', async () => {
    const originalTimezone = process.env.TZ
    process.env.TZ = 'Asia/Shanghai'
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-03-08T12:00:00Z'))
    try {
      const serverItems = [
        makeLink({ id: 'server-authoritative-1' }),
        makeLink({ id: 'server-authoritative-2' }),
      ]
      const { client, calls } = fakeClient(async () => page(serverItems))
      const { result } = renderHook(() =>
        useTestLinks(client, { type: 'smart', id: 'today', name: '今天' }),
      )
      await act(async () => { await Promise.resolve() })

      expect(calls[0]).toEqual({
        library_kind: 'reading',
        status: 'done',
        created_from: '2026-03-07T16:00:00.000Z',
        created_before: '2026-03-08T16:00:00.000Z',
        limit: 30,
        after: '',
      })
      expect(result.current.links.map((link) => link.id).sort()).toEqual([
        'server-authoritative-1',
        'server-authoritative-2',
      ])
    } finally {
      vi.useRealTimers()
      if (originalTimezone === undefined) delete process.env.TZ
      else process.env.TZ = originalTimezone
    }
  })

  it('annotated 视图枚举 durable 全量索引后只保留完成的 Reading 链接', async () => {
    const indexed = makeReadingLink({ id: 'indexed-reading-link' })
    const site = makeLink({ id: 'indexed-site-link', library_kind: 'site' })
    const pending = makeReadingLink({ id: 'indexed-pending-link', status: 'pending' })
    for (const link of [indexed, site, pending]) await indexAnnotatedLink(link.id)
    const getLinks = vi.fn(async () => page([makeLink({ id: 'latest-but-unannotated' })]))
    const byID = new Map([indexed, site, pending].map((link) => [link.id, link]))
    const getLink = vi.fn(async (id: string) => ({ ok: true as const, data: byID.get(id)! }))
    const client = { getLinks, getLink } as unknown as ReaderClient
    const selection: Selection = { type: 'smart', id: 'annotated', name: '有划线' }
    const { result } = renderHook(() => useTestLinks(client, selection))

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.links.map((link) => link.id)).toEqual([indexed.id])
    expect(getLink).toHaveBeenCalledWith(indexed.id, expect.objectContaining({
      signal: expect.any(AbortSignal),
    }))
    expect(getLinks).not.toHaveBeenCalled()
  })

  it('错误透出 error 且清空列表', async () => {
    const { client } = fakeClient(async () => ({ ok: false, error: { kind: 'unauthorized', message: '401' } }))
    const { result } = renderHook(() => useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }))
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.error?.kind).toBe('unauthorized')
    expect(result.current.links).toEqual([])
  })
})

describe('useLinks Reading corpus authority', () => {
  it('保留旧后端 scoped links 的权威 total，但部分 corpus 不标为完整', async () => {
    const { client } = fakeClient(async () =>
      legacyPage([makeLink({ id: 'loaded-1' })], 2),
    )
    const { result } = renderHook(() =>
      useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }),
    )

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.authoritativeTotal).toBe(2)
    expect(result.current.corpusComplete).toBe(false)
  })

  it('只有分页结束且去重条数等于权威 total 才标记完整', async () => {
    const items = [makeLink({ id: 'reading-1' }), makeLink({ id: 'reading-2' })]
    const { client } = fakeClient(async () => legacyPage(items, 2))
    const { result } = renderHook(() =>
      useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }),
    )

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.authoritativeTotal).toBe(2)
    expect(result.current.corpusComplete).toBe(true)
  })

  it('cursor 响应的占位 total=0 不冒充权威总数', async () => {
    const { client } = fakeClient(async () => page([makeLink({ id: 'cursor-item' })]))
    const { result } = renderHook(() =>
      useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }),
    )

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.authoritativeTotal).toBeNull()
    expect(result.current.corpusComplete).toBe(false)
  })
})

describe('useLinks 竞态守卫', () => {
  it('A client 在 B cache partition 激活时不请求也不写入', async () => {
    const leaseB = readerIdentity.activeLease!
    const leaseA = new IdentityLease({
      serverClientDataNamespace: 'server-A',
      physicalNamespace: 'physical-A',
      localEpoch: leaseB.context.localEpoch + 1,
    })
    const getLinks = vi.fn(async () => page([makeLink({ id: 'A-private-link' })]))
    const clientA = bindClient({ getLinks } as unknown as ReaderClient, leaseA)
    const writes: string[] = []
    const stopObserving = resourceStore.onChange((key) => writes.push(key))

    type UseLinksArguments = Parameters<typeof useLinks>
    // @ts-expect-error useLinks has no independently supplied lease argument
    const rejectedLeaseParameter: UseLinksArguments[2] = leaseB
    expect(rejectedLeaseParameter).toBe(leaseB)

    const { result } = renderHook(() =>
      useLinks(clientA, { type: 'smart', id: 'all', name: '全部' }),
    )
    await waitFor(() => expect(result.current.error?.kind).toBe('identity-mismatch'))

    expect(result.current.loading).toBe(false)
    expect(getLinks).not.toHaveBeenCalled()
    expect(writes).toEqual([])
    stopObserving()
    leaseA.revoke()
  })

  it('旧请求的迟到响应不覆盖新请求结果', async () => {
    let resolveSlow: (v: ApiResult<PaginatedLinksResponse>) => void = () => {}
    let n = 0
    const { client } = fakeClient((p) => {
      n += 1
      if (n === 1) {
        // 第一次（all）：慢，手动 resolve
        return new Promise<ApiResult<PaginatedLinksResponse>>((res) => {
          resolveSlow = res
        })
      }
      // 第二次（tag）：快
      return Promise.resolve(page([makeLink({ id: 'fast', tags: [p.tags || 'x'] })]))
    })

    const { result, rerender } = renderHook(({ sel }: { sel: Selection }) => useTestLinks(client, sel), {
      initialProps: { sel: { type: 'smart', id: 'all', name: '全部' } as Selection },
    })

    // 切到 tag 视图（触发第二次快请求）
    rerender({ sel: { type: 'tag', id: 'LLM', name: '#LLM' } })
    await waitFor(() => expect(result.current.links.some((l) => l.id === 'fast')).toBe(true))

    // 现在让第一次慢请求迟到 resolve —— 不应覆盖
    act(() => resolveSlow(page([makeLink({ id: 'stale' })])))
    await Promise.resolve()
    expect(result.current.links.some((l) => l.id === 'stale')).toBe(false)
    expect(result.current.links.some((l) => l.id === 'fast')).toBe(true)
  })

  it('does not append an A-era next page after the identity lease is replaced', async () => {
    const leaseA = readerIdentity.activeLease!
    const ownershipA = leaseA.capture('links pagination test')
    const full = Array.from({ length: 30 }, (_, index) => makeLink({ id: `A-${index}` }))
    let resolvePage!: (value: ApiResult<PaginatedLinksResponse>) => void
    const delayedPage = new Promise<ApiResult<PaginatedLinksResponse>>((resolve) => {
      resolvePage = resolve
    })
    let requestCount = 0
    const { client } = fakeClient(
      () => {
        requestCount += 1
        return requestCount === 1 ? Promise.resolve(page(full)) : delayedPage
      },
      () => leaseA.isCurrent(ownershipA),
    )
    const { result } = renderHook(() =>
      useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }),
    )
    await waitFor(() => expect(result.current.links).toHaveLength(30))
    act(() => result.current.loadMore())
    await waitFor(() => expect(requestCount).toBe(2))

    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    await act(async () => {
      resolvePage(page([makeLink({ id: 'A-late-page' })]))
      await delayedPage
    })

    expect(result.current.links.some((link) => link.id === 'A-late-page')).toBe(false)
  })

  it('fences a same-filter load-more page when reload starts a new pagination epoch', async () => {
    const oldFirst = Array.from({ length: 30 }, (_, index) => makeLink({
      id: `old-first-${index}`,
      created_at: `2026-08-10T00:${String(index).padStart(2, '0')}:00Z`,
    }))
    const newFirst = Array.from({ length: 30 }, (_, index) => makeLink({
      id: `new-first-${index}`,
      created_at: `2026-08-11T00:${String(index).padStart(2, '0')}:00Z`,
    }))
    let resolveOldPage!: (value: ApiResult<PaginatedLinksResponse>) => void
    const oldPage = new Promise<ApiResult<PaginatedLinksResponse>>((resolve) => {
      resolveOldPage = resolve
    })
    const calls: ListLinksParams[] = []
    const client = {
      getLinks: vi.fn((params: ListLinksParams = {}) => {
        calls.push(params)
        if (calls.length === 1) return Promise.resolve({
          ok: true as const,
          data: { items: oldFirst, total: 0, page: 0, limit: 30, next_cursor: 'old-cursor' },
        })
        if (params.after === 'old-cursor') return oldPage
        if (params.after === '') return Promise.resolve({
          ok: true as const,
          data: { items: newFirst, total: 0, page: 0, limit: 30, next_cursor: 'new-cursor' },
        })
        return Promise.resolve({
          ok: true as const,
          data: { items: [makeLink({ id: 'new-second' })], total: 0, page: 0, limit: 30 },
        })
      }),
    } as unknown as ReaderClient
    const { result } = renderHook(() => useTestLinks(client, {
      type: 'smart', id: 'all', name: '全部',
    }))
    await waitFor(() => expect(result.current.links).toHaveLength(30))

    act(() => result.current.loadMore())
    await waitFor(() => expect(calls.some((params) => params.after === 'old-cursor')).toBe(true))
    await act(async () => { await result.current.reload() })
    expect(result.current.links.map((link) => link.id)).toEqual(newFirst.map((link) => link.id).reverse())

    await act(async () => {
      resolveOldPage({
        ok: true,
        data: { items: [makeLink({ id: 'old-late-page' })], total: 0, page: 0, limit: 30 },
      })
      await oldPage
    })
    expect(result.current.links.some((link) => link.id === 'old-late-page')).toBe(false)
    expect(result.current.loadingMore).toBe(false)

    act(() => result.current.loadMore())
    await waitFor(() => expect(calls.some((params) => params.after === 'new-cursor')).toBe(true))
    await waitFor(() => expect(result.current.links.some((link) => link.id === 'new-second')).toBe(true))
  })

  it('does not revive an old cursor after the replacement first page fails', async () => {
    const oldFirst = Array.from({ length: 30 }, (_, index) => makeLink({ id: `old-${index}` }))
    let resolveOldPage!: (value: ApiResult<PaginatedLinksResponse>) => void
    const oldPage = new Promise<ApiResult<PaginatedLinksResponse>>((resolve) => {
      resolveOldPage = resolve
    })
    let calls = 0
    const client = {
      getLinks: vi.fn((params: ListLinksParams = {}) => {
        calls += 1
        if (calls === 1) return Promise.resolve({
          ok: true as const,
          data: { items: oldFirst, total: 0, page: 0, limit: 30, next_cursor: 'old-cursor' },
        })
        if (params.after === 'old-cursor') return oldPage
        return Promise.resolve(err({ kind: 'network-unreachable', message: 'reload failed' }))
      }),
    } as unknown as ReaderClient
    const { result } = renderHook(() => useTestLinks(client, {
      type: 'smart', id: 'all', name: '全部',
    }))
    await waitFor(() => expect(result.current.links).toHaveLength(30))
    act(() => result.current.loadMore())
    await waitFor(() => expect(calls).toBe(2))
    await act(async () => { await result.current.reload() })
    expect(result.current.links).toEqual([])
    expect(result.current.hasMore).toBe(false)

    await act(async () => {
      resolveOldPage(page([makeLink({ id: 'old-late' })]))
      await oldPage
    })
    act(() => result.current.loadMore())
    expect(calls).toBe(3)
    expect(result.current.links).toEqual([])
    expect(result.current.hasMore).toBe(false)
  })
})

// 采集 → 出现在库里，是收藏类产品最基本的那条闭环。此前它断在这里：
// 列表不轮询，后端也没有推送通道，用户在扩展里采集完切到 Reader 什么都没有，
// 必须手动点同步。
describe('useLinks 自动刷新', () => {
  it('停留在第 1 页时按间隔静默重拉', async () => {
    vi.useFakeTimers()
    try {
      const { client, calls } = fakeClient(async () => page())
      renderHook(() => useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }))
      await act(async () => {
        await Promise.resolve()
      })
      const initial = calls.length
      expect(initial).toBeGreaterThan(0)

      await act(async () => {
        vi.advanceTimersByTime(30_000)
        await Promise.resolve()
      })
      expect(calls.length).toBeGreaterThan(initial)
    } finally {
      vi.useRealTimers()
    }
  })

  // 翻到第 N 页的人不该被定时器拽回顶部。
  it('已翻页时不自动刷新', async () => {
    vi.useFakeTimers()
    try {
      // 满页才会有 hasMore，loadMore 才真的推进到第 2 页——否则这条测试
      // 会在「page 始终是 1」的前提下通过，什么也没验到。
      const full = Array.from({ length: 30 }, (_, i) => makeLink({ id: `l${i}` }))
      const { client, calls } = fakeClient(async () => page(full))
      const { result } = renderHook(() => useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }))
      await act(async () => {
        await Promise.resolve()
      })
      act(() => result.current.loadMore())
      await act(async () => {
        await Promise.resolve()
      })
      const afterLoadMore = calls.length

      await act(async () => {
        vi.advanceTimersByTime(30_000 * 3)
        await Promise.resolve()
      })
      expect(calls.length).toBe(afterLoadMore)
    } finally {
      vi.useRealTimers()
    }
  })

  // 后台标签页不该继续打后端。
  it('页面不可见时跳过这一轮', async () => {
    vi.useFakeTimers()
    const spy = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden')
    try {
      const { client, calls } = fakeClient(async () => page())
      renderHook(() => useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }))
      await act(async () => {
        await Promise.resolve()
      })
      const initial = calls.length

      await act(async () => {
        vi.advanceTimersByTime(30_000 * 2)
        await Promise.resolve()
      })
      expect(calls.length).toBe(initial)
    } finally {
      spy.mockRestore()
      vi.useRealTimers()
    }
  })
})

// 自动刷新的两条行为约束。只数请求次数是不够的——上面三条即使在「每 30 秒
// 把列表塌成正在加载、一次网络抖动就清空列表」的实现下也全绿。
describe('useLinks 自动刷新不打断阅读', () => {
  it('后台刷新在途时不切 loading（列表不塌成「正在加载…」）', async () => {
    vi.useFakeTimers()
    try {
      let release: ((v: ApiResult<PaginatedLinksResponse>) => void) | null = null
      let calls = 0
      const client = {
        getLinks: vi.fn(async () => {
          calls += 1
          if (calls === 1) return page()
          // 第二次（定时器触发的那次）挂住，好在「在途」这一刻检查 loading。
          return new Promise<ApiResult<PaginatedLinksResponse>>((r) => {
            release = r
          })
        }),
      } as unknown as ReaderClient

      const { result } = renderHook(() =>
        useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }),
      )
      await act(async () => {
        await Promise.resolve()
      })
      expect(result.current.loading).toBe(false)

      await act(async () => {
        vi.advanceTimersByTime(30_000)
        await Promise.resolve()
      })
      // 请求已经发出但还没回来——此刻 loading 必须仍是 false。
      expect(calls).toBe(2)
      expect(result.current.loading).toBe(false)

      await act(async () => {
        release?.(page())
        await Promise.resolve()
      })
    } finally {
      vi.useRealTimers()
    }
  })

  it('后台刷新失败时保留已有列表，不制造用户没触发过的错误界面', async () => {
    vi.useFakeTimers()
    try {
      let calls = 0
      const client = {
        getLinks: vi.fn(async () => {
          calls += 1
          if (calls === 1) return page([makeLink({ id: 'kept' })])
          return {
            ok: false as const,
            error: { kind: 'network-unreachable' as const, message: 'offline' },
          }
        }),
      } as unknown as ReaderClient

      const { result } = renderHook(() =>
        useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }),
      )
      await act(async () => {
        await Promise.resolve()
      })
      expect(result.current.links.map((l) => l.id)).toEqual(['kept'])

      await act(async () => {
        vi.advanceTimersByTime(30_000)
        await Promise.resolve()
      })
      expect(result.current.links.map((l) => l.id)).toEqual(['kept'])
      expect(result.current.error).toBeNull()
    } finally {
      vi.useRealTimers()
    }
  })

  // 用户主动触发的失败仍要显式报错——silent 只对定时器生效。
  it('用户主动 reload 失败仍然清空并报错', async () => {
    let calls = 0
    const client = {
      getLinks: vi.fn(async () => {
        calls += 1
        if (calls === 1) return page([makeLink({ id: 'kept' })])
        return {
          ok: false as const,
          error: { kind: 'network-unreachable' as const, message: 'offline' },
        }
      }),
    } as unknown as ReaderClient

    const { result } = renderHook(() =>
      useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))

    act(() => { void result.current.reload() })
    await waitFor(() => expect(result.current.error).not.toBeNull())
    expect(result.current.links).toEqual([])
  })
})

// S1 的回归防线：silent 请求不得让界面卡在「无数据、无错误、无重试入口」的
// 死态。
//
// 原始复现路径依赖 seqRef 是 silent 与非 silent **共用的一个计数器**：用户点
// 同步（慢）→ 定时器 tick 抢占序号 → tick 失败早退且不清 loading → 用户那次
// 响应回来时序号已过期被丢弃 → 永久加载态。
//
// PF3 之后那条路径在结构上不存在了：同一个键的并发请求由 store 合并成**一次**
// 往返（不再有两个请求互相抢序号），且 loading 是从「有没有数据」派生的，
// 不是一个可以被落下的标志位。因此这里改为断言最终不变量本身——无论 silent 与
// 用户请求如何交错，界面都不能停在无数据且无错误的状态。
describe('useLinks silent 请求不得泄漏 loading', () => {
  it('silent 与用户请求交错之后，界面不会停在无数据且无错误的死态', async () => {
    let calls = 0
    const client = {
      getLinks: vi.fn(async () => {
        calls += 1
        if (calls === 1) return page([makeLink({ id: 'initial' })])
        if (calls === 2) {
          return {
            ok: false as const,
            error: { kind: 'network-unreachable' as const, message: 'offline' },
          }
        }
        return page([makeLink({ id: 'initial' })])
      }),
    } as unknown as ReaderClient

    const { result } = renderHook(() =>
      useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.links.map((l) => l.id)).toEqual(['initial'])

    // 用户主动 reload 与 silent tick 交错发生。
    act(() => { void result.current.reload() })
    await act(async () => {
      document.dispatchEvent(new Event('visibilitychange'))
      await Promise.resolve()
      await Promise.resolve()
    })

    await waitFor(() => expect(result.current.loading).toBe(false))
    // 死态判据：既没有内容、也没有错误 —— 用户既看不到东西又没有重试入口。
    const stuck =
      result.current.links.length === 0 &&
      result.current.error === null &&
      result.current.loading === false
    expect(stuck).toBe(false)
  })

  it('已有缓存时用户重取不闪加载态（SWR：先渲染旧的）', async () => {
    let calls = 0
    let release: ((v: ApiResult<PaginatedLinksResponse>) => void) | null = null
    const client = {
      getLinks: vi.fn(async () => {
        calls += 1
        if (calls === 1) return page([makeLink({ id: 'cached' })])
        return new Promise<ApiResult<PaginatedLinksResponse>>((r) => {
          release = r
        })
      }),
    } as unknown as ReaderClient

    const { result } = renderHook(() =>
      useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }),
    )
    await waitFor(() => expect(result.current.loading).toBe(false))

    act(() => { void result.current.reload() })
    await act(async () => {
      await Promise.resolve()
    })

    // 重取在途，但已有内容 —— 不该塌成加载态，用户的滚动位置与阅读不被打断。
    expect(result.current.loading).toBe(false)
    expect(result.current.links.map((l) => l.id)).toEqual(['cached'])

    await act(async () => {
      release?.(page([makeLink({ id: 'cached' })]))
      await Promise.resolve()
    })
    expect(result.current.loading).toBe(false)
  })
})

// 后台刷新成功必须清掉上一次的错误，否则新数据到不了界面：ListPane 的列表
// 渲染条件是 `!loading && !error`，error 非空时一条都不渲染。
it('后台刷新成功后清掉上一次的错误，新数据得以显示', async () => {
  let calls = 0
  const client = {
    getLinks: vi.fn(async () => {
      calls += 1
      if (calls === 1) return page([makeLink({ id: 'first' })])
      if (calls === 2) {
        return {
          ok: false as const,
          error: { kind: 'network-unreachable' as const, message: 'offline' },
        }
      }
      return page([makeLink({ id: 'fresh' })])
    }),
  } as unknown as ReaderClient

  const { result } = renderHook(() =>
    useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }),
  )
  await waitFor(() => expect(result.current.loading).toBe(false))

  // 用户主动刷新失败 → 显式错误态。
  act(() => { void result.current.reload() })
  await waitFor(() => expect(result.current.error).not.toBeNull())

  // 后台刷新成功。
  await act(async () => {
    document.dispatchEvent(new Event('visibilitychange'))
    await Promise.resolve()
    await Promise.resolve()
  })

  expect(result.current.error).toBeNull()
  expect(result.current.links.map((l) => l.id)).toEqual(['fresh'])
})

// PF8：列表改走游标分页（?after=），后端据此跳过 COUNT(*) OVER() 与 OFFSET 扫描。
describe('useLinks 游标分页', () => {
  it('首屏发 ?after= 进入游标模式，不再发 page=', async () => {
    const { client, calls } = fakeClient(async () => page())
    renderHook(() => useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }))
    await waitFor(() => expect(calls.length).toBeGreaterThan(0))

    // after 存在（哪怕是空串）就是游标模式的开关，后端据此分流。
    expect(calls[0].after).toBe('')
    expect(calls[0].page).toBeUndefined()
  })

  it('loadMore 带上一次响应的 next_cursor 续读', async () => {
    const full = Array.from({ length: 30 }, (_, i) => makeLink({ id: `l${i}` }))
    const { client, calls } = fakeClient(async () => page(full))
    const { result } = renderHook(() => useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }))
    await waitFor(() => expect(result.current.links).toHaveLength(30))
    expect(result.current.hasMore).toBe(true)

    act(() => result.current.loadMore())
    await waitFor(() => expect(calls.length).toBeGreaterThan(1))

    expect(calls[calls.length - 1]).toEqual({
      library_kind: 'reading',
      status: 'done',
      limit: 30,
      after: 'cursor-30',
    })
  })

  it('响应不带 next_cursor 即读到末尾，hasMore 归 false', async () => {
    const short = Array.from({ length: 5 }, (_, i) => makeLink({ id: `s${i}` }))
    const { client } = fakeClient(async () => page(short))
    const { result } = renderHook(() => useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }))
    await waitFor(() => expect(result.current.links).toHaveLength(5))

    expect(result.current.hasMore).toBe(false)
  })

  it('today 用同一冻结范围游标读取 101 条', async () => {
    const items = Array.from({ length: 101 }, (_, index) =>
      makeLink({ id: `today-${String(index).padStart(3, '0')}` }),
    )
    const starts = new Map<string, number>([['', 0], ['cursor-30', 30], ['cursor-60', 60], ['cursor-90', 90]])
    const { client, calls } = fakeClient(async (params) => {
      const start = starts.get(params.after ?? '')
      if (start === undefined) throw new Error(`unexpected cursor ${params.after}`)
      const pageItems = items.slice(start, start + 30)
      return {
        ok: true,
        data: {
          items: pageItems,
          total: 0,
          page: 0,
          limit: 30,
          ...(start + pageItems.length < items.length
            ? { next_cursor: `cursor-${start + pageItems.length}` }
            : {}),
        },
      }
    })
    const { result } = renderHook(() =>
      useTestLinks(client, { type: 'smart', id: 'today', name: '今天' }),
    )

    for (const expected of [30, 60, 90, 101]) {
      await waitFor(() => expect(result.current.links).toHaveLength(expected))
      if (expected < 101) act(() => result.current.loadMore())
    }

    expect(new Set(result.current.links.map((link) => link.id)).size).toBe(101)
    expect(result.current.hasMore).toBe(false)
    expect(calls).toHaveLength(4)
    const firstRange = {
      created_from: calls[0].created_from,
      created_before: calls[0].created_before,
    }
    for (const call of calls) {
      expect(call).toMatchObject({
        library_kind: 'reading',
        status: 'done',
        limit: 30,
        ...firstRange,
      })
    }
    expect(calls.map((call) => call.after)).toEqual(['', 'cursor-30', 'cursor-60', 'cursor-90'])
  })

  it('本地午夜更换 Today 缓存流与请求边界', async () => {
    const originalTimezone = process.env.TZ
    process.env.TZ = 'Asia/Shanghai'
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-03-08T15:59:59.500Z'))
    try {
      let request = 0
      const { client, calls } = fakeClient(async () => {
        request += 1
        return page([makeLink({ id: `day-${request}` })])
      })
      const { result } = renderHook(() =>
        useTestLinks(client, { type: 'smart', id: 'today', name: '今天' }),
      )
      await act(async () => { await Promise.resolve() })
      expect(result.current.links.map((link) => link.id)).toEqual(['day-1'])

      await act(async () => {
        vi.advanceTimersByTime(502)
        await Promise.resolve()
        await Promise.resolve()
      })

      expect(calls).toHaveLength(2)
      expect(calls[0]).toMatchObject({
        created_from: '2026-03-07T16:00:00.000Z',
        created_before: '2026-03-08T16:00:00.000Z',
      })
      expect(calls[1]).toMatchObject({
        created_from: '2026-03-08T16:00:00.000Z',
        created_before: '2026-03-09T16:00:00.000Z',
      })
      expect(result.current.links.map((link) => link.id)).toEqual(['day-2'])
    } finally {
      vi.useRealTimers()
      if (originalTimezone === undefined) delete process.env.TZ
      else process.env.TZ = originalTimezone
    }
  })
})

// 两条回归防线，来自 PF3 审查实测复现的问题。
describe('useLinks 缓存键与乐观补丁的回归防线', () => {
  it('普通列表填充的 point cache 会在 annotated 视图直接复用且不重复点读', async () => {
    const annotated = makeReadingLink({
      id: 'ann',
      title: 'cached title',
      created_at: new Date().toISOString(),
    })
    const plain = makeLink({ id: 'plain', created_at: new Date().toISOString() })
    await indexAnnotatedLink(annotated.id)
    const getLinks = vi.fn(async () => page([annotated, plain]))
    const getLink = vi.fn(async (): Promise<ApiResult<LinkResponse>> => ({
      ok: true,
      data: makeLink({ ...annotated, title: 'Unexpected point-read title' }),
    }))
    const client = { getLinks, getLink } as unknown as ReaderClient

    const { result, rerender } = renderHook(
      ({ sel }: { sel: Selection }) => useTestLinks(client, sel),
      { initialProps: { sel: { type: 'smart', id: 'today', name: '今天' } as Selection } },
    )
    await waitFor(() => expect(result.current.links.length).toBeGreaterThan(0))
    // Today trusts the server-side natural-day range; both returned rows remain.
    expect(result.current.links.map((l) => l.id).sort()).toEqual(['ann', 'plain'])

    rerender({ sel: { type: 'smart', id: 'annotated', name: '有划线' } as Selection })
    await waitFor(() => expect(result.current.links).toEqual([annotated]))
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.links).toEqual([annotated])
    expect(getLink).not.toHaveBeenCalled()
  })

  it('服务端给出新数据之后，乐观补丁不再压住真值', async () => {
    let phase = 0
    const client = {
      getLinks: vi.fn(async () => {
        phase += 1
        return page([makeLink({ id: 'L1', status: phase === 1 ? 'pending' : 'done' })])
      }),
    } as unknown as ReaderClient

    const { result } = renderHook(() =>
      useTestLinks(client, { type: 'smart', id: 'all', name: '全部' }),
    )
    await waitFor(() => expect(result.current.links).toHaveLength(1))

    // 乐观地标成 pending（「重新解析」就是这么做的）。
    act(() => result.current.patchLink('L1', { status: 'pending' }))
    expect(result.current.links[0].status).toBe('pending')

    // 服务端重取拿回 done —— 补丁必须让位，否则界面永久停在「解析中」。
    act(() => { void result.current.reload() })
    await waitFor(() => expect(result.current.links[0].status).toBe('done'))
  })

  it('cached-only annotated reload 保留补丁，失效后的非完成态真值会清除补丁并退出视图', async () => {
    const first = makeReadingLink({
      id: 'annotated-authority',
      title: 'Initial authoritative title',
      status: 'done',
    })
    const refreshed = makeLink({
      ...first,
      title: 'Refreshed authoritative title',
      status: 'failed',
    })
    await indexAnnotatedLink(first.id)
    const getLink = vi.fn()
      .mockResolvedValueOnce({ ok: true as const, data: first })
      .mockResolvedValueOnce({ ok: true as const, data: refreshed })
    const client = { getLink } as unknown as ReaderClient
    const selection: Selection = { type: 'smart', id: 'annotated', name: '有划线' }
    const { result } = renderHook(() => useTestLinks(client, selection))
    await waitFor(() => expect(result.current.links).toEqual([first]))

    act(() => result.current.patchLink(first.id, {
      title: 'Optimistic stale title',
      status: 'pending',
    }))
    expect(result.current.links[0]).toMatchObject({
      title: 'Optimistic stale title',
      status: 'pending',
    })

    act(() => { void result.current.reload() })
    await waitFor(() => expect(result.current.loading).toBe(true))
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(getLink).toHaveBeenCalledTimes(1)
    expect(result.current.links[0]).toMatchObject({
      title: 'Optimistic stale title',
      status: 'pending',
    })

    act(() => invalidateLibrary())
    await waitFor(() => expect(getLink).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(result.current.links).toEqual([]))
    expect(resourceStore.peek(linkDetailCacheKey(first.id)).data).toMatchObject({
      title: 'Refreshed authoritative title',
      status: 'failed',
    })
  })
})
