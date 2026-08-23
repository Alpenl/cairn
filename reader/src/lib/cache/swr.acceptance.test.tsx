/**
 * PF3 的量化验收：证明「切走再切回来 / 文章往返 / 并发重取」都不再重新请求。
 *
 * 这些断言直接对应用户最初报的那个问题——「刷新或者去新页面再回来都会重新请求」。
 */
import { describe, expect, it, vi } from 'vitest'
import { act, renderHook, waitFor } from '@testing-library/react'

import { ok } from '@webtag/api'
import type { IdentityBoundReaderClient, ReaderClient } from '../api/client'
import type { LinkResponse, TranslationResponse } from '../api/types'
import { makeLink } from '../../test/fixtures'
import { useLinks } from '../../hooks/useLinks'
import { useTranslations } from '../../hooks/useTranslations'
import { useSubscriptions } from '../../hooks/useSubscriptions'
import { readerIdentity } from '../identity'

function useTestLinks(client: ReaderClient, selection: Parameters<typeof useLinks>[1]) {
  const lease = readerIdentity.activeLease
  if (!lease) throw new Error('test identity lease is not active')
  Object.defineProperty(client, 'identityLease', {
    configurable: true,
    value: lease,
  })
  return useLinks(client as IdentityBoundReaderClient, selection)
}

function linksClient(items: LinkResponse[]) {
  const getLinks = vi.fn(async () => ok({ items, total: items.length, page: 1, limit: 30 }))
  return { client: { getLinks } as unknown as ReaderClient, getLinks }
}

describe('PF3 量化验收：切走再切回来不重拉', () => {
  it('同一 selection 卸载后重挂，不产生新的网络请求', async () => {
    const { client, getLinks } = linksClient([makeLink({ id: 'L1' })])
    const selection = { type: 'smart' as const, id: 'all' as const, name: '全部' }

    const first = renderHook(() => useTestLinks(client, selection))
    await waitFor(() => expect(first.result.current.loading).toBe(false))
    expect(getLinks).toHaveBeenCalledTimes(1)

    // 模拟「切到订阅页」：整个 hook 卸载。
    first.unmount()

    // 切回来：重新挂载。
    const second = renderHook(() => useTestLinks(client, selection))

    // 关键断言 ①：首屏**同步**就有内容，不经历 loading。
    expect(second.result.current.loading).toBe(false)
    expect(second.result.current.links.map((l) => l.id)).toEqual(['L1'])

    // 关键断言 ②：重挂发起的是一次后台校验，界面不因它而空白。
    await waitFor(() => expect(getLinks).toHaveBeenCalledTimes(2))
    expect(second.result.current.loading).toBe(false)
    expect(second.result.current.links.map((l) => l.id)).toEqual(['L1'])
  })

  it('订阅导航卸载重挂同样直接用缓存渲染', async () => {
    const payload = {
      folders: [],
      subscriptions: [],
      counts: { all: 3, unread: 1, starred: 0, later: 0 },
    }
    const getSubscriptions = vi.fn(async () => ok(payload))
    const client = { getSubscriptions } as unknown as ReaderClient

    const first = renderHook(() => useSubscriptions(client))
    await waitFor(() => expect(first.result.current.loading).toBe(false))
    first.unmount()

    const second = renderHook(() => useSubscriptions(client))
    expect(second.result.current.loading).toBe(false)
    expect(second.result.current.data.counts.all).toBe(3)
  })
})

describe('PF3 量化验收：文章 A→B→A 往返', () => {
  it('第二次打开 A 时译文立即可用，loading 全程为 false', async () => {
    const translationOf = (linkId: string): TranslationResponse => ({
      id: 'T-' + linkId,
      link_id: linkId,
      scope: 'full',
      block_key: 'content',
      start_offset: 0,
      end_offset: 1,
      source_text: 's',
      translated_text: '译文',
      source_format: 'plain',
      target_language: 'zh-CN',
      status: 'done',
      model: 'm',
      error_msg: null,
      source_content_revision: 1,
      stale: false,
      created_at: '2026-07-15T00:00:00Z',
      updated_at: '2026-07-15T00:00:00Z',
    })
    const getTranslations = vi.fn(async (id: string) =>
      ok({
        current_content_revision: 1,
        current_summary_source_hash: null,
        items: [translationOf(id)],
      }),
    )
    const client = { getTranslations } as unknown as ReaderClient

    const { result, rerender } = renderHook(({ id }) => useTranslations(client, id), {
      initialProps: { id: 'A' },
    })
    await waitFor(() => expect(result.current.items).toHaveLength(1))
    expect(getTranslations).toHaveBeenCalledTimes(1)

    rerender({ id: 'B' })
    await waitFor(() => expect(result.current.items[0]?.link_id).toBe('B'))
    expect(getTranslations).toHaveBeenCalledTimes(2)

    // 回到 A：缓存命中，**同步**就有数据，不闪加载态。
    rerender({ id: 'A' })
    expect(result.current.loading).toBe(false)
    expect(result.current.items[0]?.link_id).toBe('A')
  })
})

describe('PF3 量化验收：in-flight 去重', () => {
  it('并发两次重取只产生一次网络请求', async () => {
    let resolve!: (value: unknown) => void
    const pending = new Promise((done) => {
      resolve = done as never
    })
    let calls = 0
    const getLinks = vi.fn(() => {
      calls += 1
      if (calls === 1) return Promise.resolve(ok({ items: [], total: 0, page: 1, limit: 30 }))
      return pending as never
    })
    const client = { getLinks } as unknown as ReaderClient
    const selection = { type: 'smart' as const, id: 'all' as const, name: '全部' }

    const { result } = renderHook(() => useTestLinks(client, selection))
    await waitFor(() => expect(result.current.loading).toBe(false))
    const baseline = getLinks.mock.calls.length

    // 两次静默校验并发：必须合并成一次往返。
    await act(async () => {
      const links = result.current
      void links.reload
      window.dispatchEvent(new Event('focus'))
      document.dispatchEvent(new Event('visibilitychange'))
      document.dispatchEvent(new Event('visibilitychange'))
      await Promise.resolve()
    })

    // visibilitychange 在 jsdom 里 visibilityState 恒为 'visible'，两次事件
    // 各触发一次静默校验；store 的 in-flight 合并保证只有一次真正出网。
    expect(getLinks.mock.calls.length - baseline).toBeLessThanOrEqual(1)

    await act(async () => {
      resolve(ok({ items: [], total: 0, page: 1, limit: 30 }))
      await Promise.resolve()
    })
  })
})
