import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import { localFilterLinks, useCommandSearch } from './useCommandSearch'
import type { ReaderClient } from '../lib/api/client'
import type { ApiResult } from '../lib/api/result'
import type { GroupedSearchResponse } from '../lib/api/types'
import { makeLink } from '../test/fixtures'
import { readerIdentity } from '../lib/identity'

function grouped(items = [makeLink()]): ApiResult<GroupedSearchResponse> {
  return { ok: true, data: { reading: { items, total_hint: items.length }, sites: { items: [], total_hint: 0 } } }
}

function fakeClient(
  impl: () => Promise<ApiResult<GroupedSearchResponse>>,
  isIdentityCurrent: () => boolean = () => true,
) {
  const searchLibrary = vi.fn(impl)
  return {
    client: {
      searchLibrary,
      isIdentityCurrent: vi.fn(isIdentityCurrent),
    } as unknown as ReaderClient,
    searchLibrary,
  }
}

const corpus = [
  makeLink({ id: 'c1', title: '本地命中关键词', url: 'https://a.com/x' }),
  makeLink({ id: 'c2', title: '无关', url: 'https://b.com/y' }),
]

describe('localFilterLinks', () => {
  it('按标题命中', () => {
    expect(localFilterLinks(corpus, '关键词').map((l) => l.id)).toEqual(['c1'])
  })
  it('空串返回空', () => {
    expect(localFilterLinks(corpus, '  ')).toEqual([])
  })
})

describe('useCommandSearch：防抖 + 降级', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('未到防抖点用本地预览，到点后用后端结果替换', async () => {
    const backendHit = makeLink({ id: 'srv', title: '后端语义结果' })
    const { client, searchLibrary } = fakeClient(async () => grouped([backendHit]))
    const { result, rerender } = renderHook(
      ({ q }) => useCommandSearch(client, q, corpus),
      { initialProps: { q: '关键词' } },
    )
    // 立即：本地预览，后端未调用。
    expect(result.current.results.map((l) => l.id)).toEqual(['c1'])
    expect(searchLibrary).not.toHaveBeenCalled()

    // 推进 300ms + 刷新挂起的 promise/微任务 → 触发后端并应用结果。
    await act(async () => { await vi.advanceTimersByTimeAsync(300) })
    expect(searchLibrary).toHaveBeenCalledWith('关键词', 50, 10, 20)
    rerender({ q: '关键词' })
    expect(result.current.results.map((l) => l.id)).toEqual(['srv'])
    expect(result.current.degraded).toBe(false)
  })

  it('# 前缀不调用后端链接搜索', async () => {
    const { client, searchLibrary } = fakeClient(async () => grouped())
    renderHook(() => useCommandSearch(client, '#tag', corpus))
    await vi.advanceTimersByTimeAsync(400)
    expect(searchLibrary).not.toHaveBeenCalled()
  })

  it('后端不支持 q=（kind=other）→ 降级本地过滤并标记 degraded', async () => {
    const { client } = fakeClient(async () => ({
      ok: false as const,
      error: { kind: 'other' as const, message: 'HTTP 404', status: 404 },
    }))
    const { result, rerender } = renderHook(() => useCommandSearch(client, '关键词', corpus))
    await act(async () => { await vi.advanceTimersByTimeAsync(300) })
    rerender()
    expect(result.current.degraded).toBe(true)
    // 降级后仍展示本地结果。
    expect(result.current.results.map((l) => l.id)).toEqual(['c1'])
  })

  it('后端结果保留网站分组', async () => {
    const { client } = fakeClient(async () => ({ ok: true, data: { reading: { items: [], total_hint: 0 }, sites: { total_hint: 1, items: [{ id: 'site-1', name: 'Example', matched_entries: [{ id: 'entry-1', name: 'Docs', url: 'https://example.com/docs' }] }] } } }))
    const { result } = renderHook(() => useCommandSearch(client, 'docs', corpus))
    await act(async () => { await vi.advanceTimersByTimeAsync(300) })
    expect(result.current.sites).toEqual([{ id: 'site-1', name: 'Example', matched_entries: [{ id: 'entry-1', name: 'Docs', url: 'https://example.com/docs' }] }])
  })

  it('后端结果保留已发布想法和笔记分组', async () => {
    const { client } = fakeClient(async () => ({
      ok: true,
      data: {
        reading: { items: [], total_hint: 0 },
        sites: { items: [], total_hint: 0 },
        thoughts: {
          total_hint: 1,
          items: [{ id: 'thought-1', host_kind: 'link', host_id: 'link-1', link_id: 'link-1', snippet: '公开想法', updated_at: '2026-08-09T00:00:00Z' }],
        },
        notes: {
          total_hint: 1,
          items: [{ id: 'note-1', title: '公开笔记', snippet: '已发布内容', published_revision: 2, updated_at: '2026-08-09T00:00:00Z' }],
        },
      },
    }))
    const { result } = renderHook(() => useCommandSearch(client, '公开', corpus))
    await act(async () => { await vi.advanceTimersByTimeAsync(300) })
    expect(result.current.thoughts[0]?.snippet).toBe('公开想法')
    expect(result.current.notes[0]?.title).toBe('公开笔记')
  })

  it('以 ID 合并后续想法页，并保留完整 total hint', async () => {
    const first: ApiResult<GroupedSearchResponse> = {
      ok: true,
      data: {
        reading: { items: [], total_hint: 0 },
        sites: { items: [], total_hint: 0 },
        thoughts: {
          total_hint: 21,
          next_cursor: 'opaque-page-2',
          items: [{ id: 'thought-1', host_kind: 'link', host_id: 'link-1', snippet: '第一页想法', updated_at: '2026-08-10T00:00:00Z' }],
        },
      },
    }
    const second: ApiResult<GroupedSearchResponse> = {
      ok: true,
      data: {
        reading: { items: [], total_hint: 0 },
        sites: { items: [], total_hint: 0 },
        thoughts: {
          total_hint: 21,
          items: [
            { id: 'thought-1', host_kind: 'link', host_id: 'link-1', snippet: '重复边界想法', updated_at: '2026-08-10T00:00:00Z' },
            { id: 'thought-2', host_kind: 'link', host_id: 'link-2', snippet: '第二页想法', updated_at: '2026-08-09T00:00:00Z' },
          ],
        },
      },
    }
    const searchLibrary = vi.fn()
    searchLibrary.mockResolvedValueOnce(first).mockResolvedValueOnce(second)
    const client = {
      searchLibrary,
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result } = renderHook(() => useCommandSearch(client, '分页', corpus))

    await act(async () => { await vi.advanceTimersByTimeAsync(300) })
    expect(result.current.thoughts.map((thought) => thought.id)).toEqual(['thought-1'])
    expect(result.current.thoughtTotalHint).toBe(21)
    expect(result.current.hasMoreThoughts).toBe(true)

    await act(async () => { await result.current.loadMoreThoughts() })
    expect(searchLibrary).toHaveBeenNthCalledWith(1, '分页', 50, 10, 20)
    expect(searchLibrary).toHaveBeenNthCalledWith(2, '分页', 50, 10, 20, 'opaque-page-2')
    expect(result.current.thoughts.map((thought) => thought.id)).toEqual(['thought-1', 'thought-2'])
    expect(result.current.thoughtTotalHint).toBe(21)
    expect(result.current.hasMoreThoughts).toBe(false)
  })

  it('旧 client 缺少 grouped search 时只降级到本地链接，不伪装完整 union', async () => {
    const client = {
      isIdentityCurrent: vi.fn(() => true),
    } as unknown as ReaderClient
    const { result } = renderHook(() => useCommandSearch(client, '关键词', corpus))

    expect(result.current.results.map((item) => item.id)).toEqual(['c1'])
    await act(async () => { await vi.advanceTimersByTimeAsync(300) })

    expect(result.current.degraded).toBe(true)
    expect(result.current.sites).toEqual([])
    expect(result.current.thoughts).toEqual([])
    expect(result.current.notes).toEqual([])
  })

  it('does not apply an A-era search response after the identity lease is replaced', async () => {
    const leaseA = readerIdentity.activeLease!
    const ownershipA = leaseA.capture('command search test')
    let resolveSearch!: (value: ApiResult<GroupedSearchResponse>) => void
    const delayed = new Promise<ApiResult<GroupedSearchResponse>>((resolve) => {
      resolveSearch = resolve
    })
    const backendHit = makeLink({ id: 'A-late', title: 'A 的迟到结果' })
    const { client } = fakeClient(
      () => delayed,
      () => leaseA.isCurrent(ownershipA),
    )
    const { result } = renderHook(() => useCommandSearch(client, '关键词', corpus))
    await act(async () => {
      await vi.advanceTimersByTimeAsync(300)
    })

    readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    await act(async () => {
      resolveSearch(grouped([backendHit]))
      await delayed
    })

    expect(result.current.results.map((item) => item.id)).toEqual(['c1'])
  })
})
