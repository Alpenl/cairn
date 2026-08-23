/**
 * 已保存原文读侧的守卫。
 *
 * 这里测的是**真实实现**，不是组件测试里那种自己写的回调闭包——上一轮的变异
 * 测试正是栽在这上面：把渲染期 peek 废掉、或者让读写用不同的键，组件测试照样
 * 全绿，因为它们各自被另一条路径掩护着。下面每条用例只允许一个机制生效。
 */
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { renderHook } from '@testing-library/react'

import { ok, type ApiResult } from '@webtag/api'
import type { LinkContentResponse } from '../api/types'
import { readerIdentity } from '../identity'
import { contentCacheKey, invalidateLinkContent } from './invalidate'
import { loadLinkContent, useCachedLinkContent } from './link-content'
import { resourceStore } from './store'

/**
 * 组件侧真正用的读法：订阅那个键，读它当下的值。
 *
 * 读完立刻 unmount——不卸载的话订阅会活到用例结束，后续对同一个键的写入会通知一个
 * 已经没人看的组件，跑出一堆 act() 警告。
 */
function readCached(linkId: string, revision: number | undefined): LinkContentResponse | null {
  const view = renderHook(() => useCachedLinkContent(linkId, revision))
  const value = view.result.current
  view.unmount()
  return value
}

const BODY: LinkContentResponse = {
  link_id: 'L1',
  content: '正文',
  content_document: '# 正文',
  content_format: 'markdown',
  fetcher_type: 'stored',
  content_source: 'fetched',
  content_revision: 7,
}

function clientWith(getContent = vi.fn(async () => ok(BODY))) {
  const captureIdentity = vi.fn((logicalKey: string) => {
    const lease = readerIdentity.activeLease
    if (!lease) return null
    const ownership = lease.captureOwnership(logicalKey)
    return lease.isOwnershipCurrent(ownership) ? ownership : null
  })
  return { client: { captureIdentity, getContent }, captureIdentity, getContent }
}

beforeEach(() => {
  resourceStore.clear()
})

describe('已保存原文的读侧', () => {
  it('未命中时回源，并把结果留在缓存里', async () => {
    const { client, getContent } = clientWith()

    expect(readCached('L1', 7)).toBeNull()
    const res = await loadLinkContent(client, 'L1', 7)

    expect(res.ok && res.data.content).toBe('正文')
    expect(getContent).toHaveBeenCalledTimes(1)
    expect(readCached('L1', 7)?.content).toBe('正文')
  })

  // MUT-C 守卫：删掉 loadLinkContent 里的 peek 短路（退回 store.fetch 的 SWR
  // 回源）这条就会红。它是独立的一道——渲染期 peek 落空时全靠它。
  it('命中缓存时不回源：第二次 load 一次网络都不发', async () => {
    const { client, getContent } = clientWith()

    await loadLinkContent(client, 'L1', 7)
    await loadLinkContent(client, 'L1', 7)
    await loadLinkContent(client, 'L1', 7)

    expect(getContent).toHaveBeenCalledTimes(1)
  })

  // 守卫：订阅侧与取数侧必须算出同一个键。任何一侧把 revision 算歪（反查落空
  // 退化成 rev=0、或者干脆漏传），这条就会红——那正是用户报的那个 bug。
  it('订阅与取数用同一个键：load 写进去的，同样的 revision 就能读到', async () => {
    const { client } = clientWith()

    await loadLinkContent(client, 'L1', 7)

    expect(readCached('L1', 7)?.content).toBe('正文')
    // 换一个 revision 就是另一条缓存——正文变了必须重新下载，不能张冠李戴。
    expect(readCached('L1', 8)).toBeNull()
    expect(readCached('L1', undefined)).toBeNull()
  })

  it('回源失败时不写缓存，也不把失败当成正文', async () => {
    const getContent = vi.fn(async () => ({
      ok: false as const,
      error: { kind: 'other' as const, message: 'boom' },
    }))
    const { client } = clientWith(getContent)

    const res = await loadLinkContent(client, 'L1', 7)

    expect(res.ok).toBe(false)
    expect(readCached('L1', 7)).toBeNull()
  })

  it('并发展开合并成一次往返', async () => {
    const { client, getContent } = clientWith()

    await Promise.all([
      loadLinkContent(client, 'L1', 7),
      loadLinkContent(client, 'L1', 7),
      loadLinkContent(client, 'L1', 7),
    ])

    expect(getContent).toHaveBeenCalledTimes(1)
  })

  it('写进去的键就是 contentCacheKey 算出来的那个（落盘白名单据此匹配前缀）', async () => {
    const { client } = clientWith()

    await loadLinkContent(client, 'L1', 7)

    expect(resourceStore.has(contentCacheKey('L1', 7))).toBe(true)
  })

  it('只按服务端响应的实际 revision 落键', async () => {
    const response = { ...BODY, content_revision: 8, content: 'revision eight' }
    const { client } = clientWith(vi.fn(async () => ok(response)))

    await expect(loadLinkContent(client, 'L1', 7)).resolves.toEqual(ok(response))

    expect(readCached('L1', 7)).toBeNull()
    expect(readCached('L1', 8)?.content).toBe('revision eight')
  })

  it('同一 active identity 下的不同客户端共享同键在途请求', async () => {
    let resolveA!: (result: ApiResult<LinkContentResponse>) => void
    const responseA = { ...BODY, content: 'client A' }
    const clientA = clientWith(
      vi.fn<() => Promise<ApiResult<LinkContentResponse>>>(() =>
        new Promise((resolve) => { resolveA = resolve }),
      ),
    )
    const clientB = clientWith(vi.fn(async () => ok({ ...BODY, content: 'client B' })))

    const requestA = loadLinkContent(clientA.client, 'L1', 7)
    const requestB = loadLinkContent(clientB.client, 'L1', 7)

    expect(clientA.getContent).toHaveBeenCalledTimes(1)
    expect(clientB.getContent).not.toHaveBeenCalled()
    resolveA(ok(responseA))

    const [resultA, resultB] = await Promise.all([requestA, requestB])
    expect(resultA).toEqual(ok(responseA))
    expect(resultB).toEqual(ok(responseA))
    expect(readCached('L1', 7)?.content).toBe('client A')
  })

  it('请求途中失效后，迟到的高 revision 响应不会回填任何正文键', async () => {
    let resolve!: (result: ApiResult<LinkContentResponse>) => void
    const response = { ...BODY, content_revision: 8, content: 'stale revision eight' }
    const { client, getContent } = clientWith(
      vi.fn<() => Promise<ApiResult<LinkContentResponse>>>(() =>
        new Promise((release) => { resolve = release }),
      ),
    )

    const request = loadLinkContent(client, 'L1', 7)
    expect(getContent).toHaveBeenCalledTimes(1)

    invalidateLinkContent('L1')
    resolve(ok(response))

    await expect(request).resolves.toEqual(ok(response))
    expect(resourceStore.has(contentCacheKey('L1', 7))).toBe(false)
    expect(resourceStore.has(contentCacheKey('L1', 8))).toBe(false)
    expect(readCached('L1', 7)).toBeNull()
    expect(readCached('L1', 8)).toBeNull()
  })

  it('身份缺失时在缓存读、回源和缓存写之前失败', async () => {
    resourceStore.set(contentCacheKey('L1', 7), BODY)
    const peek = vi.spyOn(resourceStore, 'peek')
    const getContent = vi.fn(async () => ok(BODY))
    const client = {
      captureIdentity: vi.fn(() => null),
      getContent,
    }

    const result = await loadLinkContent(client, 'L1', 7)

    expect(result).toMatchObject({ ok: false, error: { kind: 'identity-mismatch' } })
    expect(peek).not.toHaveBeenCalled()
    expect(getContent).not.toHaveBeenCalled()
  })

  it('过期 ownership 在缓存读、回源和缓存写之前失败', async () => {
    const leaseA = readerIdentity.activeLease
    if (!leaseA) throw new Error('test identity missing')
    const staleOwnership = leaseA.captureOwnership('stale content request')
    const leaseB = readerIdentity.install({
      serverClientDataNamespace: leaseA.context.serverClientDataNamespace,
      physicalNamespace: leaseA.context.physicalNamespace,
    })
    resourceStore.activateIdentity(leaseB)
    resourceStore.set(contentCacheKey('L1', 7), BODY)
    const peek = vi.spyOn(resourceStore, 'peek')
    const getContent = vi.fn(async () => ok(BODY))
    const client = {
      captureIdentity: vi.fn(() => staleOwnership),
      getContent,
    }

    const result = await loadLinkContent(client, 'L1', 7)

    expect(result).toMatchObject({ ok: false, error: { kind: 'identity-mismatch' } })
    expect(peek).not.toHaveBeenCalled()
    expect(getContent).not.toHaveBeenCalled()
  })

  it('同一客户端跨 namespace 的在途请求不合并，旧身份也不能覆盖新缓存', async () => {
    let resolveA!: (result: ApiResult<LinkContentResponse>) => void
    let resolveB!: (result: ApiResult<LinkContentResponse>) => void
    const responseA = { ...BODY, content: 'namespace A' }
    const responseB = { ...BODY, content: 'namespace B' }
    const getContent = vi
      .fn<() => Promise<ApiResult<LinkContentResponse>>>()
      .mockImplementationOnce(() => new Promise((resolve) => { resolveA = resolve }))
      .mockImplementationOnce(() => new Promise((resolve) => { resolveB = resolve }))
    const { client } = clientWith(getContent)

    const requestA = loadLinkContent(client, 'L1', 7)
    const leaseB = readerIdentity.install({
      serverClientDataNamespace: 'server-B',
      physicalNamespace: 'physical-B',
    })
    resourceStore.activateIdentity(leaseB)
    const requestB = loadLinkContent(client, 'L1', 7)

    expect(getContent).toHaveBeenCalledTimes(2)
    resolveB(ok(responseB))
    await expect(requestB).resolves.toEqual(ok(responseB))
    resolveA(ok(responseA))
    await expect(requestA).resolves.toMatchObject({
      ok: false,
      error: { kind: 'identity-mismatch' },
    })
    expect(readCached('L1', 7)?.content).toBe('namespace B')
  })
})
